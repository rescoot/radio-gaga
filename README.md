# Radio Gaga

[![Build and Release](https://github.com/rescoot/radio-gaga/actions/workflows/release.yml/badge.svg)](https://github.com/rescoot/radio-gaga/actions/workflows/release.yml)

```bash
make dist   # optimized ARM build for deployment
```

Telemetry and remote control bridge for electric scooters (unu Scooter Pro / Librescoot). Sits between the vehicle's Redis state bus and the Sunshine cloud platform via MQTT. Adapts its reporting frequency to what the scooter is actually doing: once per second while driving, once a day while hibernating.

Part of the [Rescoot](https://github.com/rescoot) project.

## How It Works

Radio Gaga reads vehicle state from Redis hashes populated by other on-board services (vehicle-service, battery-service, modem-service, etc.) and publishes structured telemetry to MQTT. It also listens for commands from the cloud and executes them locally.

```
Redis (vehicle state) --> radio-gaga --> MQTT --> Sunshine cloud
                              ^
                              |
                         MQTT commands
```

### MQTT Topics

| Topic | Direction | Purpose |
|-------|-----------|---------|
| `scooters/{id}/telemetry` | out | Periodic telemetry data |
| `scooters/{id}/commands` | in | Incoming commands (JSON) |
| `scooters/{id}/acks` | out | Command responses |
| `scooters/{id}/events` | out | Detected events (alarm, movement, etc.) |
| `scooters/{id}/telemetry_batch` | out | Buffered telemetry batches (retransmitted after reconnect) |

### Adaptive Telemetry

Reporting intervals adjust automatically based on vehicle state:

| State | Default Interval | Rationale |
|-------|-----------------|-----------|
| Driving | 30s | Movement tracking without flooding the broker |
| Standby (with battery) | 5m | Periodic check-ins |
| Standby (no battery) | 8h | Conserve cellular data |
| Hibernate | 24h | Minimal keepalive |

State changes are flushed promptly via the priority deadlines below, regardless of the interval timer.

### Priority-Based Flushing

Not all telemetry fields change at the same rate. When a field changes between regular intervals, radio-gaga schedules an early flush based on the field's priority:

| Priority | Default Deadline | Fields |
|----------|-----------------|--------|
| Immediate | 10s | Vehicle state, lock status, blinkers |
| Quick | 30s | GPS, battery charge level |
| Medium | 5m | Most other fields |
| Slow | 1h | Aux battery, CBB, BLE status |

Deadlines must stay ordered `immediate <= quick <= medium <= slow`. Lower values are more responsive but cost more traffic.

### Telemetry Buffering

When MQTT is unavailable (tunnel, dead spot, broker maintenance), telemetry events are buffered to Redis with disk fallback. On reconnect, buffered events are retransmitted in batches with their original timestamps recalibrated for clock drift. The buffer also flushes to disk before power suspension to avoid data loss.

### Event Detection

Radio Gaga monitors telemetry for noteworthy state changes and publishes them as discrete events. Detected event types:

- **Alarm**: arm/disarm/trigger
- **Unauthorized movement**: speed while parked
- **Battery warning**: charge level thresholds
- **Temperature warning**: motor/battery overtemp
- **State change**: vehicle state transitions
- **Connectivity**: internet/cloud status changes
- **Fault**: error conditions

Events are buffered to disk and retried independently of telemetry.

## Commands

Commands arrive as JSON on the MQTT commands topic:

```json
{
  "command": "lock",
  "params": {},
  "request_id": "abc-123"
}
```

### Vehicle Control

| Command | Description |
|---------|-------------|
| `lock` | Lock the scooter |
| `unlock` | Unlock the scooter |
| `open_seatbox` | Open the seat box |
| `blinkers` | Control turn signals (left/right/both/off) |
| `honk` | Sound the horn (configurable duration) |
| `locate` | Flash lights and honk in a pattern |
| `alarm` | Trigger alarm system (hazard lights, horn patterns) |
| `navigate` | Set one destination (`latitude`, `longitude`, optional `address`); omit both coordinates to clear navigation |
| `navigate:route` | Set an ordered route (`waypoints` array, 1–25 stops); unsupported clients reject this command without changing the current target |
| `hibernate` | Force hibernate mode |

`navigate:route` accepts stops with `latitude`/`longitude` (or `lat`/`lon`) and optional `label` (or `name`):

```json
{"command":"navigate:route","params":{"waypoints":[{"latitude":52.51,"longitude":13.41,"label":"Work"},{"latitude":52.52,"longitude":13.42,"label":"Home"}]},"request_id":"abc-123"}
```

The first stop is the current navigation target. The route command requires `nav=2` in the locally advertised `system[capabilities]` registry; an absent or older registry is rejected without changing the destination. `navigate:route` calls settings-service `plan.replace` on `settings:route-plan` with the parsed stops. A single `navigate` replaces the plan with one stop; omitting both coordinates calls `plan.clear`. The cloud command receives an error if the RPC is unavailable or rejects the request. Only settings-service writes the `navigation` projection.

### System

| Command | Description |
|---------|-------------|
| `ping` | Health check |
| `get_state` | Return full current telemetry |
| `self_update` | OTA update with checksum verification and rollback |
| `update_ca_cert` | Replace MQTT CA certificate (validates PEM, checks CA flag) |
| `fetch_logs` | Collect and upload system logs to Sunshine |

### Keycard Management

| Command | Description |
|---------|-------------|
| `keycards:list` | List authorized keycard UIDs |
| `keycards:add` | Authorize a keycard UID |
| `keycards:delete` | Remove a keycard UID |
| `keycards:master_key:get` | Get the current master keycard |
| `keycards:master_key:set` | Set the master keycard |

### Configuration

Runtime config changes via `config:get`, `config:set`, `config:del`, `config:save`. Changes from `config:set` take effect immediately except startup-only cellular settings; call `config:save` to persist them to disk. Fields use dot notation (`telemetry.intervals.driving`). Save creates a backup of the previous config file.

### Cellular APN

An optional `cellular` block configures both the data connection and LTE attachment:

```yaml
cellular:
  apn: internet.example
  username: ""
  password: ""
  auth: none # none, pap, or chap
  connection: wwan # stock NetworkManager profile name
```

Settings are applied at startup, with retries while the modem or platform services are unavailable. Omit the block to leave cellular configuration untouched. On stock systems, radio-gaga updates NetworkManager, ModemManager's initial EPS bearer, and the SIM7100E's CID 1 attach APN/authentication, then re-registers and activates the data connection. This interrupts cellular connectivity. An explicitly empty APN uses the SIM's default attach APN.

On Librescoot, radio-gaga fills missing or empty `cellular.apn`, `cellular.username`, `cellular.password`, and `cellular.auth` fields in LS's `settings` hash and notifies the settings watchers. Existing nonempty values take precedence. The LS modem service owns modem configuration; radio-gaga does not run modem or NetworkManager commands on Librescoot. To replace an existing LS setting, edit that setting directly.

Cellular configuration changes require saving the config and restarting radio-gaga. Protect the configuration file because it contains credentials. On stock systems, credentials are also briefly visible in command arguments. Quotes, commas, NUL, and line breaks in APN or credentials are rejected because of the AT/mmcli property syntax.

### Location Sync

`locations:merge` receives saved locations from the server and merges them into Redis with 25m deduplication. Used for the saved-locations feature on Librescoot DBC dashboards.

### Development-Only

Available when `environment: development`:

| Command | Description |
|---------|-------------|
| `shell` | Execute shell commands with streaming output |
| `redis` | Execute Redis operations (get, set, hget, hset, hgetall, lpush, lpop, publish) |

`shell` is disabled outside the development environment. `redis` is available in all environments but hard-limited to the typed operations listed above (anything else is rejected); the backend relies on it for alarm control, admin tooling, and the API.

## Notifications

### Telegram

Direct alerts to a Telegram chat when events fire. Create a bot via [@BotFather](https://t.me/BotFather), get your chat ID from the `/getUpdates` endpoint.

```yaml
scooter:
  name: Deep Blue           # used in notification messages

telegram:
  enabled: true
  bot_token: "123456789:ABCdef..."
  chat_id: "123456789"
  rate_limit: 1s
  queue_size: 100
  daily_limit: 0            # 0 = unlimited
  events:
    alarm: true
    unauthorized_movement: true
    battery_warning: true
    temperature_warning: true
    state_change: false
    connectivity: false
    fault: false
```

### Notification Rules

For more complex alerting, define rules with conditions evaluated against Redis state. Multiple conditions per rule use AND logic. Rules can route to Telegram, SMS, or both.

```yaml
notifications:
  rules:
    - name: "cbb_low"
      conditions:
        - source: "cb-battery"
          field: "charge"
          operator: "<"
          value: "50"
      channels: [telegram]
      cooldown: "30m"
      message: "CB battery at {{cb-battery.charge}}%"

    - name: "alarm_while_parked"
      conditions:
        - source: "alarm"
          field: "status"
          operator: "=="
          value: "triggered"
        - source: "vehicle"
          field: "state"
          operator: "=="
          value: "standby"
      channels: [telegram, sms]
```

Supported condition operators: `<`, `>`, `<=`, `>=`, `==`, `!=`. Raw pub/sub message matching is also supported with `==` (exact) or `contains` (substring).

### SMS

SMS notifications go through ModemManager (`mmcli`), so they work directly over the scooter's cellular modem. No external SMS gateway needed.

```yaml
notifications:
  sms:
    enabled: true
    phone_number: "+491234567890"
    rate_limit: "30s"
    daily_limit: 20
    queue_size: 20
```

## Self-Update

OTA updates via the `self_update` command:

1. Download raw or `application/zstd` bytes into a persistent, resumable cache.
2. Verify the decoded binary's checksum (SHA-256 or SHA-1).
3. Stage and run that exact candidate in probe mode using the current config. The probe must connect to MQTT and subscribe successfully.
4. Only after probe success, atomically install the candidate and schedule a service restart. A rejected candidate leaves the installed binary and config untouched.

The download has a 2-hour overall budget, not a five-minute transfer cutoff. A 90-second no-progress watchdog resets on every body read, so slow transfers can continue while making progress. Connection, TLS handshake and response-header waits are each limited to 30 seconds. Each attempt's deadline is the remaining overall budget; the probe has a separate 60-second deadline. Cancellation and budget exhaustion retain partial bytes.

There are at most six HTTP attempts. Transient network/body failures, HTTP 408/425/429 and 5xx except 501/505 use exponential backoff: ceilings of 5, 10, 20, 40 and 80 seconds, with random delays between half and all of each ceiling (a 2-minute cap). Retry-After seconds or HTTP dates take precedence up to five minutes if they fit the remaining budget. Longer Retry-After values end the attempt sequence rather than retrying early. Other HTTP failures, invalid compression, oversized payloads and checksum mismatches are not blindly re-downloaded.

The single cache slot lives under `<state-dir>/self-update` (or `.self-update` alongside the config when no state directory is supplied). Its identity includes the exact URL, checksum algorithm/value and representation negotiation. Partial raw and zstd wire bytes resume with `Range` and `If-Range` only when a strong ETag is available. A matching 206 must preserve ETag, final URL, representation and valid byte bounds. A 200 replaces partial bytes; invalid/changed ranges and unsatisfiable ranges restart safely. Servers without ranges or strong validators remain compatible, but interrupted transfers from them restart in full. Complete cached payloads are checksum-verified again before every probe, including after a process restart or failed probe/install.

The cache is limited to one 65 MiB wire payload and a 64 MiB decoded candidate; zstd decoder memory is limited to 64 MiB. A different download identity replaces the slot, and entries older than seven days are discarded on the next self-update. Only the latest 64 command results are kept, also expiring after seven days. Repeated request IDs replay the original success or failure without another download/install; reuse with different parameters is rejected. To deliberately retry a failed command, send a new request ID. Commands without an ID are deduplicated by download identity. Process interruption before a completed result lets redelivery resume the partial transfer.

Self-updates use one dedicated worker, so a stalled transfer does not block vehicle commands or change their ordering. Concurrent deliveries of the same request share its eventual response; other update requests receive a busy error. While an update is active, `txn:replace`, `config:set`, `config:del`, `config:save`, `update_ca_cert` and `migrate_broker` also receive busy errors to keep the candidate's binary/config transaction isolated. MQTT delivery acknowledgements are separate from application success, which is sent only after probe and installation succeed. A reconnect keeps the same update job and sends its result through the current connection. Shutdown cancels and joins the worker; interrupted commands can resume on redelivery.

Optional local configuration (not command payload parameters):

```yaml
commands:
  self_update:
    params:
      download_budget: 2h   # 1s to 24h, includes retries and waits
      stall_timeout: 90s   # 1s to 30m, no minimum throughput
```

## unu-uplink Integration

For scooters still running stock unu firmware: radio-gaga can reconfigure the legacy unu-uplink service to point at the Sunshine broker instead of the defunct unu servers. Handles CA certificate setup and service restart. Only touches the config if it still points to unu infrastructure.

```yaml
unu_uplink:
  enabled: true
```

## Configuration

Configuration priority: command-line flags > YAML file > Redis fallback values.

See [`radio-gaga.example.yml`](radio-gaga.example.yml) for a complete annotated example.

### Command-Line Flags

```
-version              Print version and exit
-config string        Path to config file (default: radio-gaga.yml)
-identifier string    Vehicle identifier (MQTT username)
-token string         Authentication token (MQTT password)
-mqtt-broker string   MQTT broker URL
-mqtt-cacert string   Path to MQTT CA certificate
-mqtt-keepalive string  MQTT keepalive duration (default: 30s)
-redis-url string     Redis URL (default: redis://localhost:6379)
-environment string   production or development
-debug                Enable debug logging
-ntp-enabled          Enable NTP sync (default: true)
-ntp-server string    NTP server (default: pool.ntp.rescoot.org)
-driving-interval string    Telemetry interval while driving (default: 30s)
-standby-interval string    Telemetry interval in standby (default: 5m)
-standby-no-battery-interval string  Without battery (default: 8h)
-hibernate-interval string  In hibernate (default: 24h)
-buffer-enabled       Enable telemetry buffering
-buffer-max-size int  Maximum buffered events (default: 1000)
-buffer-max-retries int  Maximum send retries (default: 5)
-buffer-retry-interval string  Retry interval (default: 1m)
-buffer-persist-path string  Disk persistence path (empty = no persistence)
-transmit-period string  Buffer transmission period (default: 5m)
-api-base-url string  Sunshine API base URL
-api-scooter-id string  Sunshine API scooter ID
```

### Redis Schema

Radio Gaga reads from these Redis hashes (populated by other on-board services):

| Key | Contents |
|-----|----------|
| `vehicle` | State, handlebar lock, kickstand, blinkers, brakes, seatbox |
| `battery:0`, `battery:1` | Main battery data (charge, voltage, current, temp, health) |
| `aux-battery` | Auxiliary battery level, voltage, charge status |
| `cb-battery` | Connectivity Battery Box (cycle count, capacity, serial, etc.) |
| `engine-ecu` | Speed, RPM, odometer, voltage, current, temperature |
| `gps` | Latitude, longitude, altitude, speed, course |
| `internet` | Internet/cloud connection status, signal quality, SIM info |
| `modem` | Modem power state, SIM state, operator, roaming |
| `power-manager` | Power state, wakeup source, hibernate level |
| `power-mux` | Power mux input source |
| `ble` | Bluetooth MAC address and status |
| `keycard` | Keycard reader authentication, UID, type |
| `dashboard` | Dashboard mode, ready state, serial number |
| `navigation` | Destination coordinates |
| `system` | MDB/DBC version and flavor, firmware, serial numbers |
| `settings` | Fallback config values (`cloud:mqtt-url`, `cloud:mqtt-ca`) |

### MDB Flavor Detection

Detected from the hostname prefix and stored in Redis as `system.mdb-flavor`:

- `librescoot-*` -> librescoot
- `mdb-*` -> stock
- anything else -> unknown

## Building

```bash
make            # development build (current platform)
make amd64      # Linux AMD64
make arm        # Linux ARM (ARMv7)
make arm-debug  # ARM with debug symbols
make dist       # optimized ARM build, stripped
make clean
```

Version is embedded at build time from `git describe`.

### CI/CD

GitHub Actions builds an ARM distribution on every push to `main` (and on manual dispatch). Pushing a version tag (`v1.0.0`) creates a GitHub Release with the packaged binary, example config, systemd unit, and CA certificate.

```bash
git tag v1.2.3
git push origin v1.2.3
```

## Installation

### From Source

```bash
git clone https://github.com/rescoot/radio-gaga.git
cd radio-gaga
make
cp radio-gaga.example.yml radio-gaga.yml
# edit radio-gaga.yml
./radio-gaga
```

### On a Scooter

Generate an installer link in [Sunshine Account settings](https://sunshine.rescoot.org/account/security#bootstrap-tokens), then copy its bootstrap token. Developer mode, a user API token and manual VIN selection are not required.

Run the installer as root on the scooter:

```bash
bash install.sh  # prompts for the bootstrap token
# Or supply the token explicitly:
BOOTSTRAP_TOKEN='<bootstrap-token>' bash install.sh
```

`install.sh` downloads Sunshine's shared installer over HTTPS. That installer detects the distro, installs the binary and `radio-gaga.service` where needed, and uses radio-gaga's bootstrap mode to identify the scooter and fetch its configuration. Accept any pending scooter claim in Sunshine to complete setup. `SUNSHINE_URL` selects a different HTTPS Sunshine instance.

For laptop enrollment, `-bootstrap-capabilities` reports
`enrollment_protocol: 1` and `installation_binding: 1` in its JSON output, and
`-bootstrap-identity` reads IMEI, ICCID and optional IMSI from the local modem.
The same report includes `installer_protocol: 1`, `bootstrap_announce: 1`,
`mqtt_username: 1`, `verified_tls: 1` and `offline_config: 1`. Installer protocol 1
supports bootstrap-only configurations, a separate MQTT username, mandatory
verified TLS, and offline configuration validation. Installers must check these
capabilities, including on a downloaded candidate, rather than infer them from a
version string.

`radio-gaga -config /path/to/config.yaml -validate-config` checks configuration
and TLS settings without contacting Redis/MQTT, creating state directories or
recovering a transaction. It prints `enrolled` or `announce`, and rejects invalid
configuration or missing `mqtt.require_verified_tls: true`. It never prints a
credential. This preflight does not perform a broker connectivity probe.

A managed installation writes `installer-protocol` beside the executable. For
protocol 1, self-update checks the candidate's same capability report before its
normal broker probe and transaction commit. A client missing these capabilities
cannot replace the managed executable. Self-update follows `os.Executable()`, so
a service running `/data/radio-gaga/radio-gaga` updates that persistent file,
not the image's `/usr/bin/radio-gaga`.

The laptop uses a
bootstrap credential, never its OAuth token. Setting `bootstrap.installation_id`
to a persistent UUID enables installation-specific configuration/result topics
and requires replies to match that installation and the current process nonce.
Without an installation ID, laptop enrollment uses an individual 256-bit
`bootstrap.token` for each installation. See the
[installation binding protocol](docs/bootstrap-installation-binding.md) for
configuration examples, server requirements and compatibility.
Bootstrap MQTT requires TLS with a valid certificate chain, hostname and clock;
plaintext brokers and malformed configured CAs are rejected. Config deliveries
and results carry a `txn_id`. Enrollment configs set `mqtt.require_verified_tls`
to prevent insecure certificate/time fallback after switching to normal operation.

You can also run the online installer command shown in Account settings, or paste the same token or online installer URL into LSD's Cloud page on Librescoot. If the scooter cannot reach Sunshine yet, use the separate offline claim command in Account settings; the scooter waits for its configuration after you accept the claim.

Target platform is Linux ARM (ARMv7). To test the installer wrapper without network or scooter access, run `bash tests/install_test.sh`.

## Dependencies

| Module | Purpose |
|--------|---------|
| `github.com/eclipse/paho.mqtt.golang` | MQTT client |
| `github.com/go-redis/redis/v8` | Redis client |
| `gopkg.in/yaml.v2` | YAML config parsing |
| `github.com/beevik/ntp` | NTP time synchronization |

Requires Go 1.24+. Runtime dependencies: Redis, an MQTT broker (Sunshine).

## Security

- TLS for MQTT with custom CA certificate support
- NTP sync on startup (required for TLS certificate validation)
- Per-vehicle authentication (identifier + token)
- Environment-based command restrictions (shell disabled in production; redis limited to a fixed whitelist of typed operations)
- Checksum verification for OTA updates with automatic rollback
- Read-only filesystem handling during updates
- Retained MQTT message cleanup to prevent stale command replay

## License

[GNU Affero General Public License 3.0](LICENSE)

## Authors

- Teal Bauer <teal@rescoot.org>
