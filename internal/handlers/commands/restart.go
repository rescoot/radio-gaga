package commands

import (
	"log"
	"os"
	"syscall"
	"time"
)

// Indirection points so tests can exercise HandleRestartCommand without killing
// the test runner.
var (
	restartGrace = 2 * time.Second
	restartFunc  = func() error { return syscall.Kill(os.Getpid(), syscall.SIGTERM) }
)

// HandleRestartCommand restarts the radio-gaga process by sending itself SIGTERM
// after a short grace (so the success response leaves the wire first); systemd
// (Restart=always) respawns it, reloading the on-disk config with a single fresh
// MQTT client. Unlike self_update / txn:replace it needs no binary download and
// runs no probe, so it is the cheapest way to apply a saved config or clear a
// bad in-memory state (e.g. a reconnect loop).
func HandleRestartCommand() error {
	go func() {
		time.Sleep(restartGrace)
		log.Println("restart: triggering process restart for systemd respawn")
		if err := restartFunc(); err != nil {
			log.Printf("restart: failed to trigger restart: %v", err)
		}
	}()
	return nil
}
