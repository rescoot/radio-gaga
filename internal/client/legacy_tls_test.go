package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"radio-gaga/internal/models"
)

func legacyTLSFixture(t *testing.T) (*models.Config, *tls.Config, *x509.Certificate) {
	t.Helper()
	ca, err := os.ReadFile("testdata/legacy-mqtt-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	leafPEM, err := os.ReadFile("testdata/legacy-mqtt-leaf.pem")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(leafPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA fixture")
	}
	return &models.Config{MQTT: models.MQTTConfig{BrokerURL: "ssl://mqtt.sunshine.rescoot.org:8883", CACertEmbedded: string(ca)}}, &tls.Config{RootCAs: roots}, leaf
}

func TestLegacyPinnedTLSAuthenticatesExpiredCertificate(t *testing.T) {
	config, base, leaf := legacyTLSFixture(t)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: base.RootCAs, DNSName: legacyBrokerHost, CurrentTime: leaf.NotAfter.Add(time.Hour)}); err == nil {
		t.Fatal("fixture must fail ordinary expiry validation")
	}
	cfg, ok := legacyPinnedTLS(config, base)
	if !ok {
		t.Fatal("legacy configuration not recognized")
	}
	if base.InsecureSkipVerify || base.VerifyConnection != nil {
		t.Fatal("mutated original config")
	}
	if cfg.ServerName != legacyBrokerHost || cfg.VerifyConnection == nil {
		t.Fatal("missing identity verification")
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("missing certificate accepted")
	}
	if err := verifyPinnedLegacyCertificate(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, x509.NewCertPool()); err == nil {
		t.Fatal("untrusted signer accepted")
	}
	changed := *leaf
	changed.Raw = append([]byte(nil), leaf.Raw...)
	changed.Raw[len(changed.Raw)-1] ^= 1
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{&changed}}); err == nil {
		t.Fatal("unrecognized certificate accepted")
	}
}

func TestLegacyPinnedTLSIsNarrowlyScoped(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*models.Config, *tls.Config)
	}{
		{"strict enrollment", func(c *models.Config, _ *tls.Config) { c.MQTT.RequireVerifiedTLS = true }},
		{"modern broker", func(c *models.Config, _ *tls.Config) { c.MQTT.BrokerURL = "ssl://mqtt2.sunshine.rescoot.org:8883" }},
		{"other host", func(c *models.Config, _ *tls.Config) { c.MQTT.BrokerURL = "ssl://example.org:8883" }},
		{"other port", func(c *models.Config, _ *tls.Config) { c.MQTT.BrokerURL = "ssl://mqtt.sunshine.rescoot.org:8887" }},
		{"plaintext", func(c *models.Config, _ *tls.Config) { c.MQTT.BrokerURL = "tcp://mqtt.sunshine.rescoot.org:8883" }},
		{"missing CA", func(c *models.Config, _ *tls.Config) { c.MQTT.CACertEmbedded = "" }},
		{"invalid CA", func(c *models.Config, _ *tls.Config) { c.MQTT.CACertEmbedded = "invalid" }},
		{"other CA", func(c *models.Config, _ *tls.Config) {
			b, _ := os.ReadFile("testdata/legacy-mqtt-leaf.pem")
			c.MQTT.CACertEmbedded = string(b)
		}},
		{"conflicting SNI", func(_ *models.Config, b *tls.Config) { b.ServerName = "example.org" }},
		{"insecure input", func(_ *models.Config, b *tls.Config) { b.InsecureSkipVerify = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, base, _ := legacyTLSFixture(t)
			test.change(config, base)
			if _, ok := legacyPinnedTLS(config, base); ok {
				t.Fatal("unexpected compatibility exception")
			}
		})
	}
}

func TestOrdinaryLibrescootTLSDoesNotRequireFreshCoordinator(t *testing.T) {
	previous := ensureVerifiedClock
	defer func() { ensureVerifiedClock = previous }()
	ensureVerifiedClock = func(*models.NTPConfig, bool) error {
		t.Fatal("ordinary verified TLS must not require coordinator freshness")
		return fmt.Errorf("stale")
	}
	config := &models.Config{MQTT: models.MQTTConfig{BrokerURL: "ssl://mqtt2.sunshine.rescoot.org:8883"}}
	calls := 0
	base := &tls.Config{VerifyConnection: func(tls.ConnectionState) error { calls++; return nil }}
	opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(base).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
	_, _ = createMQTTClient(config, opts, "", nil, nil, true)
	if opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("ordinary certificate verification disabled")
	}
	for i := 0; i < 2; i++ {
		if err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("existing TLS verifier not preserved")
	}
}

func TestLegacyPinnedTLSFileCAAndPriorVerifier(t *testing.T) {
	config, base, leaf := legacyTLSFixture(t)
	config.MQTT.CACertEmbedded = ""
	config.MQTT.CACert = "testdata/legacy-mqtt-ca.pem"
	base.VerifyConnection = func(tls.ConnectionState) error { return fmt.Errorf("additional policy") }
	cfg, ok := legacyPinnedTLS(config, base)
	if !ok {
		t.Fatal("file CA not recognized")
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil || !strings.Contains(err.Error(), "additional policy") {
		t.Fatalf("lost verifier: %v", err)
	}
}

func TestLegacyPinnedTLSWorksWithoutFreshCoordinatorOnBothPlatforms(t *testing.T) {
	previous := ensureVerifiedClock
	defer func() { ensureVerifiedClock = previous }()
	ensureVerifiedClock = func(*models.NTPConfig, bool) error {
		t.Fatal("pinned certificate must not depend on wall-clock quality")
		return fmt.Errorf("stale")
	}
	for _, librescoot := range []bool{false, true} {
		config, base, leaf := legacyTLSFixture(t)
		// Avoid external traffic; exercise the production options and reconnect verifier.
		opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(base).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
		_, _ = createMQTTClient(config, opts, "", nil, nil, librescoot)
		for i := 0; i < 2; i++ {
			if opts.TLSConfig.VerifyConnection == nil {
				t.Fatal("missing pinned verifier")
			}
			if err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
