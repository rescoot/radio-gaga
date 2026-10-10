# Live ride groups

Ride-group support is opt-in (`ride_group.enabled: false` by default). Enabled clients advertise `ride_group_v2` on their authenticated device's dedicated MQTT topics. Version 1 schemas remain available for compatibility and conformance tests; a version 2 registration never silently downgrades.

The adapter exchanges transient JSON on `ride-group:activity`, `ride-group:control` and `ride-group:peers`. Location data is not added to telemetry, retained MQTT messages, Redis hashes, files or logs. Publication needs a current selected cloud grant, fresh genuine dashboard activity, an unblocked reconciled dashboard journal and a new coherent producer GPS fix. Background telemetry intervals and GPS acquisition are independent.

The dashboard owns the durable stop record. This adapter has no stop journal and never invents operation IDs or chooses a consent fence. It starts blocked until a fresh local-state probe is answered. A recovered uncertain intent can only query its original receipt. Prepared recovery queries the saved fence; only a second fresh correlated probe with that same operation/revision/fence can authorize an apply after `not_found`. Confirmation does not re-enable sharing.

GPS loss immediately invalidates staged coordinates and reports `no_fix` using the same sequence space as positions. Recovery requires a genuinely newer fix and a server-issued new stream; the recovery measurement is not republished as another fix. Producer timestamps remain independent of stream and lease resets. Motion comes from consecutive coherent positions, not retained speed/course fields.

MQTT and local IPC have bounded queues. Authorization is checked at their final invocation boundaries; data already admitted to a client library or socket cannot be recalled. Peer terminal watermarks survive receiver lease loss for the current roster. Every frame is limited to 1024 bytes, with at most five peers.

Run `GOTOOLCHAIN=go1.25.13 go test -count=1 ./...` and `GOTOOLCHAIN=go1.25.13 go test -race -count=1 ./...`. The tests include 60 version 2 and 24 unchanged version 1 fixtures, canonical Unicode/envelope vectors, recovery/replay checks and local Mosquitto delivery.
