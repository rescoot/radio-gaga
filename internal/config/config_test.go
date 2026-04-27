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

func TestLoadConfig_NoStateDirAutoDetects(t *testing.T) {
	// Override the candidate list so the test doesn't depend on host filesystem.
	autoDir := filepath.Join(t.TempDir(), "auto")
	prev := stateDirCandidates
	stateDirCandidates = []string{autoDir}
	t.Cleanup(func() { stateDirCandidates = prev })

	configPath := writeMinimalConfig(t)
	flags := &models.CommandLineFlags{ConfigPath: configPath}
	cfg, _, err := LoadConfig(flags)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.StateDir != autoDir {
		t.Errorf("cfg.StateDir = %q, want %q", cfg.StateDir, autoDir)
	}
	wantTelemetry := filepath.Join(autoDir, "telemetry-buffer.json")
	wantEvents := filepath.Join(autoDir, "events-buffer.json")
	if cfg.Telemetry.Buffer.PersistPath != wantTelemetry {
		t.Errorf("telemetry persist path = %q, want %q", cfg.Telemetry.Buffer.PersistPath, wantTelemetry)
	}
	if cfg.Events.BufferPath != wantEvents {
		t.Errorf("events buffer path = %q, want %q", cfg.Events.BufferPath, wantEvents)
	}
}

func TestLoadConfig_ConfigValuesWinOverAutoDetect(t *testing.T) {
	// When the YAML config sets explicit paths and the operator passes no flags,
	// the config values should be respected (auto-detect only fills empties).
	autoDir := filepath.Join(t.TempDir(), "auto")
	prev := stateDirCandidates
	stateDirCandidates = []string{autoDir}
	t.Cleanup(func() { stateDirCandidates = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "radio-gaga.yml")
	yaml := minimalConfigYAML + `
telemetry:
  buffer:
    persist_path: /custom/telemetry.json
events:
  buffer_path: /custom/events.json
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	flags := &models.CommandLineFlags{ConfigPath: path}
	cfg, _, err := LoadConfig(flags)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Telemetry.Buffer.PersistPath != "/custom/telemetry.json" {
		t.Errorf("telemetry persist path = %q, want /custom/telemetry.json", cfg.Telemetry.Buffer.PersistPath)
	}
	if cfg.Events.BufferPath != "/custom/events.json" {
		t.Errorf("events buffer path = %q, want /custom/events.json", cfg.Events.BufferPath)
	}
}

func TestLoadConfig_StateDirFlagOverridesConfigValues(t *testing.T) {
	// -state-dir is recent operator intent and overrides whatever the config says.
	stateDir := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "radio-gaga.yml")
	yaml := minimalConfigYAML + `
telemetry:
  buffer:
    persist_path: /old/telemetry.json
events:
  buffer_path: /old/events.json
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	flags := &models.CommandLineFlags{ConfigPath: path, StateDir: stateDir}
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
