package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadConfig_AppliesDefaultsToOmittedYAMLFields(t *testing.T) {
	withFakeStateDir(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "radio-gaga.yml")
	yaml := `
scooter:
  identifier: TESTVIN
  token: testtoken
mqtt:
  broker_url: ssl://example.test:8883
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: path})
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.NTP.Enabled || cfg.NTP.Server != "pool.ntp.rescoot.org" {
		t.Errorf("NTP defaults = %+v", cfg.NTP)
	}
	if cfg.Environment != "production" {
		t.Errorf("Environment = %q, want production", cfg.Environment)
	}
	if cfg.MQTT.KeepAlive != "30s" {
		t.Errorf("MQTT.KeepAlive = %q, want 30s", cfg.MQTT.KeepAlive)
	}
	if cfg.RedisURL != "redis://127.0.0.1:6379" {
		t.Errorf("RedisURL = %q, want default", cfg.RedisURL)
	}
	if cfg.Telemetry.Intervals.Driving != "30s" || cfg.Telemetry.Priorities.Immediate != "10s" {
		t.Errorf("telemetry defaults not applied: %+v", cfg.Telemetry)
	}
	if cfg.Events.Enabled == nil || !*cfg.Events.Enabled || cfg.Events.MaxRetries != 10 {
		t.Errorf("events defaults = %+v", cfg.Events)
	}
}

func TestLoadConfig_YAMLOverridesDefaults(t *testing.T) {
	withFakeStateDir(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "radio-gaga.yml")
	yaml := `
scooter:
  identifier: TESTVIN
  token: testtoken
environment: development
mqtt:
  broker_url: ssl://example.test:8883
  keepalive: 45s
ntp:
  enabled: false
redis_url: redis://custom:6379
telemetry:
  intervals:
    driving: 45s
  buffer:
    enabled: true
events:
  enabled: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadConfig(&models.CommandLineFlags{ConfigPath: path})
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.NTP.Enabled {
		t.Error("explicit ntp.enabled=false was overwritten")
	}
	if cfg.NTP.Server != "pool.ntp.rescoot.org" {
		t.Errorf("omitted NTP server = %q, want default", cfg.NTP.Server)
	}
	if cfg.Environment != "development" || cfg.MQTT.KeepAlive != "45s" || cfg.RedisURL != "redis://custom:6379" {
		t.Errorf("explicit overrides not retained: environment=%q keepalive=%q redis=%q", cfg.Environment, cfg.MQTT.KeepAlive, cfg.RedisURL)
	}
	if cfg.Telemetry.Intervals.Driving != "45s" || cfg.Telemetry.Intervals.Standby != "5m" || !cfg.Telemetry.Buffer.Enabled {
		t.Errorf("nested telemetry merge incorrect: %+v", cfg.Telemetry)
	}
	if cfg.Events.Enabled == nil || *cfg.Events.Enabled {
		t.Errorf("explicit events.enabled=false was overwritten: %+v", cfg.Events)
	}
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

func TestValidateConfig_MissingKeepaliveNamesCanonicalYAMLPath(t *testing.T) {
	// radio-gaga's YAML key is `keepalive`, not `keep_alive`. A config that omits
	// it must say so by the key an operator can actually edit.
	config := &models.Config{
		Scooter:  models.ScooterConfig{Identifier: "VIN", Token: "tok"},
		RedisURL: "redis://localhost:6379",
	}

	err := ValidateConfig(config)
	if err == nil {
		t.Fatal("Expected validation error for empty mqtt.keepalive, got nil")
	}
	if !strings.Contains(err.Error(), "mqtt.keepalive") {
		t.Errorf("validation error %q should name mqtt.keepalive", err.Error())
	}
	if strings.Contains(err.Error(), "keep_alive") {
		t.Errorf("validation error %q should not use the keep_alive alias", err.Error())
	}
}

func TestConfigField_KeepaliveUsesCanonicalKey(t *testing.T) {
	config := &models.Config{}

	if err := SetConfigField(config, "mqtt.keepalive", "45s"); err != nil {
		t.Fatalf("SetConfigField(mqtt.keepalive) failed: %v", err)
	}
	if config.MQTT.KeepAlive != "45s" {
		t.Errorf("MQTT.KeepAlive = %q, want 45s", config.MQTT.KeepAlive)
	}

	if err := SetConfigField(config, "mqtt.keep_alive", "1m"); err == nil {
		t.Error("SetConfigField(mqtt.keep_alive) should be rejected now that the alias is gone")
	}
}
