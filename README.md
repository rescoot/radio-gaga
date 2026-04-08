# Radio Gaga

[![Build and Release](https://github.com/rescoot/radio-gaga/actions/workflows/release.yml/badge.svg)](https://github.com/rescoot/radio-gaga/actions/workflows/release.yml)

```bash
make dist   # optimized ARM build for deployment
```

Telemetry and remote control bridge for electric scooters (unu Scooter Pro / LibreScoot). Sits between the vehicle's Redis state bus and the Sunshine cloud platform via MQTT. Adapts its reporting frequency to what the scooter is actually doing: once per second while driving, once a day while hibernating.

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
| Driving | 1s | Real-time tracking |
| Standby (with battery) | 5m | Periodic check-ins |
| Standby (no battery) | 8h | Conserve cellular data |
| Hibernate | 24h | Minimal keepalive |

State changes trigger an immediate telemetry push regardless of the interval timer.

### Priority-Based Flushing

Not all telemetry fields change at the same rate. When a field changes between regular intervals, radio-gaga schedules an early flush based on the field's priority:

| Priority | Default Deadline | Fields |
|----------|-----------------|--------|
| Immediate | 1s | Vehicle state, lock status, blinkers |
| Quick | 5s | GPS, battery charge level |
| Medium | 1m | Most other fields |
| Slow | 15m | Aux battery, CBB, BLE status |

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
| `navigate` | Set destination coordinates for DBC navigation |
| `hibernate` | Force hibernate mode |

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

Runtime config changes via `config:get`, `config:set`, `config:del`, `config:save`. Changes from `config:set` take effect immediately; call `config:save` to persist them to disk. Fields use dot notation (`telemetry.intervals.driving`). Save creates a backup of the previous config file.

### Location Sync

`locations:merge` receives saved locations from the server and merges them into Redis with 25m deduplication. Used for the saved-locations feature on LibreScoot DBC dashboards.

### Development-Only

Available when `environment: development`:

| Command | Description |
|---------|-------------|
| `shell` | Execute shell commands with streaming output |
| `redis` | Execute Redis operations (get, set, hget, hset, hgetall, lpush, lpop, publish) |

These are disabled in production.

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

1. Download binary from provided URL
2. Verify checksum (SHA-256 or SHA-1)
3. Remount filesystem read-write if needed
4. Back up the current binary
5. Replace and restart the systemd service
6. Automatic rollback if the new binary fails to start

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
-driving-interval string    Telemetry interval while driving (default: 1s)
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

The repository includes an installer script (`install.sh`) that handles setup on unu Scooter Pro hardware: validates the environment, fetches scooter-specific config from the Sunshine API, downloads the binary, creates a systemd service, and starts it.

Target platform is Linux ARM (ARMv7). The binary runs as a systemd service (`rescoot-radio-gaga.service` on stock, `radio-gaga` on LibreScoot).

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
- Environment-based command restrictions (shell/redis disabled in production)
- Checksum verification for OTA updates with automatic rollback
- Read-only filesystem handling during updates
- Retained MQTT message cleanup to prevent stale command replay

## License

[GNU Affero General Public License 3.0](LICENSE)

## Authors

- Teal Bauer <teal@rescoot.org>
