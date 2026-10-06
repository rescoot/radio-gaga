package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"radio-gaga/internal/models"
)

func bootstrapTestCertificate(t *testing.T, hostname string, notBefore, notAfter time.Time) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestBootstrapTLSVerifiesServer(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, hostname string
		from, to       time.Time
		trusted, valid bool
	}{
		{"trusted", "bootstrap.test", now.Add(-time.Hour), now.Add(time.Hour), true, true},
		{"wrong hostname", "different.test", now.Add(-time.Hour), now.Add(time.Hour), true, false},
		{"untrusted", "bootstrap.test", now.Add(-time.Hour), now.Add(time.Hour), false, false},
		{"expired", "bootstrap.test", now.Add(-2 * time.Hour), now.Add(-time.Hour), true, false},
		{"not yet valid", "bootstrap.test", now.Add(time.Hour), now.Add(2 * time.Hour), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, ca := bootstrapTestCertificate(t, tc.hostname, tc.from, tc.to)
			if !tc.trusted {
				_, ca = bootstrapTestCertificate(t, "bootstrap.test", now.Add(-time.Hour), now.Add(time.Hour))
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			defer server.Close()

			config, err := bootstrapTLSConfig(models.MQTTConfig{
				BrokerURL: "ssl://bootstrap.test:8883", CACertEmbedded: ca,
			})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp",
				strings.TrimPrefix(server.URL, "https://"), config)
			if conn != nil {
				conn.Close()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("handshake error = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestBootstrapTLSRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "tcp://broker:1883", "ws://broker", "ssl://", "ssl://user:secret@broker", "ssl://broker/path", "ssl://broker?token=secret", "ssl://broker#fragment"} {
		if _, err := bootstrapTLSConfig(models.MQTTConfig{BrokerURL: endpoint}); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
}

func TestBootstrapTLSCertificateSources(t *testing.T) {
	_, ca := bootstrapTestCertificate(t, "bootstrap.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(ca), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []models.MQTTConfig{
		{BrokerURL: "ssl://bootstrap.test:8883"},
		{BrokerURL: "tls://bootstrap.test:8883", CACert: path},
		{BrokerURL: "ssl://bootstrap.test:8883", CACertEmbedded: ca, CACert: "missing.pem"},
	} {
		result, err := bootstrapTLSConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		if result.InsecureSkipVerify || result.ServerName != "bootstrap.test" || result.MinVersion < tls.VersionTLS12 {
			t.Fatal("bootstrap must verify server certificates and require TLS 1.2")
		}
		if config.CACert == "" && config.CACertEmbedded == "" && result.RootCAs != nil {
			t.Fatal("no custom CA should select system trust")
		}
	}
	for _, config := range []models.MQTTConfig{
		{BrokerURL: "ssl://bootstrap.test:8883", CACert: "missing.pem"},
		{BrokerURL: "ssl://bootstrap.test:8883", CACertEmbedded: "not a certificate"},
	} {
		if _, err := bootstrapTLSConfig(config); err == nil {
			t.Fatal("invalid custom CA must fail closed")
		}
	}
}
