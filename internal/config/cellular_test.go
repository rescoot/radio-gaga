package config

import (
	"os"
	"path/filepath"
	"testing"

	"radio-gaga/internal/models"
)

func TestLoadCellularConfig(t *testing.T) {
	withFakeStateDir(t)
	for _, tc := range []struct {
		name, stanza     string
		present, invalid bool
	}{
		{name: "omitted"},
		{name: "configured", stanza: "cellular:\n  apn: internet\n  username: user\n  password: secret\n  auth: pap\n", present: true},
		{name: "clear", stanza: "cellular: {}\n", present: true},
		{name: "invalid", stanza: "cellular:\n  auth: invalid\n", present: true, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(minimalConfigYAML+tc.stanza), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: path})
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted invalid auth")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (cfg.Cellular != nil) != tc.present {
				t.Fatal("incorrect cellular presence")
			}
			if tc.name == "configured" && (cfg.Cellular.APN != "internet" || cfg.Cellular.Auth != "pap" || cfg.Cellular.Password != "secret") {
				t.Fatal("lost cellular config")
			}
		})
	}
}

func TestSetCellularConfigField(t *testing.T) {
	cfg := &models.Config{}
	for field, value := range map[string]string{"apn": "internet", "username": "user", "password": "secret", "auth": "chap", "connection": "mobile"} {
		if err := SetConfigField(cfg, "cellular."+field, value); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.Cellular == nil || cfg.Cellular.APN != "internet" || cfg.Cellular.Password != "secret" || cfg.Cellular.Auth != "chap" {
		t.Fatal("incorrect config")
	}
	before := *cfg.Cellular
	for _, field := range []string{"auth", "password"} {
		if err := SetConfigField(cfg, "cellular."+field, "invalid\n"); err == nil {
			t.Fatal("accepted invalid setting")
		}
		if *cfg.Cellular != before {
			t.Fatal("invalid update mutated config")
		}
	}
	if err := SetConfigField(cfg, "cellular.apn", 123); err == nil {
		t.Fatal("accepted non-string")
	}
	if err := SetConfigField(cfg, "cellular.unknown", "x"); err == nil {
		t.Fatal("accepted unknown field")
	}
}
