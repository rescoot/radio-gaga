package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"radio-gaga/internal/models"
)

func TestVerifiedTLSWaitsForNTPBeforeConnecting(t *testing.T) {
	previous := ensureVerifiedClock
	ensureVerifiedClock = func(*models.NTPConfig, bool) error { return fmt.Errorf("time unavailable") }
	defer func() { ensureVerifiedClock = previous }()
	config := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}}
	_, err := createMQTTClient(config, mqtt.NewClientOptions(), "", nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "synchronized NTP time") {
		t.Fatalf("wanted NTP failure before connection, got %v", err)
	}
}

func TestEnrollmentConfigRefusesInsecureTLSFallback(t *testing.T) {
	previous := ensureVerifiedClock
	ensureVerifiedClock = func(*models.NTPConfig, bool) error { return nil }
	defer func() { ensureVerifiedClock = previous }()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("expired certificate must prevent application data")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots, Time: func() time.Time { return server.Certificate().NotAfter.Add(time.Hour) }}
	opts := mqtt.NewClientOptions().AddBroker("ssl://" + strings.TrimPrefix(server.URL, "https://")).
		SetTLSConfig(tlsConfig).SetConnectTimeout(time.Second).SetAutoReconnect(false)
	config := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: false}}
	_, err := createMQTTClient(config, opts, "", nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "certificate verification is required") {
		t.Fatalf("wanted verified TLS failure, got %v", err)
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLS verification was disabled")
	}
}

func TestVerifiedTLSUsesSelectedPlatformPolicy(t *testing.T) {
	previous := ensureVerifiedClock
	defer func() { ensureVerifiedClock = previous }()
	for _, platform := range []bool{false, true} {
		seen := !platform
		ensureVerifiedClock = func(_ *models.NTPConfig, librescoot bool) error {
			seen = librescoot
			return fmt.Errorf("not synchronized")
		}
		cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}}
		_, err := createMQTTClient(cfg, mqtt.NewClientOptions(), "", nil, nil, platform)
		if err == nil || seen != platform {
			t.Fatalf("platform=%v, passed=%v, err=%v", platform, seen, err)
		}
	}
}
