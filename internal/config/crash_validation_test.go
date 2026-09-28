package config

import (
	"os"
	"strings"
	"testing"

	"radio-gaga/internal/models"
)

func TestValidateConfigRejectsNonPositiveTickerDurations(t *testing.T) {
	cases := []struct {
		name, yaml string
	}{
		{"driving", "telemetry:\n  intervals:\n    driving: 0s\n"},
		{"transmit", "telemetry:\n  transmit_period: -1s\n"},
		{"telegram", "telegram:\n  enabled: true\n  rate_limit: 0s\n"},
		{"sms", "notifications:\n  sms:\n    enabled: true\n    rate_limit: 0s\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := writeMinimalConfig(t)
			if err := os.WriteFile(path, []byte(minimalConfigYAML+tt.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: path})
			if err == nil || !strings.Contains(err.Error(), "positive duration") {
				t.Fatalf("LoadConfig error = %v, want positive duration validation", err)
			}
		})
	}
}
