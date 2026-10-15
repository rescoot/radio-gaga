# Installation-bound MQTT bootstrap

`-bootstrap-capabilities` advertises `"installation_binding":1` alongside
`"enrollment_protocol":1`. Installation binding is selected by a nonempty
`bootstrap.installation_id`. It requires matching server routing and broker ACLs.

## Configuration

The installer generates a unique, nonzero lowercase UUID for each installation
attempt and durably records it before installing. The same UUID goes in the
scooter's YAML and the laptop's upload report. Retries and restarts preserve it;
an explicitly requested replacement installation gets a new UUID.

```yaml
scooter:
  name: My scooter
bootstrap:
  code: '<installer-code>'
  installation_id: '797ad894-3fb7-41bc-af68-e534a92b2546'
mqtt:
  broker_url: ssl://mqtt.example.org:8883
  require_verified_tls: true
redis_url: redis://127.0.0.1:6379
```

Use exactly one of `bootstrap.code` (six-character installer code) or
`bootstrap.token` (canonical base64url-encoded 256-bit enrollment token).
Credentials authenticate the MQTT connection; neither appears in announcements
or topic names. The broker username is the existing credential digest prefix.
TLS certificate, hostname and clock verification are mandatory.

Radio-gaga reads the modem/SIM without requiring an active cellular subscription.
It waits for a readable identity before announcing an installation-bound setup.
It retries announcements every 60 seconds while connected, re-subscribes and
announces on broker reconnect, and exits on initial connection failure so the
service manager can retry. The service must be enabled with `Restart=always` and
a restart delay. A powered-off or sleeping vehicle cannot connect until awake.

## Topics and messages

For broker username `U` and installation UUID `I`:

| Direction | Topic |
| --- | --- |
| Device announces | `claim/U/announce` |
| Device receives configuration | `claim/U/installations/I/config` |
| Device reports transaction result | `claim/U/installations/I/result` |

The device subscribes only to its installation's configuration topic, never the
shared `claim/U/config` topic in this mode. The server computes the reply topic
from the authenticated credential and validated installation UUID, not an
arbitrary topic supplied by a device.

Announcement (QoS 1, not retained):

```json
{
  "installation_id": "797ad894-3fb7-41bc-af68-e534a92b2546",
  "nonce": "8653aaccf2d850d1",
  "name": "My scooter",
  "imei": "123456789012345",
  "iccid": "8949000000000000001",
  "imsi": "901280000000001",
  "mdb_serial": "example-mdb",
  "dbc_serial": "example-dbc",
  "os_version": "example-version",
  "platform": "librescoot"
}
```

Empty optional fields are omitted. The installation UUID persists on disk. The
nonce is generated for each process and remains unchanged across MQTT reconnects.
Name is a display label, not an identity or authorization check.

Configuration delivery uses the existing transactional replacement envelope,
with two mandatory binding fields in `params`:

```json
{
  "command": "txn:replace",
  "params": {
    "txn_id": "example-transaction",
    "kind": "config",
    "installation_id": "797ad894-3fb7-41bc-af68-e534a92b2546",
    "nonce": "8653aaccf2d850d1",
    "config_yaml": "<complete normal-operation YAML>",
    "restart": true
  }
}
```

Both `installation_id` and `nonce` must exactly match the running device. Missing,
mismatched or malformed deliveries are ignored before reaching the configuration
transaction machinery. A retained response from an earlier process is ignored;
the server must send a response using the nonce from a fresh announcement.
There is no fallback to an unbound response.

Result (QoS 1, not retained):

```json
{
  "status": "success",
  "txn_id": "example-transaction",
  "installation_id": "797ad894-3fb7-41bc-af68-e534a92b2546",
  "nonce": "8653aaccf2d850d1"
}
```

Failed transactions report `status: "rollback"` and a `reason`; bootstrap keeps
waiting for a valid retry. Successful transactions commit the normal config and
exit so the service manager restarts into it. The normal config must contain the
individual scooter credentials and omit the bootstrap stanza.

## Server and installer requirements

- Persist the laptop's installation UUID, selected scooter/VIN and observed
  IMEI/ICCID/IMSI together. Upload retries must be idempotent. The laptop report
  and device announcement can arrive in either order.
- Match the announcement to that registration and authenticated account. Enforce
  ownership, explicit acceptance/replacement and hardware-identity checks before
  sending normal credentials. The UUID and nonce are correlation values, not
  secrets or proof of ownership.
- Refresh the process nonce only after validating the installation and hardware
  identity. Reissue pending deliveries with the current nonce after a restart.
- Validate result topic, installation UUID, nonce and transaction ID together.
  Clear retained configuration only for the matching completed installation.
- Scope broker ACLs to the credential's namespace: publish announcements and
  installation results; subscribe/receive installation configurations. Do not
  grant a device permission to publish configuration or access another account's
  namespace. Subscription and message-delivery ACLs both need the nested topics.
- A reusable credential is account-level authority. These bindings prevent
  accidental cross-installation delivery and stale-process application; they do
  not isolate hostile devices that share the same credential. Separate device
  credentials are needed for that trust boundary.

## Compatibility and rollout

An omitted installation UUID selects the unbound protocol with
`claim/U/config` and `claim/U/result`. Existing individual enrollment tokens and
single-device installer-code flows remain supported. Do not share an unbound
credential between simultaneous waiting installations.

A bound device does not consume a shared-topic reply. Installing a binary with
this capability does not itself change existing configurations. The installer
must require `installation_binding >= 1`, confirm server/broker support, and then
write the installation UUID. If support is missing, bound mode keeps waiting;
it does not accept an unbound configuration.

## Local verification

```sh
go test ./...
go test -race ./cmd/radio-gaga ./internal/config ./internal/models
make dist
```

With `mosquitto` on `PATH`, tests run a private loopback TLS broker and exercise
the production bootstrap session with simulated hardware reads and configuration
application: same-token/same-name scooters, crossed or missing bindings, rollback
and retry, process restart with a retained stale response, and broker reconnect.
These broker tests skip when Mosquitto is unavailable; release CI installs it.
The transaction implementation and TLS verification have separate test suites.
