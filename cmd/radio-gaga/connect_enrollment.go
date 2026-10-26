package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"gopkg.in/yaml.v2"
	"radio-gaga/internal/txn"
)

const connectProfile = "radio-gaga/1"

type connectRequest struct {
	SessionID      string `json:"session_id"`
	Profile        string `json:"profile"`
	ProviderOrigin string `json:"provider_origin"`
	ClaimToken     string `json:"claim_token"`
}
type connectIdentity struct {
	IMEI string `json:"imei"`
	MDB  string `json:"mdb_serial"`
	DBC  string `json:"dbc_serial"`
}
type connectMaterial struct {
	Profile       string `json:"profile"`
	SessionID     string `json:"session_id"`
	TransactionID string `json:"transaction_id"`
	Identifier    string `json:"identifier"`
	Token         string `json:"token"`
	ScooterID     string `json:"scooter_id"`
	BrokerURL     string `json:"mqtt_broker_url"`
	Username      string `json:"mqtt_username"`
	CACert        string `json:"mqtt_ca_cert"`
}
type connectJournal struct {
	RequestDigest string           `json:"request_digest"`
	Identity      connectIdentity  `json:"identity"`
	Material      *connectMaterial `json:"material,omitempty"`
	ConfigDigest  string           `json:"config_digest,omitempty"`
}

func connectDigest(data []byte) string { v := sha256.Sum256(data); return hex.EncodeToString(v[:]) }
func readConnectJSON(r io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(r, 65537))
	if err != nil {
		return err
	}
	if len(data) > 65536 {
		return errors.New("oversized enrollment data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid enrollment JSON")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("trailing enrollment data")
	}
	return nil
}
func readConnectJournal(path string, journal *connectJournal) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("private enrollment journal required")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return readConnectJSON(f, journal)
}
func saveConnectJournal(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".connect-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	ce := file.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func connectHTTP(ctx context.Context, method, endpoint, token string, payload, out any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("provider request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	if out != nil {
		return readConnectJSON(response.Body, out)
	}
	return nil
}
func connectSafeState(ctx context.Context) error {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
	defer client.Close()
	values, err := client.HMGet(ctx, "vehicle", "state", "kickstand").Result()
	if err != nil {
		return errors.New("vehicle state unavailable")
	}
	if !(values[0] == "stand-by" || values[0] == "parked" && values[1] == "down") {
		return errors.New("waiting_for_safe_state")
	}
	statuses, err := client.HMGet(ctx, "ota", "status:mdb", "status:dbc").Result()
	if err != nil {
		return errors.New("update state unavailable")
	}
	for _, value := range statuses {
		if value == nil {
			continue
		}
		switch value {
		case "", "idle", "failed", "error", "complete", "completed", "success":
		default:
			return errors.New("update in progress")
		}
	}
	return nil
}
func renderConnectConfig(material connectMaterial, providerOrigin string) ([]byte, error) {
	broker, err := url.Parse(material.BrokerURL)
	if err != nil || broker.Hostname() == "" || broker.User != nil || broker.RawQuery != "" || broker.Fragment != "" || (broker.Path != "" && broker.Path != "/") {
		return nil, errors.New("invalid MQTT endpoint")
	}
	if broker.Scheme != "ssl" && broker.Scheme != "tls" {
		return nil, errors.New("verified MQTT TLS required")
	}
	if len(material.Token) < 32 || len(material.Token) > 4096 || !regexp.MustCompile(`^[!-~]+$`).MatchString(material.Token) || material.Identifier == "" || len(material.Identifier) > 128 || material.Username == "" || len(material.Username) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(material.ScooterID) {
		return nil, errors.New("invalid connection credential")
	}
	mqtt := map[string]any{"broker_url": material.BrokerURL, "username": material.Username, "keepalive": "180s", "require_verified_tls": true}
	if material.CACert != "" {
		mqtt["ca_cert_embedded"] = material.CACert
	}
	data := map[string]any{"environment": "production", "redis_url": "redis://127.0.0.1:6379", "scooter": map[string]string{"identifier": material.Identifier, "token": material.Token}, "mqtt": mqtt, "commands": map[string]any{"shell": map[string]bool{"disabled": true}, "redis": map[string]bool{"disabled": true}}, "api": map[string]string{"base_url": providerOrigin, "scooter_id": material.ScooterID}}
	return yaml.Marshal(data)
}
func runConnectEnrollment(ctx context.Context, path string, development bool, identityFile string, input io.Reader, output io.Writer) error {
	var request connectRequest
	if err := readConnectJSON(input, &request); err != nil {
		return err
	}
	if request.Profile != connectProfile || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(request.SessionID) || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(request.ClaimToken) {
		return errors.New("invalid pairing claim")
	}
	origin, err := url.Parse(request.ProviderOrigin)
	if err != nil || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") || (origin.Scheme != "https" && !(development && origin.Scheme == "http" && origin.Hostname() == "127.0.0.1")) {
		return errors.New("verified provider origin required")
	}
	base := strings.TrimSuffix(request.ProviderOrigin, "/")
	if path == "" {
		path = "/data/radio-gaga/config.yaml"
	}
	if !development && path != "/data/radio-gaga/config.yaml" {
		return errors.New("managed enrollment requires the Librescoot config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil || !directory.IsDir() {
		return errors.New("unsafe config directory")
	}
	fd, err := syscall.Open(filepath.Join(filepath.Dir(path), ".connect.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), "connect-lock")
	defer lock.Close()
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("enrollment already running")
	}
	journalPath := filepath.Join(filepath.Dir(path), ".connect-enrollment.json")
	requestBytes, _ := json.Marshal(request)
	digest := connectDigest(requestBytes)
	journal := connectJournal{RequestDigest: digest}
	if err := readConnectJournal(journalPath, &journal); err != nil && !os.IsNotExist(err) {
		return err
	}
	if journal.RequestDigest != digest {
		return errors.New("another enrollment owns this configuration")
	}
	if journal.Material == nil {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("existing configuration blocks enrollment")
		} else if !os.IsNotExist(err) {
			return err
		}
		if identityFile != "" {
			if !development {
				return errors.New("identity override is only available for isolated tests")
			}
			file, err := os.Open(identityFile)
			if err != nil {
				return err
			}
			err = readConnectJSON(file, &journal.Identity)
			file.Close()
			if err != nil {
				return err
			}
		} else if journal.Identity.IMEI == "" && journal.Identity.MDB == "" {
			journal.Identity.IMEI = tryReadIMEI(bootstrapModemTimeout)
			journal.Identity.MDB, journal.Identity.DBC = tryReadSerials()
		}
		if journal.Identity.IMEI == "" && journal.Identity.MDB == "" {
			return errors.New("no scooter identity available")
		}
		if err := saveConnectJournal(journalPath, journal); err != nil {
			return err
		}
		payload := map[string]string{"session_id": request.SessionID, "profile": request.Profile, "imei": journal.Identity.IMEI, "mdb_serial": journal.Identity.MDB, "dbc_serial": journal.Identity.DBC}
		var material connectMaterial
		if err := connectHTTP(ctx, http.MethodPost, base+"/api/v1/connect/exchange", request.ClaimToken, payload, &material); err != nil {
			return err
		}
		if material.SessionID != request.SessionID || material.Profile != request.Profile || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(material.TransactionID) {
			return errors.New("provider binding mismatch")
		}
		journal.Material = &material
		if err := saveConnectJournal(journalPath, journal); err != nil {
			return err
		}
	}
	candidate, err := renderConnectConfig(*journal.Material, base)
	if err != nil {
		return err
	}
	candidateDigest := connectDigest(candidate)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	manager := &txn.Manager{LiveConfigPath: path, LiveBinaryPath: exe, PendingPath: filepath.Join(filepath.Dir(path), ".txn-pending.json"), ConfigInstallOnly: true, ConfigMode: 0600, Logger: log.New(io.Discard, "", 0)}
	if pending, err := os.ReadFile(manager.PendingPath); err == nil {
		var marker struct {
			TxnID string `json:"txn_id"`
		}
		if json.Unmarshal(pending, &marker) != nil || marker.TxnID != journal.Material.TransactionID {
			return errors.New("another configuration transaction is pending")
		}
		if err := manager.RecoverOnBoot(); err != nil {
			return errors.New("configuration recovery failed")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe configuration target")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing, readErr := os.ReadFile(path)
	if readErr == nil {
		if connectDigest(existing) != candidateDigest {
			return errors.New("existing configuration blocks enrollment")
		}
	} else {
		if !os.IsNotExist(readErr) {
			return readErr
		}
		if !development {
			if err := connectSafeState(ctx); err != nil {
				return err
			}
		}
		committed, err := manager.Run(ctx, journal.Material.TransactionID, txn.KindConfig, txn.Candidate{Config: candidate}, txn.SubprocessProbe(io.Discard))
		if !committed || err != nil {
			return errors.New("connection probe or config commit failed")
		}
	}
	journal.ConfigDigest = candidateDigest
	if err := saveConnectJournal(journalPath, journal); err != nil {
		return err
	}
	if !development {
		if err := connectSafeState(ctx); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, "systemctl", "start", "librescoot-radio-gaga.service").Run(); err != nil {
			return errors.New("cloud client start failed")
		}
		if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "librescoot-radio-gaga.service").Run(); err != nil {
			return errors.New("cloud client is not running")
		}
	}
	if err := connectHTTP(ctx, http.MethodPost, base+"/api/v1/connect/complete", journal.Material.Token, map[string]string{"session_id": request.SessionID}, nil); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]string{"state": "connected", "profile": connectProfile})
}
