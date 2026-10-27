package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstallerConfigCLIIsOffline(t *testing.T) {
	if path := os.Getenv("RADIO_GAGA_INSTALLER_CONFIG_TEST"); path != "" {
		flag.CommandLine = flag.NewFlagSet("radio-gaga", flag.ExitOnError)
		os.Args = []string{"radio-gaga", "-validate-config", "-config", path}
		main()
		os.Exit(0)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	pending := filepath.Join(dir, ".txn-pending.json")
	if err := os.WriteFile(pending, []byte("must remain untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ content, mode string }{
		{"scooter: {identifier: test, token: test}\nmqtt: {broker_url: 'ssl://unreachable.invalid:8883', require_verified_tls: true}\n", "enrolled\n"},
		{"bootstrap: {code: abc123}\nmqtt: {broker_url: 'ssl://unreachable.invalid:8883', require_verified_tls: true}\n", "announce\n"},
	} {
		if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestInstallerConfigCLIIsOffline$")
		cmd.Env = append(os.Environ(), "RADIO_GAGA_INSTALLER_CONFIG_TEST="+path)
		output, err := cmd.Output()
		if err != nil || string(output) != tc.mode {
			t.Fatalf("output=%q err=%v", output, err)
		}
		got, err := os.ReadFile(pending)
		if err != nil || string(got) != "must remain untouched" {
			t.Fatal("preflight ran transaction recovery")
		}
	}
}
