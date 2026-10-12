package client

import (
	"crypto/tls"
	"crypto/x509"
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

func TestEnrollmentConfigRefusesInsecureTLSFallback(t *testing.T) {
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
	_, err := createMQTTClient(config, opts, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "certificate verification is required") {
		t.Fatalf("wanted verified TLS failure, got %v", err)
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLS verification was disabled")
	}
}
