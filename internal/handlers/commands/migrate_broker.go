package commands

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"

	configPkg "radio-gaga/internal/config"
	"radio-gaga/internal/models"
)

// HandleMigrateBrokerCommand atomically points radio-gaga at a new MQTT broker
// (and optionally a new embedded CA), persists the config, and triggers a single
// clean reconnect. Doing both in one command means broker_url and CA never
// disagree mid-flight, and the single RequestReconnect (serialised by
// reconnectMu) cannot orphan a client.
//
// Params:
//
//	broker_url string  (required) e.g. "ssl://mqtt2.sunshine.rescoot.org:8883"
//	ca_cert    string  (optional) PEM of the CA for the new broker; when set it
//	                   replaces the embedded CA and clears any CA file path.
func HandleMigrateBrokerCommand(client UpdateCACertClient, config *models.Config, params map[string]interface{}, requestID string) error {
	brokerURL, ok := params["broker_url"].(string)
	if !ok || brokerURL == "" {
		return fmt.Errorf("missing or empty 'broker_url' parameter")
	}

	// Validate the CA up front (if provided) so we never half-apply.
	caPEM, hasCA := params["ca_cert"].(string)
	if hasCA && caPEM != "" {
		block, _ := pem.Decode([]byte(caPEM))
		if block == nil {
			return fmt.Errorf("failed to decode CA PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("failed to parse CA certificate: %v", err)
		}
		if !cert.IsCA {
			return fmt.Errorf("certificate is not a CA certificate (BasicConstraints CA:FALSE)")
		}
		log.Printf("migrate_broker: new CA subject=%s expires=%s", cert.Subject, cert.NotAfter)
	}

	configPath := client.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("config file path not available")
	}

	// Capture old values so we can revert in memory if the save fails, avoiding a
	// persisted/in-memory split that a later reconnect would act on.
	oldURL := config.MQTT.BrokerURL
	oldEmbedded := config.MQTT.CACertEmbedded
	oldCAFile := config.MQTT.CACert

	config.MQTT.BrokerURL = brokerURL
	if hasCA && caPEM != "" {
		config.MQTT.CACertEmbedded = caPEM
		config.MQTT.CACert = ""
	}

	if err := configPkg.SaveConfig(config, configPath); err != nil {
		config.MQTT.BrokerURL = oldURL
		config.MQTT.CACertEmbedded = oldEmbedded
		config.MQTT.CACert = oldCAFile
		return fmt.Errorf("failed to save configuration: %v", err)
	}

	log.Printf("migrate_broker: broker_url -> %s, config saved to %s", brokerURL, configPath)

	// Single clean reconnect; the dispatcher sends the success response first.
	client.RequestReconnect()
	return nil
}
