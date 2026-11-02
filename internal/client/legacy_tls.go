package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"time"

	"radio-gaga/internal/models"
)

const (
	legacyBrokerHost      = "mqtt.sunshine.rescoot.org"
	legacyCAFingerprint   = "f96f7a3661c226921d388d73d184e956a19812dc4983b1e171fad8dbc08e59b3"
	legacyLeafFingerprint = "e68b12c6df7f83ac9fc7cda3633af4542dbe00f511bdf4250cb4c88333ebc8a8"
)

// legacyPinnedTLS tolerates dates only for the exact legacy server and configured
// CA. It still verifies the hostname, chain signatures and TLS proof of key possession.
func legacyPinnedTLS(config *models.Config, base *tls.Config) (*tls.Config, bool) {
	if base == nil || base.InsecureSkipVerify {
		return nil, false
	}
	u, err := url.Parse(config.MQTT.BrokerURL)
	if err != nil || u.Hostname() != legacyBrokerHost || (u.Port() != "" && u.Port() != "8883") ||
		(u.Scheme != "ssl" && u.Scheme != "tls" && u.Scheme != "mqtts") ||
		(base.ServerName != "" && base.ServerName != legacyBrokerHost) {
		return nil, false
	}
	data := []byte(config.MQTT.CACertEmbedded)
	if len(data) == 0 && config.MQTT.CACert != "" {
		data, err = os.ReadFile(config.MQTT.CACert)
		if err != nil {
			return nil, false
		}
	}
	roots := x509.NewCertPool()
	found := false
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || certificateFingerprint(block.Bytes) != legacyCAFingerprint {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, false
		}
		roots.AddCert(cert)
		found = true
	}
	if !found {
		return nil, false
	}
	cfg := base.Clone()
	previous := cfg.VerifyConnection
	// Go's built-in date check must be replaced, not certificate authentication.
	cfg.InsecureSkipVerify = true
	cfg.ServerName = legacyBrokerHost
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verifyPinnedLegacyCertificate(state, roots); err != nil {
			return err
		}
		if previous != nil {
			return previous(state)
		}
		return nil
	}
	return cfg, true
}

func verifyPinnedLegacyCertificate(state tls.ConnectionState, roots *x509.CertPool) error {
	if len(state.PeerCertificates) == 0 || certificateFingerprint(state.PeerCertificates[0].Raw) != legacyLeafFingerprint {
		return fmt.Errorf("legacy MQTT certificate does not match the pinned identity")
	}
	leaf := state.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	// The exact pinned certificate is checked inside its validity window; this
	// never changes the system clock or relaxes validation for another identity.
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		DNSName:       legacyBrokerHost,
		CurrentTime:   leaf.NotBefore.Add(time.Second),
	})
	if err != nil {
		return fmt.Errorf("legacy MQTT certificate authentication failed: %w", err)
	}
	return nil
}

func certificateFingerprint(der []byte) string {
	digest := sha256.Sum256(der)
	return hex.EncodeToString(digest[:])
}
