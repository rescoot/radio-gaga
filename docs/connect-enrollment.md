# Installed-client connect enrollment

Radio-gaga supports the provider-neutral `radio-gaga/1` provisioning profile through a one-shot local interface:

```sh
radio-gaga -connect-capabilities
radio-gaga -connect-enroll
```

The first command prints supported profiles. The second reads a JSON object from stdin containing `session_id`, `profile`, `provider_origin` and a short-lived `claim_token`. These secrets must not be passed through command-line arguments. The connect CLI supplies this request after a user has selected a provider and confirmed an account connection on the web.

The client exchanges the claim directly at the selected provider's `/api/v1/connect/exchange` endpoint. It supplies available modem IMEI and MDB/DBC real serials directly to that provider. The bouncer receives neither those identifiers nor the returned permanent connection credential.

The versioned response contains session/profile/transaction bindings, identifier, scooter ID, token, MQTT endpoint/username and CA certificate. Unknown fields are rejected. Radio-gaga owns the YAML rendering, local Redis target, service name and file layout. It requires verified MQTT TLS, renders production configuration with shell/Redis commands disabled, and derives the API origin from the authenticated provider selection. No provider-selected executable, path or arbitrary YAML is accepted.

Real enrollment is limited to an unconfigured Librescoot installation at `/data/radio-gaga/config.yaml`. Existing configurations, including invalid or unknown files, block enrollment. A private journal persists request identity and returned material before application. Retries preserve the same transaction and credential. A successful or recovered commit is recognized by its configuration digest; it is not reapplied.

Initial configuration uses `txn.Manager` with `ConfigInstallOnly` and mode 0600. The transaction probes the candidate using this same binary's existing `-probe` mode. Atomic no-replace linking prevents a concurrent writer's configuration from being overwritten. Recovery compares the staged/live inode before removing an uncommitted initial install, preserving unrelated files. Pending transactions belonging to another operation block enrollment.

Before real application the client checks stationary vehicle state and inactive OTA status. It never unlocks the vehicle. After committing it starts and verifies only `librescoot-radio-gaga.service`, then authenticates completion to the provider with the issued scooter credential. A lost completion response can be retried from the journal. This administrative CLI is not an automatic OTA/import or dashboard integration; physical rollout is separately authorized.

`-connect-development` permits loopback HTTP testing and skips vehicle/systemd integration. Only that mode permits a temporary `-config` target and `-connect-identity-file` with synthetic identity. The actual MQTT probe still requires verified TLS. The connect-service installed-client test drives real Sunshine web approval and a password-authenticated TLS test broker using these options.

Targeted checks:

```sh
go test ./cmd/radio-gaga ./internal/txn
go test -race ./cmd/radio-gaga ./internal/txn
go vet ./...
```
