package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticConnectRequest(origin string) connectRequest {
	return connectRequest{SessionID: strings.Repeat("A", 43), Profile: connectProfile, ProviderOrigin: origin, ClaimToken: strings.Repeat("B", 43)}
}
func TestConnectEnrollmentDoesNotOverwriteExistingConfig(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	os.WriteFile(path, []byte("unmanaged"), 0600)
	request, _ := json.Marshal(syntheticConnectRequest(server.URL))
	var output bytes.Buffer
	if err := runConnectEnrollment(context.Background(), path, true, "", bytes.NewReader(request), &output); err == nil {
		t.Fatal("existing config accepted")
	}
	if calls != 0 {
		t.Fatal("contacted provider for existing configuration")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "unmanaged" {
		t.Fatal("existing configuration changed")
	}
}
func TestExistingCredentialRequiresMatchingProviderAndSafeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("scooter:\n  token: existing-secret\nmqtt:\n  broker_url: ssl://mqtt2.sunshine.rescoot.org:8883\n")
	if credential, err := existingConnectCredential(path, "https://sunshine.rescoot.org"); err != nil || credential != "existing-secret" {
		t.Fatal("legacy Sunshine configuration was not recognized")
	}
	if _, err := existingConnectCredential(path, "https://another.example"); err == nil {
		t.Fatal("existing credential exposed to unrelated provider")
	}
	write("scooter:\n  token: existing-secret\napi:\n  base_url: https://another.example\nmqtt:\n  broker_url: ssl://mqtt2.sunshine.rescoot.org:8883\n")
	if _, err := existingConnectCredential(path, "https://sunshine.rescoot.org"); err == nil {
		t.Fatal("explicit different API origin ignored")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err := existingConnectCredential(path, "https://sunshine.rescoot.org"); err == nil {
		t.Fatal("symlink configuration accepted")
	}
}

func TestExistingCredentialExchangeUsesOnlyProviderAuthorizationHeader(t *testing.T) {
	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer claim-secret" || r.Header.Get("X-Connect-Device-Credential") != "device-secret" {
			t.Error("missing credential binding")
		}
		if strings.Contains(r.URL.String(), "secret") {
			t.Error("credential leaked to URL")
		}
		received = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := connectHTTPWithDeviceCredential(context.Background(), http.MethodPost, server.URL, "claim-secret", "device-secret", map[string]string{"profile": connectProfile}, nil); err != nil || !received {
		t.Fatal("device proof exchange failed")
	}
}

func TestConnectTypedConfigCannotSetPathsOrDisableTLS(t *testing.T) {
	valid := connectMaterial{Profile: connectProfile, SessionID: strings.Repeat("A", 43), TransactionID: "connect-1", Identifier: "test", Token: strings.Repeat("a", 64), ScooterID: "1", BrokerURL: "ssl://broker.example:8883", Username: "test"}
	config, err := renderConnectConfig(valid, "https://cloud.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"redis://127.0.0.1:6379", "require_verified_tls: true", "environment: production"} {
		if !bytes.Contains(config, []byte(wanted)) {
			t.Fatalf("missing %s", wanted)
		}
	}
	valid.Token = "provider-issued." + strings.Repeat("x", 40)
	valid.ScooterID = "8ff77c45-e053-4f72-9713-b146a33bf72b"
	if _, err := renderConnectConfig(valid, "https://another-cloud.example"); err != nil {
		t.Fatal("provider-specific credential/ID format leaked into profile validation")
	}
	valid.BrokerURL = "tcp://broker.example:1883"
	if _, err := renderConnectConfig(valid, "https://cloud.example"); err == nil {
		t.Fatal("plaintext broker accepted")
	}
	if readConnectJSON(strings.NewReader(`{"profile":"radio-gaga/1","config_path":"/etc/shadow"}`), &connectMaterial{}) == nil {
		t.Fatal("provider-selected path accepted")
	}
}
func TestConnectResumeAcknowledgesWithoutRotatingOrApplying(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/connect/complete" {
			t.Error("repeated credential exchange")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	request := syntheticConnectRequest(server.URL)
	encoded, _ := json.Marshal(request)
	material := &connectMaterial{Profile: connectProfile, SessionID: request.SessionID, TransactionID: "connect-1", Identifier: "test", Token: strings.Repeat("a", 64), ScooterID: "1", BrokerURL: "ssl://broker.example:8883", Username: "test"}
	candidate, err := renderConnectConfig(*material, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, candidate, 0600)
	journal := connectJournal{RequestDigest: connectDigest(encoded), Material: material}
	if err := saveConnectJournal(filepath.Join(root, ".connect-enrollment.json"), journal); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var output bytes.Buffer
		if err := runConnectEnrollment(context.Background(), path, true, "", bytes.NewReader(encoded), &output); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), material.Token) {
			t.Fatal("credential printed")
		}
	}
	if calls != 2 {
		t.Fatal("completion not replayed")
	}
}
