package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallerConfigPreflightDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	old := stateDirCandidates
	stateDirCandidates = []string{filepath.Join(dir, "state")}
	t.Cleanup(func() { stateDirCandidates = old })
	for _, tc := range []struct {
		name, content   string
		valid, announce bool
	}{
		{"enrolled", "scooter: {identifier: test, token: retained}\nmqtt: {broker_url: 'ssl://mqtt.example.test:8883', require_verified_tls: true}\n", true, false},
		{"announce", "bootstrap: {code: abc123}\nmqtt: {broker_url: 'ssl://mqtt.example.test:8883', require_verified_tls: true}\n", true, true},
		{"missing identity", "mqtt: {broker_url: 'ssl://mqtt.example.test:8883', require_verified_tls: true}\n", false, false},
		{"unverified", "scooter: {identifier: test, token: retained}\nmqtt: {broker_url: 'ssl://mqtt.example.test:8883'}\n", false, false},
		{"plaintext", "scooter: {identifier: test, token: retained}\nmqtt: {broker_url: 'tcp://mqtt.example.test:1883', require_verified_tls: true}\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := ReadInstallerConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if err == nil && cfg.InBootstrapState() != tc.announce {
				t.Fatal("wrong enrollment mode")
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatal("preflight created state")
			}
		})
	}
}
