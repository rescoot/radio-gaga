package config

import (
	"os"
	"path/filepath"
	"testing"

	"radio-gaga/internal/models"
)

const minimalConfigYAML = `
scooter:
  identifier: TESTVIN
  token: testtoken
redis_url: redis://localhost:6379
mqtt:
  broker_url: ssl://example.test:8883
  keepalive: 30s
`

func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "radio-gaga.yml")
	if err := os.WriteFile(path, []byte(minimalConfigYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig_StateDirDerivesBufferPaths(t *testing.T) {
	stateDir := t.TempDir()
	configPath := writeMinimalConfig(t)

	flags := &models.CommandLineFlags{ConfigPath: configPath, StateDir: stateDir}
	cfg, _, err := LoadConfig(flags)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	wantTelemetry := filepath.Join(stateDir, "telemetry-buffer.json")
	wantEvents := filepath.Join(stateDir, "events-buffer.json")

	if cfg.Telemetry.Buffer.PersistPath != wantTelemetry {
		t.Errorf("telemetry persist path = %q, want %q", cfg.Telemetry.Buffer.PersistPath, wantTelemetry)
	}
	if cfg.Events.BufferPath != wantEvents {
		t.Errorf("events buffer path = %q, want %q", cfg.Events.BufferPath, wantEvents)
	}

	// MkdirAll should have ensured the dir exists.
	if info, err := os.Stat(stateDir); err != nil || !info.IsDir() {
		t.Errorf("state dir %s missing or not a directory: %v", stateDir, err)
	}
}

func TestLoadConfig_NoStateDirKeepsDefaults(t *testing.T) {
	configPath := writeMinimalConfig(t)
	flags := &models.CommandLineFlags{ConfigPath: configPath}
	cfg, _, err := LoadConfig(flags)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// With no -state-dir and no path set in config, telemetry persist path should remain empty
	// (radio-gaga treats empty as "no disk persistence") and events buffer should fall back
	// to the legacy default in ValidateConfig.
	if cfg.Telemetry.Buffer.PersistPath != "" {
		t.Errorf("telemetry persist path = %q, want empty", cfg.Telemetry.Buffer.PersistPath)
	}
	if cfg.Events.BufferPath != "/var/lib/radio-gaga/events-buffer.json" {
		t.Errorf("events buffer path = %q, want legacy default", cfg.Events.BufferPath)
	}
}

func TestValidatePriorityOrdering_ValidOrder(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected no error for valid priority ordering, got: %v", err)
	}
}

func TestValidatePriorityOrdering_EqualDurations(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "1s",
				Medium:    "1s",
				Slow:      "1s",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected no error for equal durations, got: %v", err)
	}
}

func TestValidatePriorityOrdering_ImmediateGreaterThanQuick(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "10s",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when immediate > quick, got nil")
	}
}

func TestValidatePriorityOrdering_QuickGreaterThanMedium(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "2m",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when quick > medium, got nil")
	}
}

func TestValidatePriorityOrdering_MediumGreaterThanSlow(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "5s",
				Medium:    "1h",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when medium > slow, got nil")
	}
}

func TestValidatePriorityOrdering_InvalidDuration(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "invalid",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	// When durations are invalid, we return nil and let the general duration validation catch it
	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected nil for invalid duration (handled elsewhere), got: %v", err)
	}
}
