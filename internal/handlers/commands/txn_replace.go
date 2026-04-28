package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
	"radio-gaga/internal/txn"
)

// Defaults for the txn:replace command.
const (
	txnDefaultDeadline   = 60 * time.Second
	txnMaxDeadline       = 5 * time.Minute
	txnBinaryDownloadTO  = 60 * time.Second
	txnPostCommitGrace   = 2 * time.Second
	txnBinaryMaxBytes    = 64 * 1024 * 1024 // 64 MiB cap
)

// HandleTxnReplaceCommand handles a `txn:replace` MQTT command. Sunshine sends
// a candidate config and/or binary URL; radio-gaga stages it alongside the
// live one, spawns a child probe, and either commits (atomic rename) or
// rolls back. For binary swap or combined kinds, the live process exits via
// SIGTERM after the response is published so systemd's Restart=always picks
// up the new binary.
//
// Params:
//
//	{
//	  "txn_id":           "<unique id, surfaced in response>",
//	  "kind":             "config" | "binary" | "both"  (defaults to "config")
//	  "config_yaml":      "<YAML string>"               (required for config / both)
//	  "binary_url":       "https://.../radio-gaga"      (required for binary / both)
//	  "binary_sha256":    "<hex>"                       (optional but recommended)
//	  "deadline_seconds": <int, optional, default 60, capped at 5min>
//	}
//
// Response (on the data topic):
//
//	success  → { status: "success",  type: "txn", txn_id, committed: true,
//	              kind, restart_pending: bool }
//	rollback → { status: "rollback", type: "txn", txn_id, committed: false, reason }
//	warning  → { status: "warning",  type: "txn", txn_id, committed: true,  reason }
//
// `restart_pending: true` tells the operator that the scoot will SIGTERM
// itself within ~2s of the response being acked so systemd respawns into the
// new binary. Reconnects show up shortly afterward.
func HandleTxnReplaceCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]any, requestID string) error {
	txnID, _ := params["txn_id"].(string)
	if txnID == "" {
		return fmt.Errorf("txn_id is required")
	}

	kindStr, _ := params["kind"].(string)
	if kindStr == "" {
		kindStr = string(txn.KindConfig)
	}
	kind := txn.Kind(kindStr)
	if kind != txn.KindConfig && kind != txn.KindBinary && kind != txn.KindBoth {
		return fmt.Errorf("kind %q not supported (use config, binary, or both)", kindStr)
	}

	configYAML, _ := params["config_yaml"].(string)
	binaryURL, _ := params["binary_url"].(string)
	binarySha256, _ := params["binary_sha256"].(string)

	if (kind == txn.KindConfig || kind == txn.KindBoth) && configYAML == "" {
		return fmt.Errorf("config_yaml is required for kind=%s", kind)
	}
	if (kind == txn.KindBinary || kind == txn.KindBoth) && binaryURL == "" {
		return fmt.Errorf("binary_url is required for kind=%s", kind)
	}

	deadline := txnDefaultDeadline
	if v, ok := params["deadline_seconds"].(float64); ok && v > 0 {
		deadline = min(time.Duration(v)*time.Second, txnMaxDeadline)
	}

	configPath := client.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("config path unavailable; cannot run a config txn")
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	candidate := txn.Candidate{}
	if kind == txn.KindConfig || kind == txn.KindBoth {
		candidate.Config = []byte(configYAML)
	}
	if kind == txn.KindBinary || kind == txn.KindBoth {
		bin, err := downloadBinary(binaryURL, binarySha256)
		if err != nil {
			return fmt.Errorf("download binary: %w", err)
		}
		candidate.Binary = bin
	}

	manager := &txn.Manager{
		LiveConfigPath: configPath,
		LiveBinaryPath: exePath,
		PendingPath:    filepath.Join(filepath.Dir(configPath), ".txn-pending.json"),
		Logger:         log.Default(),
	}

	probe := txn.SubprocessProbe(log.Default().Writer())

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	committed, runErr := manager.Run(ctx, txnID, kind, candidate, probe)

	restartPending := committed && (kind == txn.KindBinary || kind == txn.KindBoth)

	resp := map[string]any{
		"type":            "txn",
		"txn_id":          txnID,
		"kind":            string(kind),
		"committed":       committed,
		"restart_pending": restartPending,
		"request_id":      requestID,
	}
	switch {
	case runErr == nil && committed:
		resp["status"] = "success"
		log.Printf("txn:replace %s committed (kind=%s, restart_pending=%v)", txnID, kind, restartPending)
	case runErr != nil && !committed:
		resp["status"] = "rollback"
		resp["reason"] = runErr.Error()
		log.Printf("txn:replace %s rolled back: %v", txnID, runErr)
	default:
		resp["status"] = "warning"
		resp["reason"] = runErr.Error()
		log.Printf("txn:replace %s committed with warning: %v", txnID, runErr)
	}

	respJSON, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Failed to marshal txn:replace response: %v", err)
		return nil
	}

	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	token := mqttClient.Publish(topic, 1, false, respJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		log.Printf("Failed to publish txn:replace response: %v", token.Error())
	}

	// For binary swap, send self SIGTERM so systemd respawns into the new
	// binary. The post-commit grace gives the response time to actually
	// leave the wire (we already waited for PUBACK above; this is belt +
	// suspenders for whatever else may be flushing).
	if restartPending {
		go func(id string) {
			time.Sleep(txnPostCommitGrace)
			log.Printf("txn:replace %s: requesting clean restart for binary swap", id)
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				log.Printf("txn:replace %s: SIGTERM self failed: %v", id, err)
			}
		}(txnID)
	}
	return nil
}

// downloadBinary GETs the binary from URL with a size cap, optionally
// verifies the SHA-256, and returns the bytes. Caller writes them into the
// txn staging path with executable mode.
func downloadBinary(url, expectedSHA256 string) ([]byte, error) {
	c := &http.Client{Timeout: txnBinaryDownloadTO}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, txnBinaryMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > txnBinaryMaxBytes {
		return nil, fmt.Errorf("binary too large (cap %d bytes)", txnBinaryMaxBytes)
	}

	if expectedSHA256 != "" {
		got := sha256.Sum256(body)
		gotHex := hex.EncodeToString(got[:])
		want := strings.TrimPrefix(strings.ToLower(expectedSHA256), "sha256:")
		if gotHex != want {
			return nil, fmt.Errorf("sha256 mismatch: got %s want %s", gotHex, want)
		}
	}

	return body, nil
}
