package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"radio-gaga/internal/modeminfo"
	"radio-gaga/internal/txn"
	"radio-gaga/internal/utils"
)

// Bootstrap timing knobs. The IMEI fetch can be slow on a fresh boot — the
// modem USB device takes a while to enumerate after reset — so we wait a
// while before giving up. MDB serials are read from /sys, instant.
const (
	bootstrapModemTimeout = 30 * time.Second
	bootstrapHTTPTimeout  = 30 * time.Second
	bootstrapTxnTimeout   = 90 * time.Second
)

// runBootstrap performs the per-user-token install flow. Stages the YAML
// returned by Sunshine through txn.Manager.Run; the probe child is the same
// binary running with the staged config.
//
// configPath is where the live config will end up (and where the staging /
// LKG / pending files live alongside). For LibreScoot that's typically
// /data/radio-gaga/config.yaml; for stock it's /etc/rescoot/radio-gaga.yml.
// The install script passes this as -config.
func runBootstrap(token, apiBaseURL, configPath, softwareVersion string) error {
	if token == "" {
		return errors.New("-bootstrap-token is required for -bootstrap")
	}
	if apiBaseURL == "" {
		return errors.New("-api-base-url is required for -bootstrap")
	}
	if configPath == "" {
		return errors.New("-config is required for -bootstrap (where the live config will end up)")
	}

	// 1. Hardware identifiers — best-effort. We need at least IMEI or MDB SN.
	log.Printf("bootstrap: gathering hardware identifiers (modem timeout %s)…", bootstrapModemTimeout)
	imei := tryReadIMEI(bootstrapModemTimeout)
	mdbSerial, dbcSerial := tryReadSerials()
	if imei == "" && mdbSerial == "" {
		return errors.New("no hardware identifier available: modem unreachable AND MDB serial unreadable")
	}
	log.Printf("bootstrap: imei=%q mdb_sn=%q dbc_sn=%q", imei, mdbSerial, dbcSerial)

	// 2. POST to Sunshine.
	yaml, scooterID, status, err := postBootstrap(apiBaseURL, token, imei, mdbSerial, dbcSerial, softwareVersion)
	if err != nil {
		return fmt.Errorf("bootstrap request: %w", err)
	}
	log.Printf("bootstrap: server status=%s scooter_id=%d (%d bytes of config)", status, scooterID, len(yaml))

	// 3. Hand to the txn machinery. Run RecoverOnBoot first so any leftover
	//    pending state from a prior crashed bootstrap is reconciled before
	//    we open a new transaction.
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	manager := &txn.Manager{
		LiveConfigPath: configPath,
		LiveBinaryPath: exePath,
		PendingPath:    filepath.Join(filepath.Dir(configPath), ".txn-pending.json"),
		Logger:         log.Default(),
	}
	if err := manager.RecoverOnBoot(); err != nil {
		log.Printf("bootstrap: WARNING pre-bootstrap recovery failed: %v (continuing)", err)
	}

	probe := txn.SubprocessProbe(log.Default().Writer())

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTxnTimeout)
	defer cancel()

	txnID := fmt.Sprintf("bootstrap-%d", time.Now().Unix())
	committed, runErr := manager.Run(ctx, txnID, txn.KindConfig, txn.Candidate{Config: []byte(yaml)}, probe)
	if !committed {
		return fmt.Errorf("bootstrap probe rejected the config: %w", runErr)
	}
	log.Printf("bootstrap: config committed at %s", configPath)
	return nil
}

// tryReadIMEI fetches the modem's IMEI via mmcli with a timeout. Returns "" if
// the modem is unavailable; the caller falls back to MDB serial.
func tryReadIMEI(timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	info, err := modeminfo.Fetch(ctx)
	if err != nil {
		log.Printf("bootstrap: modem IMEI unavailable: %v", err)
		return ""
	}
	return info.EquipmentID
}

// tryReadSerials reads the MDB serial from /sys (always works) and the DBC
// serial from local Redis if available (informational; can be empty).
func tryReadSerials() (mdbReal, dbcReal string) {
	if _, real, err := utils.ReadMdbSerialNumbers(); err == nil {
		mdbReal = real
	} else {
		log.Printf("bootstrap: MDB serial unreadable: %v", err)
	}
	// DBC serial we leave blank for now — it lives in the Redis system hash
	// populated by version-service / dbc-dispatcher; on a fresh install
	// those may not be populated yet. The bootstrap endpoint accepts dbc_serial
	// optionally.
	return
}

// detectPlatform reads /etc/os-release. The bootstrap endpoint uses this to
// pick LibreScoot vs stock paths in the generated config.
func detectPlatform() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "stock"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "ID=") {
			continue
		}
		id := strings.TrimPrefix(line, "ID=")
		id = strings.Trim(id, "\"'")
		if strings.HasPrefix(id, "librescoot") {
			return "librescoot"
		}
		return id
	}
	return "stock"
}

func postBootstrap(baseURL, token, imei, mdbSerial, dbcSerial, softwareVersion string) (yamlBody string, scooterID int, status string, err error) {
	body := map[string]any{
		"imei":             imei,
		"mdb_serial":       mdbSerial,
		"dbc_serial":       dbcSerial,
		"software_version": softwareVersion,
		"platform":         detectPlatform(),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", 0, "", fmt.Errorf("marshal bootstrap body: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + "/api/v1/scooters/bootstrap"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	httpClient := &http.Client{Timeout: bootstrapHTTPTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, "", fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Status     string `json:"status"`
		Message    string `json:"message"`
		ScooterID  int    `json:"scooter_id"`
		ConfigYAML string `json:"config_yaml"`
		Error      struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(respBody, &parsed)

	if resp.StatusCode >= 400 {
		switch {
		case parsed.Error.Message != "":
			return "", 0, "", fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, parsed.Error.Code, parsed.Error.Message)
		case parsed.Message != "":
			return "", 0, "", fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, parsed.Status, parsed.Message)
		default:
			return "", 0, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}
	}

	if parsed.ConfigYAML == "" {
		return "", 0, "", fmt.Errorf("bootstrap response missing config_yaml (status=%s)", parsed.Status)
	}
	return parsed.ConfigYAML, parsed.ScooterID, parsed.Status, nil
}
