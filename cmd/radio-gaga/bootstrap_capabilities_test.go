package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"testing"
)

func TestBootstrapCapabilitiesCLI(t *testing.T) {
	if os.Getenv("RADIO_GAGA_CAPABILITIES_TEST") == "1" {
		flag.CommandLine = flag.NewFlagSet("radio-gaga", flag.ExitOnError)
		os.Args = []string{"radio-gaga", "-bootstrap-capabilities"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapCapabilitiesCLI$")
	cmd.Env = append(os.Environ(), "RADIO_GAGA_CAPABILITIES_TEST=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var capabilities map[string]int
	if err := json.Unmarshal(output, &capabilities); err != nil {
		t.Fatalf("stdout must contain only JSON: %q: %v", output, err)
	}
	for _, key := range []string{"enrollment_protocol", "installation_binding", "installer_protocol", "bootstrap_announce", "mqtt_username", "verified_tls", "offline_config"} {
		if capabilities[key] != 1 {
			t.Fatalf("missing capability %s: %v", key, capabilities)
		}
	}
}
