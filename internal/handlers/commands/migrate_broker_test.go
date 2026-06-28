package commands

import (
	"os"
	"path/filepath"
	"testing"

	"radio-gaga/internal/models"
)

func migrateTestConfig(configPath string) *models.Config {
	return &models.Config{
		Scooter:     models.ScooterConfig{Identifier: "test", Token: "tok"},
		Environment: "production",
		MQTT:        models.MQTTConfig{BrokerURL: "ssl://old:8883", KeepAlive: "30s", CACertEmbedded: "OLD-CA"},
		RedisURL:    "redis://localhost:6379",
		Telemetry: models.TelemetryConfig{
			Intervals:      models.TelemetryIntervals{Driving: "1s", Standby: "5m", StandbyNoBattery: "8h", Hibernate: "24h"},
			Buffer:         models.BufferConfig{MaxSize: 1000, MaxRetries: 5, RetryInterval: "1m"},
			TransmitPeriod: "5m",
		},
	}
}

func TestHandleMigrateBrokerCommand(t *testing.T) {
	caPEM, err := generateTestCACert()
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "radio-gaga.yml")
	os.WriteFile(configPath, []byte("mqtt:\n  broker_url: ssl://old:8883\n"), 0644)

	t.Run("broker_url + CA sets both atomically and reconnects", func(t *testing.T) {
		config := migrateTestConfig(configPath)
		client := &mockUpdateCACertClient{configPath: configPath}

		params := map[string]interface{}{
			"broker_url": "ssl://mqtt2.sunshine.rescoot.org:8883",
			"ca_cert":    caPEM,
		}
		if err := HandleMigrateBrokerCommand(client, config, params, "req-1"); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if config.MQTT.BrokerURL != "ssl://mqtt2.sunshine.rescoot.org:8883" {
			t.Errorf("broker_url not set: %q", config.MQTT.BrokerURL)
		}
		if config.MQTT.CACertEmbedded != caPEM {
			t.Error("CACertEmbedded not updated")
		}
		if !client.reconnectCalled {
			t.Error("RequestReconnect not called")
		}
	})

	t.Run("broker_url only leaves CA untouched", func(t *testing.T) {
		config := migrateTestConfig(configPath)
		client := &mockUpdateCACertClient{configPath: configPath}

		params := map[string]interface{}{"broker_url": "ssl://mqtt2.sunshine.rescoot.org:8883"}
		if err := HandleMigrateBrokerCommand(client, config, params, "req-2"); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if config.MQTT.CACertEmbedded != "OLD-CA" {
			t.Errorf("CA should be untouched, got %q", config.MQTT.CACertEmbedded)
		}
	})

	t.Run("missing broker_url errors", func(t *testing.T) {
		config := migrateTestConfig(configPath)
		client := &mockUpdateCACertClient{configPath: configPath}
		if err := HandleMigrateBrokerCommand(client, config, map[string]interface{}{}, "req-3"); err == nil {
			t.Fatal("expected error for missing broker_url")
		}
	})

	t.Run("non-CA cert rejected and config unchanged", func(t *testing.T) {
		nonCA, err := generateTestNonCACert()
		if err != nil {
			t.Fatalf("generate non-CA: %v", err)
		}
		config := migrateTestConfig(configPath)
		client := &mockUpdateCACertClient{configPath: configPath}
		params := map[string]interface{}{"broker_url": "ssl://mqtt2:8883", "ca_cert": nonCA}
		if err := HandleMigrateBrokerCommand(client, config, params, "req-4"); err == nil {
			t.Fatal("expected error for non-CA cert")
		}
		if config.MQTT.BrokerURL != "ssl://old:8883" {
			t.Errorf("broker_url should be unchanged on validation failure, got %q", config.MQTT.BrokerURL)
		}
		if client.reconnectCalled {
			t.Error("RequestReconnect must not be called on failure")
		}
	})
}
