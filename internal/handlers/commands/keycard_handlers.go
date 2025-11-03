package commands

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
)

// CommandHandlerClient interface for dependency injection
type CommandHandlerClient interface {
	SendCommandResponse(requestID, status, errorMsg string)
	GetCommandParam(cmd, param string, defaultValue interface{}) interface{}
	CleanRetainedMessage(topic string) error
	PublishTelemetryData(current *models.TelemetryData) error
}

// getKeycardPaths returns the appropriate file paths for keycard data based on the system
func getKeycardPaths() (authorizedPath, masterPath string) {
	// Check if this is Librescoot (has /data/keycard directory)
	if _, err := os.Stat("/data/keycard"); err == nil {
		return "/data/keycard/authorized_uids.txt", "/data/keycard/master_uids.txt"
	}
	// Default to patched stock paths
	return "/etc/reunu-keycard/authorized_uids.txt", "/etc/reunu-keycard/master_uids.txt"
}

// readKeycardFile reads a keycard file and returns the UIDs as a slice
func readKeycardFile(filePath string) ([]string, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read keycard file %s: %v", filePath, err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var uids []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			uids = append(uids, line)
		}
	}
	return uids, nil
}

// writeKeycardFile writes UIDs to a keycard file
func writeKeycardFile(filePath string, uids []string) error {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	content := strings.Join(uids, "\n")
	if len(uids) > 0 {
		content += "\n"
	}

	return ioutil.WriteFile(filePath, []byte(content), 0644)
}

// HandleKeycardsListCommand handles the keycards:list command
func HandleKeycardsListCommand(client CommandHandlerClient, mqttClient mqtt.Client, config *models.Config, requestID string) error {
	authorizedPath, _ := getKeycardPaths()

	uids, err := readKeycardFile(authorizedPath)
	if err != nil {
		return err
	}

	response := map[string]interface{}{
		"type":       "keycards",
		"subcommand": "list",
		"uids":       uids,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	token := mqttClient.Publish(topic, 1, false, responseJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		return fmt.Errorf("failed to publish response: %v", token.Error())
	}

	return nil
}

// HandleKeycardsAddCommand handles the keycards:add command
func HandleKeycardsAddCommand(params map[string]interface{}) error {
	uid, ok := params["uid"].(string)
	if !ok || uid == "" {
		return fmt.Errorf("uid parameter is required")
	}

	authorizedPath, _ := getKeycardPaths()

	uids, err := readKeycardFile(authorizedPath)
	if err != nil {
		return err
	}

	// Check if UID already exists
	for _, existingUID := range uids {
		if existingUID == uid {
			return fmt.Errorf("uid %s already exists", uid)
		}
	}

	// Add new UID
	uids = append(uids, uid)

	return writeKeycardFile(authorizedPath, uids)
}

// HandleKeycardsDeleteCommand handles the keycards:delete command
func HandleKeycardsDeleteCommand(params map[string]interface{}) error {
	uid, ok := params["uid"].(string)
	if !ok || uid == "" {
		return fmt.Errorf("uid parameter is required")
	}

	authorizedPath, _ := getKeycardPaths()

	uids, err := readKeycardFile(authorizedPath)
	if err != nil {
		return err
	}

	// Find and remove the UID
	var newUIDs []string
	found := false
	for _, existingUID := range uids {
		if existingUID != uid {
			newUIDs = append(newUIDs, existingUID)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("uid %s not found", uid)
	}

	return writeKeycardFile(authorizedPath, newUIDs)
}

// HandleKeycardsMasterKeyGetCommand handles the keycards:master_key:get command
func HandleKeycardsMasterKeyGetCommand(client CommandHandlerClient, mqttClient mqtt.Client, config *models.Config, requestID string) error {
	_, masterPath := getKeycardPaths()

	uids, err := readKeycardFile(masterPath)
	if err != nil {
		return err
	}

	var masterUID string
	if len(uids) > 0 {
		masterUID = uids[0] // Take the first (and typically only) master UID
	}

	response := map[string]interface{}{
		"type":       "keycards",
		"subcommand": "master_key_get",
		"master_uid": masterUID,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	token := mqttClient.Publish(topic, 1, false, responseJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		return fmt.Errorf("failed to publish response: %v", token.Error())
	}

	return nil
}

// HandleKeycardsMasterKeySetCommand handles the keycards:master_key:set command
func HandleKeycardsMasterKeySetCommand(params map[string]interface{}) error {
	uid, ok := params["uid"].(string)
	if !ok || uid == "" {
		return fmt.Errorf("uid parameter is required")
	}

	_, masterPath := getKeycardPaths()

	// Write single UID to master file (replacing any existing content)
	return writeKeycardFile(masterPath, []string{uid})
}
