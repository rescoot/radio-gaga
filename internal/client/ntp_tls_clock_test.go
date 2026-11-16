package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"radio-gaga/internal/models"
)

func TestIndependentNTPClockCachesAndFailsClosed(t *testing.T) {
	oldQuery := queryTLSTime
	defer func() { queryTLSTime = oldQuery }()
	calls := 0
	reference := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	queryTLSTime = func(*models.NTPConfig) (time.Time, error) {
		calls++
		if calls > 1 {
			return time.Time{}, errors.New("NTP unavailable")
		}
		return reference, nil
	}
	clock := &ntpTLSClock{config: &models.NTPConfig{Enabled: true}}
	first, err := clock.current()
	if err != nil || !first.Equal(reference) {
		t.Fatalf("first time=%v, err=%v", first, err)
	}
	second, err := clock.current()
	if err != nil || second.Before(first) || calls != 1 {
		t.Fatalf("cached time=%v, calls=%d, err=%v", second, calls, err)
	}
	clock.received -= int64(31 * time.Second)
	if got := clock.time(); got.Year() != 10000 || calls != 2 {
		t.Fatalf("stale time=%v, calls=%d", got, calls)
	}
}

func TestLibrescootStrictTLSUsesNTPAndVerifiesCertificate(t *testing.T) {
	oldQuery, oldEnsure := queryTLSTime, ensureVerifiedClock
	defer func() { queryTLSTime = oldQuery; ensureVerifiedClock = oldEnsure }()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	ref := server.Certificate().NotBefore.Add(time.Minute)
	calls := 0
	queryTLSTime = func(*models.NTPConfig) (time.Time, error) { calls++; return ref, nil }
	ensureVerifiedClock = func(*models.NTPConfig, bool) error { t.Fatal("coordinator must not be called"); return nil }
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(&tls.Config{RootCAs: roots}).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
	cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: true}}
	_, _ = createMQTTClient(cfg, opts, "", nil, nil, true)
	if opts.TLSConfig.Time == nil || opts.TLSConfig.InsecureSkipVerify || calls != 1 {
		t.Fatalf("NTP TLS config not installed: calls=%d", calls)
	}
	if got := opts.TLSConfig.Time(); got.Before(ref) || got.After(ref.Add(time.Second)) {
		t.Fatalf("TLS time=%v, want NTP reference=%v", got, ref)
	}
	conn, err := tls.Dial("tcp", strings.TrimPrefix(server.URL, "https://"), opts.TLSConfig)
	if err != nil {
		t.Fatalf("valid certificate rejected: %v", err)
	}
	conn.Close()
	if calls != 1 {
		t.Fatalf("handshake did not reuse fresh NTP sample: %d calls", calls)
	}
	wrongHost := opts.TLSConfig.Clone()
	wrongHost.ServerName = "wrong.example"
	if conn, err = tls.Dial("tcp", strings.TrimPrefix(server.URL, "https://"), wrongHost); err == nil {
		conn.Close()
		t.Fatal("accepted a certificate for a different hostname")
	}
}

func TestLibrescootStrictTLSRejectsExpiredCertificateAtNTPTime(t *testing.T) {
	oldQuery := queryTLSTime
	defer func() { queryTLSTime = oldQuery }()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	queryTLSTime = func(*models.NTPConfig) (time.Time, error) {
		return server.Certificate().NotAfter.Add(time.Hour), nil
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(&tls.Config{RootCAs: roots}).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
	cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: true}}
	_, _ = createMQTTClient(cfg, opts, "", nil, nil, true)
	if conn, err := tls.Dial("tcp", strings.TrimPrefix(server.URL, "https://"), opts.TLSConfig); err == nil {
		conn.Close()
		t.Fatal("accepted an expired certificate")
	}
}

func TestLibrescootStrictTLSQueriesNTPWithoutSettingSystemTime(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		request := make([]byte, 512)
		n, addr, err := listener.ReadFrom(request)
		if err != nil || n < 48 {
			return
		}
		response := make([]byte, 48)
		response[0], response[1] = 0x24, 2
		copy(response[24:32], request[40:48])
		stamp := time.Now().Add(time.Minute)
		for _, offset := range []int{16, 32, 40} {
			binary.BigEndian.PutUint32(response[offset:], uint32(stamp.Unix()+2208988800))
			binary.BigEndian.PutUint32(response[offset+4:], uint32(uint64(stamp.Nanosecond())<<32/1000000000))
		}
		_, _ = listener.WriteTo(response, addr)
	}()
	opts := mqtt.NewClientOptions().AddBroker("ssl://127.0.0.1:1").SetTLSConfig(&tls.Config{}).SetConnectTimeout(50 * time.Millisecond).SetAutoReconnect(false)
	cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: true, Server: listener.LocalAddr().String()}}
	before := time.Now()
	_, _ = createMQTTClient(cfg, opts, "", nil, nil, true)
	if opts.TLSConfig.Time == nil {
		t.Fatal("NTP TLS clock not installed")
	}
	if offset := opts.TLSConfig.Time().Sub(time.Now()); offset < 59*time.Second || offset > 61*time.Second {
		t.Fatalf("NTP TLS time offset=%v", offset)
	}
	if elapsed := time.Since(before); elapsed < 0 || elapsed > 5*time.Second {
		t.Fatalf("system time changed: %v", elapsed)
	}
}

func TestLibrescootStrictTLSRejectsUnavailableNTP(t *testing.T) {
	oldQuery := queryTLSTime
	defer func() { queryTLSTime = oldQuery }()
	queryTLSTime = func(*models.NTPConfig) (time.Time, error) { return time.Time{}, errors.New("NTP unavailable") }
	opts := mqtt.NewClientOptions().SetTLSConfig(&tls.Config{})
	cfg := &models.Config{MQTT: models.MQTTConfig{RequireVerifiedTLS: true}, NTP: models.NTPConfig{Enabled: true}}
	_, err := createMQTTClient(cfg, opts, "", nil, nil, true)
	if err == nil || !strings.Contains(err.Error(), "NTP unavailable") {
		t.Fatalf("expected NTP failure before connection, got %v", err)
	}
}
