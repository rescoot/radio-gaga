# Transactional config changes and MQTT cert rotation

Status: draft, awaiting review
Date: 2026-04-27
Scope: radio-gaga (Go client) and sunshine (Rails server / mosquitto MQTT)

## Why

The MQTT trust chain between radio-gaga and sunshine needs to be rotated. The
old self-signed CA's *private key* is gone, so we cannot mint new server certs
that chain to it. The existing CA cert PEM is still on the running mosquitto
host and embedded in every deployed scooter; the old server cert + key are also
still on sunshine. Some deployed scooters may never receive a firmware update
and must remain reachable indefinitely. Scooter clocks are unreliable (no
buffered RTC, revert to 2006/2016 without NTP), so cert validity dates cannot
be enforced as a control surface.

A second, more general problem: the mechanism for rotating the broker URL or
CA bundle on a deployed scooter has no rollback path. If radio-gaga is told
"connect to host X with cert Y" and the new combination doesn't work, the unit
is stuck retrying a broken config forever. self_update has the same shape but
only verifies that systemd is happy with the new binary — not that the binary
can actually reach the cloud.

This spec covers two pieces, in dependency order:

1. **Transactional config changes** — a small, generic commit-or-rollback
   primitive in radio-gaga for any change that could brick connectivity.
2. **MQTT cert rotation** — using that primitive to roll out a new CA + a new
   mosquitto listener side-by-side with the existing one, with zero
   abandonment of un-updated units.

## Non-goals

- Replacing the existing self_update binary-swap mechanics. It already has
  local rollback. Retrofitting it to use the transactional layer comes
  *after* this spec lands and is not in scope here.
- Adding mTLS / client certificates on the MQTT side. mosquitto today runs
  with `require_certificate false`; that stays.
- Removing the existing `InsecureSkipVerify` fallback in client.go. It is
  reachable today on cert-validity errors and stays as a last-ditch path.
  A clock-tolerant `VerifyPeerCertificate` is desirable but is a separate
  follow-up, not part of this spec.

## Constraints recap

| Constraint | Implication |
|---|---|
| Old CA private key lost | Cannot sign anything new with old CA. New CA must be a brand-new self-signed root. |
| Old CA cert PEM + old server cert + key still present | Existing chain can keep serving forever. No abandonment required. |
| Scooter clocks unreliable | Cert expiry is not enforceable. New CA gets absurd validity (100yr). Rollback decisions must not depend on wall-clock time. |
| Some scooters never updatable | Cannot do a flag-day cutover. Old listener stays up indefinitely. |
| Scooter writable persistence | `/data/radio-gaga/` on librescoot, similar on stock scooterOS. Derived from `-config` flag — no platform branching needed. |

## Part 1: Transactional config changes

### Pattern

A "fatal-class" change is one that could leave radio-gaga unable to reach the
cloud. CA bundle replacement, broker URL change, and (later) self-update all
qualify. Every fatal-class change runs as a transaction with this shape:

```
                  ┌── snapshot LKG (last-known-good) ───────────────────────┐
                  ▼                                                          │
  apply in-mem → local probe ──fail─→ revert in-mem (no disk change, done)  │
       │                                                                     │
       ▼                                                                     │
  publish txn_started{txn_id, kind} QoS 1 on auth-gated per-scooter topic   │
       │                                                                     │
       │  PUBACK from broker  → confirms TLS + auth + ACL on new config     │
       ▼                                                                     │
  commit (atomic rename staging → live, delete LKG)                          │
                                                                             │
  on PUBACK timeout / connect failure / publish error                        │
       └──────────────► rollback (restore LKG, reconnect on old) ───────────┘
```

**Confirmation depth: passive only.** Broker PUBACK is sufficient. PUBACK on a
per-scooter ACL-gated topic proves TLS handshake + mosquitto auth + ACL all
work on the new config. Anything app-side beyond that is recoverable by
re-sending commands; the broker layer is the part where misconfiguration
strands a unit. No sunshine protocol changes required.

**Local probe is the actual MQTT connect.** We do not maintain two parallel
connections. The probe is: tear down the current MQTT client, build a new one
from the staged config, `Connect()`, subscribe to the command topic, publish
`txn_started` at QoS 1, wait for PUBACK with timeout T. Success is the commit
trigger. Failure (connect timeout, TLS error, publish error, PUBACK timeout)
is the rollback trigger.

**Crash safety.** State on disk:

```
/data/radio-gaga/                    (or wherever -config points)
├── config.yaml                      live config (last committed)
├── config.yaml.staging              candidate; only present mid-txn
├── config.yaml.lkg                  last-known-good snapshot; only present mid-txn
└── txn/
    └── pending.json                 {txn_id, kind, started_at_monotonic, deadline}
```

On startup, if `txn/pending.json` exists:
- if `config.yaml.lkg` exists → restore it over `config.yaml`, delete staging,
  delete pending.json. Log a crash-rollback event. Continue boot on LKG.
- if `config.yaml.lkg` is missing → impossible state (should not happen);
  log loudly, delete pending.json, continue boot on whatever `config.yaml`
  contains.

This mirrors systemd-boot's `boot_count` A/B pattern, applied to config files.

### Library shape (Go)

A small package `internal/txn` exposes:

```go
type Txn struct {
    ID        string
    Kind      string  // "set_broker_url", "update_ca_cert", ...
    Deadline  time.Duration
}

type Apply func(staged *models.Config) error    // mutate staged config in place
type Probe func(ctx context.Context, staged *models.Config) error  // returns nil on PUBACK

type Manager interface {
    Run(ctx context.Context, t Txn, apply Apply, probe Probe) (committed bool, err error)
    RecoverOnBoot() error  // checks for pending.json and rolls back if present
}
```

`Run` is the single entry point. It:
1. Loads current config as LKG, writes `config.yaml.lkg`.
2. Clones live config, runs `apply` on the clone, writes `config.yaml.staging`,
   writes `txn/pending.json`.
3. Tells the MQTT client to disconnect.
4. Mutates the live in-memory config to the staged value.
5. Calls `probe` — which drives a fresh MQTT connect + the `txn_started`
   publish + PUBACK wait, against the new config.
6. On success: atomic rename `config.yaml.staging` → `config.yaml`, delete
   `config.yaml.lkg`, delete `txn/pending.json`. Returns `(true, nil)`.
7. On failure: restore `config.yaml.lkg` → in-memory config, reconnect MQTT
   on LKG, delete staging files and pending.json. Returns `(false, err)`.

`RecoverOnBoot` runs once during `client.NewScooterMQTTClient` initialization,
before any MQTT connect attempt, to handle the crash case.

### Per-scooter txn topic

`scooters/{identifier}/txn`. Per-scooter ACL is already in place for the
existing command/response topics under `scooters/{identifier}/`; the new
topic is added to the same ACL grant, so the publish exercises the real
auth path. Sunshine may consume these messages for fleet-rollout dashboards
but is not required to act on them — broker PUBACK alone is the gating
signal for radio-gaga.

### Commands that use the framework

This spec adds two:

#### `set_broker_url`

New command. Params: `{ "url": "ssl://broker.example:8884" }`. Wraps a txn
that mutates `config.MQTT.BrokerURL`. Local apply rewrites the in-memory URL
and triggers a reconnect via the txn manager. Probe is the standard publish +
PUBACK on the new connection. Idempotent: if the new URL equals the live URL,
no-op.

#### `update_ca_cert` (extended)

Existing command, semantics extended:
- Parameter `cert` accepts a PEM bundle (one or more `BEGIN CERTIFICATE` blocks).
- Each block is parsed and validated as a CA cert (`IsCA = true`).
- The resulting bundle replaces `CACertEmbedded` *as a transaction*.
- The current "replace and reconnect" path is removed; transactional path
  takes over.

This unlocks the "trust both old + new CAs" intermediate state needed for
the cert rotation in Part 2.

`x509.CertPool.AppendCertsFromPEM` already handles concatenated PEMs, so the
runtime trust pool side needs no changes. Only the validator in
`HandleUpdateCACertCommand` needs to walk all blocks instead of just the first.

### Timeouts

- T (PUBACK wait) = 30 seconds. Long enough to ride out a brief network
  hiccup, short enough that a stuck transaction recovers in a sane window.
  Worst case the server re-sends the command. Configurable via
  `config.Transactions.ProbeTimeout`.

### Failure-mode coverage

The implementation must demonstrably handle all of these. They become
test cases in the harness:

| Failure | Expected outcome |
|---|---|
| Probe: TLS handshake fails (cert mismatch) | rollback to LKG, error reported |
| Probe: TCP connect refused / DNS NXDOMAIN | rollback |
| Probe: connect succeeds, publish times out (no PUBACK) | rollback |
| Probe: connect succeeds, broker auth rejects | rollback |
| Probe: connect succeeds, ACL rejects publish | rollback |
| Process killed mid-probe (before commit) | next boot: rollback from LKG, log event |
| Process killed mid-commit (rename in flight) | safe — atomic rename or stays at LKG |
| Power loss between commit and pending.json delete | next boot: pending.json present but LKG missing → log loudly, accept current config |
| Old config restored mid-rollback fails to connect | radio-gaga keeps retrying; not worse than today |
| Two transactions overlap (server sends a second mid-flight) | second is rejected with `txn_in_progress` until first resolves |

## Part 2: MQTT cert rotation

### Approach

Run two TLS listeners on mosquitto in parallel, indefinitely:

- **:8883 (legacy)** — keeps the existing `cafile` / `certfile` / `keyfile`
  configuration. Old scooters that only ever trust `oldCA` keep connecting
  here forever.
- **:8884 (new)** — new self-signed CA with 100-year validity, new server
  cert chained to it, both keys stored properly this time. Updated scooters
  connect here.

Both listeners share the same auth plugin and ACL files. From sunshine's
point of view they are interchangeable.

### Per-scooter rollout flow

For each scooter that can be reached:

1. Server sends `update_ca_cert` with a PEM bundle of `oldCA + newCA`. The
   scooter trusts both. Connection stays on :8883 with old chain. PUBACK
   confirms; transaction commits. (Idempotent: if the bundle is already
   `oldCA + newCA`, no-op.)
2. Server sends `set_broker_url` with `ssl://broker:8884`. The scooter
   tears down its :8883 connection, connects to :8884, presents nothing
   (no client cert), validates the new server cert against its bundle
   (newCA chains successfully), publishes `txn_started`, waits for PUBACK.
   On success the new URL is committed.

If step 2 fails for any reason, step 1 already succeeded and the scooter is
back on :8883 with the dual-trust bundle. Re-trying step 2 later is safe.

We do **not** ever push a bundle that drops `oldCA`. Carrying the legacy
trust anchor forever costs ~1 KB and keeps rollback open in case we ever
need to point a unit back at :8883.

### Sunshine-side changes

- mosquitto config gains a second `listener 8884` block referencing the new
  cert files. Both listeners share `password_file` / `acl_file` / dynamic
  security plugin via `per_listener_settings false`. Verified working in a
  staging compose before any prod deploy.
- `config/mqtt_certs/` adds `new/ca.crt`, `new/server/server.crt`,
  `new/server/server.key`. Old paths untouched.
- A small admin UI / rake task that, given a scooter ID, sends the two
  rollout commands in sequence (idempotent, so safe to retry).
- A telemetry/metrics view: scooters connected per listener, scooters still
  on `oldCA`-only bundle, last successful rollout per scooter. Drives the
  long-tail visibility.

### Rollout phases

1. **Bring up :8884 in staging.** Verify mosquitto serves both listeners,
   auth + ACL identical. Walk the deep-blue test matrix end to end.
2. **First fielded scooter.** Pick a known-good unit beyond deep-blue and
   run the rollout flow against it. Watch txn events from the server side.
3. **Tranche rollout.** Per-region or per-batch, with the dashboard
   tracking long-tail.
4. **Long tail forever.** Scooters that don't update stay on :8883. The
   listener stays up. Cost is negligible.

### What we are *not* doing

- Not running mTLS.
- Not removing the legacy listener on any timeline. The plan is "indefinite",
  re-evaluated when the legacy listener has zero connected clients for a
  long stretch.
- Not changing the existing `InsecureSkipVerify` fallback path (out of scope).
- Not rotating any other secrets in this work (passwords, API tokens, etc.).

## Testing

No docker-compose harness. The transactional layer is tested against
**deep-blue** (a real scooter on librescoot) with a staging sunshine
configured for the new dual-listener setup. This is more honest than a
container harness because the failure modes that matter — filesystem
persistence on the real partition layout, systemd restart timing, real
modem network conditions, NTP behavior, the `/data/radio-gaga/` write path
— are all exercised on the actual target.

What gets unit-tested in Go (cheap, runs in CI):

- `internal/txn` pure state-machine logic: pending.json round-trip, atomic
  rename, boot-time `RecoverOnBoot` decision tree (LKG present / LKG
  missing / staging only / clean state). No MQTT involved.
- Extended `update_ca_cert` validator: PEM bundle parsing, multi-cert
  validation, rejection of non-CA certs in any block, single-block
  backward compat. Already has test scaffolding in
  `update_ca_cert_test.go`.

Everything else is exercised on deep-blue. The matrix:

| What we run on deep-blue | What we verify |
|---|---|
| Current radio-gaga + new dual-listener mosquitto on staging | Old chain on :8883 still works (no change in behavior). |
| Pre-this-work radio-gaga build + new dual-listener mosquitto | Older binaries (no txn support) still connect on :8883 — proves un-updated fleet won't break when we add :8884. |
| New radio-gaga + `update_ca_cert` with `oldCA+newCA` bundle | Trust pool grows, reconnect on :8883 still works, txn commits. |
| Same + `set_broker_url` to :8884 | Disconnects from :8883, connects to :8884, PUBACK observed, txn commits. |
| Forced failure: `set_broker_url` to non-existent host | Rollback to LKG, scooter back on :8883 within probe timeout. |
| Forced failure: `update_ca_cert` with garbage PEM | Local validation rejects, no txn started. |
| Forced failure: kill radio-gaga mid-probe (`systemctl kill`) | On restart, `RecoverOnBoot` restores LKG, scooter reconnects on old config. |
| Forced failure: power-cycle deep-blue mid-probe | Same as above, plus exercises real flush/sync behavior on the data partition. |
| Forced failure: push `set_broker_url` to a host that does TLS but rejects auth | PUBACK never arrives, rollback fires. |
| Forced failure: push `set_broker_url` to a host that does TLS + auth but ACL-rejects the txn topic | Same as above. |

deep-blue gets pinned to specific radio-gaga binary builds via the existing
`/data/radio-gaga-<timestamp>` pattern + symlink swap (already how versions
are managed there). Older versions are kept around to exercise the
"un-updatable scooter" path against the new sunshine setup.

Prod tranche rollout is gated on every row of that matrix passing.

## Files affected

### radio-gaga

New:
- `internal/txn/manager.go` — Manager, Run, RecoverOnBoot
- `internal/txn/state.go` — disk state read/write, atomic rename
- `internal/handlers/commands/set_broker_url.go` — new command
- `internal/txn/state_test.go` — pure state-machine unit tests (no MQTT)

Modified:
- `internal/handlers/commands/update_ca_cert.go` — accept PEM bundle, use txn
- `internal/handlers/command_handlers.go` — register `set_broker_url`
- `internal/client/client.go` — call `txn.RecoverOnBoot` early in init,
  wire reconnect-via-txn instead of `RequestReconnect` for fatal-class
  changes
- `internal/models/types.go` — `Config.Transactions.ProbeTimeout`,
  txn-related types

### sunshine

New:
- `config/mqtt_certs/new/...` — new CA, server cert, server key
- Admin UI / rake task for staged rollout
- Telemetry view for per-listener / per-CA fleet state

Modified:
- `config/mosquitto/mosquitto.conf` — add `listener 8884` block
- `config/mosquitto/mosquitto_dev.conf` — same, for parity in dev
- `config/deploy.yml` — mount new cert files

## Open items

- Choice of new CA generation tooling (cfssl vs openssl vs step-ca). Not
  load-bearing on the design; pick during implementation.
- Exact sunshine schema for the rollout tracker (transactions table vs
  reusing existing scooter-state table). Punt to implementation plan.
