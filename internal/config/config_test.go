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

// withFakeStateDir overrides the auto-detect candidate list to a deterministic
// temp directory and restores it on test cleanup.
func withFakeStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auto")
	prev := stateDirCandidates
	stateDirCandidates = []string{dir}
	t.Cleanup(func() { stateDirCandidates = prev })
	return dir
}

func TestLoadConfig_AutoDetectFillsBufferPaths(t *testing.T) {
	autoDir := withFakeStateDir(t)
	configPath := writeMinimalConfig(t)

	cfg, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.StateDir != autoDir {
		t.Errorf("cfg.StateDir = %q, want %q", cfg.StateDir, autoDir)
	}
	if got, want := cfg.Telemetry.Buffer.PersistPath, filepath.Join(autoDir, "telemetry-buffer.json"); got != want {
		t.Errorf("telemetry persist path = %q, want %q", got, want)
	}
	if got, want := cfg.Events.BufferPath, filepath.Join(autoDir, "events-buffer.json"); got != want {
		t.Errorf("events buffer path = %q, want %q", got, want)
	}
}

func TestLoadConfig_DeprecatedConfigPathsIgnored(t *testing.T) {
	// Stale configs in the field have hardcoded distro-specific paths. Auto-detect
	// must override them — operators can no longer pin paths via config.
	autoDir := withFakeStateDir(t)

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

	cfg, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: path})
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if got, want := cfg.Telemetry.Buffer.PersistPath, filepath.Join(autoDir, "telemetry-buffer.json"); got != want {
		t.Errorf("telemetry persist path = %q, want %q (config value should be ignored)", got, want)
	}
	if got, want := cfg.Events.BufferPath, filepath.Join(autoDir, "events-buffer.json"); got != want {
		t.Errorf("events buffer path = %q, want %q (config value should be ignored)", got, want)
	}
}

func TestLoadConfig_DeprecatedFlagsIgnored(t *testing.T) {
	// -state-dir and -buffer-persist-path are kept parseable for backward
	// compatibility with deployed systemd units, but their values are ignored.
	autoDir := withFakeStateDir(t)
	configPath := writeMinimalConfig(t)

	flags := &models.CommandLineFlags{
		ConfigPath:        configPath,
		StateDir:          "/should/be/ignored",
		BufferPersistPath: "/also/ignored.json",
	}
	cfg, _, err := LoadConfig(flags)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.StateDir != autoDir {
		t.Errorf("cfg.StateDir = %q, want %q (deprecated -state-dir should be ignored)", cfg.StateDir, autoDir)
	}
	if got, want := cfg.Telemetry.Buffer.PersistPath, filepath.Join(autoDir, "telemetry-buffer.json"); got != want {
		t.Errorf("telemetry persist path = %q, want %q (deprecated -buffer-persist-path should be ignored)", got, want)
	}
}

func TestSaveConfig_StripsDeprecatedPaths(t *testing.T) {
	autoDir := withFakeStateDir(t)

	cfg := &models.Config{
		Scooter:  models.ScooterConfig{Identifier: "VIN", Token: "tok"},
		MQTT:     models.MQTTConfig{BrokerURL: "ssl://x:8883", KeepAlive: "30s"},
		RedisURL: "redis://localhost:6379",
	}
	cfg.Telemetry.Buffer.PersistPath = filepath.Join(autoDir, "telemetry-buffer.json")
	cfg.Events.BufferPath = filepath.Join(autoDir, "events-buffer.json")

	dir := t.TempDir()
	out := filepath.Join(dir, "out.yml")
	if err := SaveConfig(cfg, out); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	got := string(data)
	if filepathContainsAny(got, "persist_path", "buffer_path") {
		t.Errorf("saved config still contains deprecated path keys:\n%s", got)
	}
}

// filepathContainsAny is a tiny substring helper for the assertion above —
// avoids pulling in strings just for one test.
func filepathContainsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
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
