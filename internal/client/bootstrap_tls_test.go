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

	"github.com/alicebob/miniredis/v2"
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

func TestStrictLibrescootTLSRechecksClockOnEveryHandshake(t *testing.T) {
	previous := ensureVerifiedClock
	defer func() { ensureVerifiedClock = previous }()
	allowed := false
	ensureVerifiedClock = func(*models.NTPConfig, bool) error {
		if !allowed {
			return fmt.Errorf("stale clock")
		}
		return nil
	}
	cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}}
	opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(&tls.Config{}).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
	_, _ = createMQTTClient(cfg, opts, "", nil, nil, true)
	if opts.TLSConfig.VerifyConnection == nil {
		t.Fatal("missing handshake quality check")
	}
	if err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("stale quality accepted")
	}
	allowed = true
	if err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{}); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("reconnect accepted stale quality")
	}
}
func TestLibrescootRejectsInsecureTLS(t *testing.T) {
	opts := mqtt.NewClientOptions().SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	_, err := createMQTTClient(&models.Config{}, opts, "", nil, nil, true)
	if err == nil {
		t.Fatal("accepted insecure TLS")
	}
}

func TestClockPolicyUnknownIdentityFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		flavor      string
		coordinator bool
	}{{"stock", false}, {"librescoot", true}, {"unknown", true}, {"unknown_error", true}, {"unrecognized-os", true}, {"", true}} {
		if got := useCoordinatorClockPolicy(tc.flavor); got != tc.coordinator {
			t.Fatalf("%q: %v", tc.flavor, got)
		}
	}
}

func TestLibrescootCertificateErrorNeverUsesInsecureFallback(t *testing.T) {
	previous := ensureVerifiedClock
	ensureVerifiedClock = func(*models.NTPConfig, bool) error { return nil }
	defer func() { ensureVerifiedClock = previous }()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("expired certificate reached application") }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cfg := &tls.Config{RootCAs: roots, Time: func() time.Time { return server.Certificate().NotAfter.Add(time.Hour) }}
	opts := mqtt.NewClientOptions().AddBroker("ssl://" + strings.TrimPrefix(server.URL, "https://")).SetTLSConfig(cfg).SetConnectTimeout(time.Second).SetAutoReconnect(false)
	_, err := createMQTTClient(&models.Config{}, opts, "", nil, nil, true)
	if err == nil || !strings.Contains(err.Error(), "certificate verification is required") {
		t.Fatalf("unexpected result: %v", err)
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("insecure fallback enabled")
	}
}

func TestDetectMDBFlavorClockPolicy(t *testing.T) {
	for _, tc := range []struct {
		id, host         string
		readErr, hostErr error
		coordinator      bool
	}{
		{"ID=librescoot\n", "mdb-example", nil, nil, true},
		{"ID=scooteros\n", "librescoot-example", nil, nil, false},
		{"ID=scooteros-mdb\n", "librescoot-example", nil, nil, false},
		{"ID=scooteros-other\n", "mdb-example", nil, nil, true},
		{"ID=unknown\n", "mdb-example", nil, nil, true},
		{"ID=stock\n", "mdb-example", nil, nil, true},
		{"ID=arbitrary\n", "mdb-example", nil, nil, true},
		{"", "mdb-example", nil, nil, false},
		{"", "librescoot-example", fmt.Errorf("missing"), nil, true},
		{"", "other", fmt.Errorf("missing"), nil, true},
		{"", "", fmt.Errorf("missing"), fmt.Errorf("missing"), true},
	} {
		flavor, _ := detectMDBFlavor(tc.id, tc.readErr, tc.host, tc.hostErr)
		if got := useCoordinatorClockPolicy(flavor); got != tc.coordinator {
			t.Fatalf("id=%q host=%q flavor=%q got=%v", tc.id, tc.host, flavor, got)
		}
	}
}

func TestProductionAndProbeShareClockPolicy(t *testing.T) {
	oldRead, oldEnsure := readMDBIdentity, ensureVerifiedClock
	defer func() { readMDBIdentity = oldRead; ensureVerifiedClock = oldEnsure }()
	server := miniredis.RunT(t)
	for _, tc := range []struct {
		id, host         string
		readErr, hostErr error
		coordinator      bool
	}{
		{"ID=unknown\n", "mdb-test", nil, nil, true},
		{"ID=other\n", "mdb-test", nil, nil, true},
		{"", "mdb-test", fmt.Errorf("missing"), nil, false},
		{"", "librescoot-test", fmt.Errorf("missing"), nil, true},
		{"", "", fmt.Errorf("missing"), fmt.Errorf("missing"), true},
		{"ID=scooteros-mdb\n", "other", nil, nil, false},
	} {
		readMDBIdentity = func() ([]byte, error, string, error) { return []byte(tc.id), tc.readErr, tc.host, tc.hostErr }
		seen := []bool{}
		ensureVerifiedClock = func(_ *models.NTPConfig, coordinated bool) error {
			seen = append(seen, coordinated)
			return fmt.Errorf("test stops before connecting")
		}
		config := &models.Config{RedisURL: "redis://" + server.Addr(), Scooter: models.ScooterConfig{Identifier: "test", Token: "test"}, MQTT: models.MQTTConfig{BrokerURL: "ssl://127.0.0.1:1", KeepAlive: "30s", RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: false}}
		_, _ = NewScooterMQTTClient(config, "", "test")
		if len(seen) != 1 || seen[0] != tc.coordinator {
			t.Fatalf("production ID=%q host=%q policy=%v", tc.id, tc.host, seen)
		}
		seen = nil
		config.NTP.Enabled = true
		_ = RunProbe(config, time.Second, 0)
		if len(seen) != 1 || seen[0] != tc.coordinator {
			t.Fatalf("probe ID=%q host=%q policy=%v", tc.id, tc.host, seen)
		}
	}
}
