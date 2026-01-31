package commands

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"

	configPkg "radio-gaga/internal/config"
	"radio-gaga/internal/models"
)

// UpdateCACertClient extends ConfigCommandHandlerClient with reconnection capability
type UpdateCACertClient interface {
	ConfigCommandHandlerClient
	RequestReconnect()
}

// HandleUpdateCACertCommand handles the update_ca_cert command.
// It validates the provided PEM certificate, updates the config, saves to disk,
// and requests an MQTT reconnection with the new CA cert.
func HandleUpdateCACertCommand(client UpdateCACertClient, config *models.Config, params map[string]interface{}, requestID string) error {
	certPEM, ok := params["cert"].(string)
	if !ok || certPEM == "" {
		return fmt.Errorf("missing or empty 'cert' parameter")
	}

	// Parse and validate the PEM certificate
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return fmt.Errorf("failed to decode PEM block")
	}

	newCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse X.509 certificate: %v", err)
	}

	if !newCert.IsCA {
		return fmt.Errorf("certificate is not a CA certificate (BasicConstraints CA:FALSE)")
	}

	// Log old cert details if we have one
	if config.MQTT.CACertEmbedded != "" {
		if oldBlock, _ := pem.Decode([]byte(config.MQTT.CACertEmbedded)); oldBlock != nil {
			if oldCert, err := x509.ParseCertificate(oldBlock.Bytes); err == nil {
				log.Printf("Old CA cert: subject=%s, expires=%s", oldCert.Subject, oldCert.NotAfter)
			}
		}
	}

	log.Printf("New CA cert: subject=%s, expires=%s", newCert.Subject, newCert.NotAfter)

	// Update config: set embedded cert, clear file path so embedded takes precedence
	config.MQTT.CACertEmbedded = certPEM
	config.MQTT.CACert = ""

	// Save config to disk
	configPath := client.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("config file path not available")
	}

	if err := configPkg.SaveConfig(config, configPath); err != nil {
		return fmt.Errorf("failed to save configuration: %v", err)
	}

	log.Printf("CA certificate updated and config saved to %s", configPath)

	// Request async reconnection (response is sent first, then reconnect happens)
	client.RequestReconnect()

	return nil
}
