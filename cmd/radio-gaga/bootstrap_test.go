package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostBootstrapRequestsRadioGagaConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/scooters/bootstrap" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer installer-token" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		for key, want := range map[string]string{
			"config_format": "radio-gaga", "imei": "866802022999001",
			"mdb_serial": "mdb-serial", "dbc_serial": "dbc-serial", "software_version": "vtest",
		} {
			if body[key] != want {
				t.Errorf("%s = %q, want %q", key, body[key], want)
			}
		}
		if want := detectPlatform(); body["platform"] != want {
			t.Errorf("platform = %q, want %q", body["platform"], want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"created","scooter_id":123,"config_yaml":"scooter: config"}`))
	}))
	defer server.Close()

	yaml, id, status, err := postBootstrap(server.URL, "installer-token", "866802022999001", "mdb-serial", "dbc-serial", "vtest")
	if err != nil || yaml != "scooter: config" || id != 123 || status != "created" {
		t.Fatalf("bootstrap = %q, %d, %q, %v", yaml, id, status, err)
	}
}

func TestPostBootstrapRejectsResponsesWithoutConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"created","scooter_id":123,"claim_pending":true}`))
	}))
	defer server.Close()

	_, _, _, err := postBootstrap(server.URL, "installer-token", "imei", "mdb", "dbc", "vtest")
	if err == nil || !strings.Contains(err.Error(), "missing config_yaml") {
		t.Fatalf("expected missing configuration error, got %v", err)
	}
}
