package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
	"radio-gaga/internal/txn"
)

// Defaults for the txn:replace command.
const (
	txnDefaultDeadline = 60 * time.Second
	txnMaxDeadline     = 5 * time.Minute
)

// HandleTxnReplaceCommand handles a `txn:replace` MQTT command. For step 1
// scope this only handles config-only swaps: the operator (Sunshine) sends a
// full YAML config, radio-gaga stages it alongside the live one, spawns a
// child probe with the new config, and either commits (atomic rename) or
// rolls back. The live process keeps running its current config throughout —
// after a successful commit the new config takes effect on the next process
// restart (systemd respawn or a future explicit reload).
//
// Params:
//
//	{
//	  "txn_id":           "<unique id, surfaced in response>",
//	  "config_yaml":      "<full config as a YAML string>",
//	  "deadline_seconds": <int, optional, default 60, capped at 5min>,
//	  "kind":             "config" (required to make future binary support
//	                      explicit; only "config" is accepted today)
//	}
//
// Response (on the data topic, like other config:* commands):
//
//	success → { status: "success", txn_id, committed: true }
//	rollback → { status: "rollback", txn_id, committed: false, reason: "..." }
//	error → { status: "error", txn_id, error: "..." } (handler-side failure
//	        before/after the txn machinery — distinct from a clean rollback)
func HandleTxnReplaceCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]any, requestID string) error {
	txnID, _ := params["txn_id"].(string)
	if txnID == "" {
		return fmt.Errorf("txn_id is required")
	}

	kind, _ := params["kind"].(string)
	if kind == "" {
		kind = string(txn.KindConfig)
	}
	if kind != string(txn.KindConfig) {
		return fmt.Errorf("kind %q not supported yet (only %q is)", kind, txn.KindConfig)
	}

	candidate, _ := params["config_yaml"].(string)
	if candidate == "" {
		return fmt.Errorf("config_yaml is required for kind=%s", kind)
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

	manager := &txn.Manager{
		LiveConfigPath: configPath,
		PendingPath:    filepath.Join(filepath.Dir(configPath), ".txn-pending.json"),
		Logger:         log.Default(),
	}

	probe := txn.SubprocessProbe(exePath, log.Default().Writer())

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	committed, runErr := manager.Run(ctx, txnID, txn.KindConfig, []byte(candidate), probe)

	resp := map[string]any{
		"type":       "txn",
		"txn_id":     txnID,
		"committed":  committed,
		"request_id": requestID,
	}
	switch {
	case runErr == nil && committed:
		resp["status"] = "success"
		log.Printf("txn:replace %s committed", txnID)
	case runErr != nil && !committed:
		resp["status"] = "rollback"
		resp["reason"] = runErr.Error()
		log.Printf("txn:replace %s rolled back: %v", txnID, runErr)
	default:
		// runErr != nil but committed — only happens if the post-rename
		// marker write fails. Surface as a warning; live state is the new
		// config, but the operator should investigate.
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
	return nil
}
