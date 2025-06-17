package commands

import (
	"encoding/json"
	"fmt"
	"log"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	configPkg "radio-gaga/internal/config"
	"radio-gaga/internal/models"
)

// CommandHandlerClient interface for dependency injection
type ConfigCommandHandlerClient interface {
	SendCommandResponse(requestID, status, errorMsg string)
	GetCommandParam(cmd, param string, defaultValue interface{}) interface{}
	CleanRetainedMessage(topic string) error
	PublishTelemetryData(current *models.TelemetryData) error
	GetConfigPath() string
}

// HandleConfigGetCommand handles the config:get command
func HandleConfigGetCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]interface{}, requestID string) error {
	// Get the field to retrieve (optional - if not specified, return entire config)
	field, hasField := params["field"].(string)

	var result interface{}
	var err error

	if hasField {
		// Get specific field
		result, err = configPkg.GetConfigField(config, field)
		if err != nil {
			return fmt.Errorf("failed to get field '%s': %v", field, err)
		}
	} else {
		// Return entire configuration
		result = config
	}

	// Send response with configuration data
	response := map[string]interface{}{
		"status":     "success",
		"config":     result,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal config get response: %v", err)
		return fmt.Errorf("failed to marshal response: %v", err)
	}

	// Send response on the data topic
	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish response: %v", token.Error())
	}

	log.Printf("Config get response sent: %s", string(responseJSON))
	return nil
}

// HandleConfigSetCommand handles the config:set command
func HandleConfigSetCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]interface{}, requestID string) error {
	// Extract field and value from params
	field, ok := params["field"].(string)
	if !ok {
		return fmt.Errorf("missing or invalid 'field' parameter")
	}

	value, ok := params["value"]
	if !ok {
		return fmt.Errorf("missing 'value' parameter")
	}

	// Apply the update to the configuration
	if err := configPkg.SetConfigField(config, field, value); err != nil {
		return fmt.Errorf("failed to set field '%s': %v", field, err)
	}

	log.Printf("Set config field '%s' to: %v", field, value)

	// Send success response
	response := map[string]interface{}{
		"status":     "success",
		"message":    "Configuration field updated successfully",
		"field":      field,
		"value":      value,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal config set response: %v", err)
		return nil // Don't fail the command if response marshaling fails
	}

	// Send response on the data topic
	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish config set response: %v", token.Error())
		return nil // Don't fail the command if response publishing fails
	}

	log.Printf("Config set response sent: %s", string(responseJSON))
	return nil
}

// HandleConfigSaveCommand handles the config:save command
func HandleConfigSaveCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, requestID string) error {
	// Get the config path from the client implementation
	configPath := client.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("config file path not available")
	}

	// Save the configuration to file
	if err := configPkg.SaveConfig(config, configPath); err != nil {
		return fmt.Errorf("failed to save configuration: %v", err)
	}

	log.Printf("Configuration saved to %s", configPath)

	// Send success response
	response := map[string]interface{}{
		"status":     "success",
		"message":    "Configuration saved successfully",
		"path":       configPath,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal config save response: %v", err)
		return nil // Don't fail the command if response marshaling fails
	}

	// Send response on the data topic
	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish config save response: %v", token.Error())
		return nil // Don't fail the command if response publishing fails
	}

	log.Printf("Config save response sent: %s", string(responseJSON))
	return nil
}

// HandleConfigDelCommand handles the config:del command
func HandleConfigDelCommand(client ConfigCommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]interface{}, requestID string) error {
	// Extract field from params
	field, ok := params["field"].(string)
	if !ok {
		return fmt.Errorf("missing or invalid 'field' parameter")
	}

	// Get the current value before deletion for the response
	currentValue, err := configPkg.GetConfigField(config, field)
	if err != nil {
		return fmt.Errorf("failed to get current value of field '%s': %v", field, err)
	}

	// Delete the field (set to zero value)
	if err := configPkg.DeleteConfigField(config, field); err != nil {
		return fmt.Errorf("failed to delete field '%s': %v", field, err)
	}

	log.Printf("Deleted config field '%s' (was: %v)", field, currentValue)

	// Send success response
	response := map[string]interface{}{
		"status":         "success",
		"message":        "Configuration field deleted successfully",
		"field":          field,
		"previous_value": currentValue,
		"request_id":     requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal config del response: %v", err)
		return nil // Don't fail the command if response marshaling fails
	}

	// Send response on the data topic
	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish config del response: %v", token.Error())
		return nil // Don't fail the command if response publishing fails
	}

	log.Printf("Config del response sent: %s", string(responseJSON))
	return nil
}
