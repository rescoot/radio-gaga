package commands

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"radio-gaga/internal/models"
)

type mockUpdateCACertClient struct {
	configPath       string
	reconnectCalled  bool
}

func (m *mockUpdateCACertClient) SendCommandResponse(requestID, status, errorMsg string) {}
func (m *mockUpdateCACertClient) GetCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	return defaultValue
}
func (m *mockUpdateCACertClient) CleanRetainedMessage(topic string) error { return nil }
func (m *mockUpdateCACertClient) PublishTelemetryData(current *models.TelemetryData) error {
	return nil
}
func (m *mockUpdateCACertClient) GetConfigPath() string { return m.configPath }
func (m *mockUpdateCACertClient) RequestReconnect()     { m.reconnectCalled = true }

func generateTestCACert() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})), nil
}

func generateTestNonCACert() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "Test Leaf"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  false,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})), nil
}

func TestHandleUpdateCACertCommand(t *testing.T) {
	caPEM, err := generateTestCACert()
	if err != nil {
		t.Fatalf("Failed to generate test CA cert: %v", err)
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "radio-gaga.yml")
	os.WriteFile(configPath, []byte("mqtt:\n  broker_url: ssl://test:8883\n"), 0644)

	t.Run("valid CA cert", func(t *testing.T) {
		config := &models.Config{
			Scooter:     models.ScooterConfig{Identifier: "test", Token: "tok"},
			Environment: "production",
			MQTT:        models.MQTTConfig{BrokerURL: "ssl://test:8883", KeepAlive: "30s", CACert: "/old/path.crt"},
			RedisURL:    "redis://localhost:6379",
			Telemetry: models.TelemetryConfig{
				Intervals: models.TelemetryIntervals{Driving: "1s", Standby: "5m", StandbyNoBattery: "8h", Hibernate: "24h"},
				Buffer:    models.BufferConfig{MaxSize: 1000, MaxRetries: 5, RetryInterval: "1m"},
				TransmitPeriod: "5m",
			},
		}
		client := &mockUpdateCACertClient{configPath: configPath}

		params := map[string]interface{}{"cert": caPEM}
		err := HandleUpdateCACertCommand(client, config, params, "req-1")
		if err != nil {
			t.Fatalf("Expected success, got error: %v", err)
		}

		if config.MQTT.CACertEmbedded != caPEM {
			t.Error("CACertEmbedded was not updated")
		}
		if config.MQTT.CACert != "" {
			t.Error("CACert file path was not cleared")
		}
		if !client.reconnectCalled {
			t.Error("RequestReconnect was not called")
		}
	})

	t.Run("missing cert param", func(t *testing.T) {
		config := &models.Config{}
		client := &mockUpdateCACertClient{configPath: configPath}

		err := HandleUpdateCACertCommand(client, config, map[string]interface{}{}, "req-2")
		if err == nil {
			t.Fatal("Expected error for missing cert param")
		}
	})

	t.Run("invalid PEM", func(t *testing.T) {
		config := &models.Config{}
		client := &mockUpdateCACertClient{configPath: configPath}

		params := map[string]interface{}{"cert": "not a valid PEM"}
		err := HandleUpdateCACertCommand(client, config, params, "req-3")
		if err == nil {
			t.Fatal("Expected error for invalid PEM")
		}
	})

	t.Run("non-CA cert rejected", func(t *testing.T) {
		nonCAPEM, err := generateTestNonCACert()
		if err != nil {
			t.Fatalf("Failed to generate test cert: %v", err)
		}

		config := &models.Config{}
		client := &mockUpdateCACertClient{configPath: configPath}

		params := map[string]interface{}{"cert": nonCAPEM}
		err = HandleUpdateCACertCommand(client, config, params, "req-4")
		if err == nil {
			t.Fatal("Expected error for non-CA cert")
		}
	})
}
