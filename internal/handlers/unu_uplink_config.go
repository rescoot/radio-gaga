package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
)

// ReconfigureUnuUplink reconfigures the mdb-uplink-service to use the same MQTT broker as radio-gaga
func ReconfigureUnuUplink(ctx context.Context, redisClient *redis.Client, config *models.Config) error {
	if !config.UnuUplink.Enabled {
		log.Println("unu-uplink reconfiguration is disabled, skipping")
		return nil
	}

	// Extract target MQTT URL from radio-gaga's config (remove ssl:// prefix)
	targetMQTTURL := strings.TrimPrefix(config.MQTT.BrokerURL, "ssl://")
	targetMQTTURL = strings.TrimPrefix(targetMQTTURL, "mqtt://")

	// Check if current MQTT URL is pointing to defunct unu servers
	currentURL, err := redisClient.HGet(ctx, "settings", "cloud:mqtt-url").Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("failed to get current cloud:mqtt-url: %v", err)
	}

	// If already configured to point to the same server, skip
	if currentURL == targetMQTTURL {
		log.Printf("unu-uplink already configured to point to %s, skipping reconfiguration", currentURL)
		return nil
	}

	// Check if current URL points to defunct unu servers
	isDefunct := strings.Contains(currentURL, "unumotors.com") ||
	             strings.Contains(currentURL, "zeus-iot") ||
	             strings.Contains(currentURL, "cloud-iot")

	if !isDefunct && currentURL != "" {
		log.Printf("unu-uplink is configured to custom server (%s), skipping automatic reconfiguration", currentURL)
		return nil
	}

	log.Printf("Reconfiguring unu-uplink from '%s' to '%s'", currentURL, targetMQTTURL)

	// Update Redis settings for mdb-uplink-service
	err = redisClient.HSet(ctx, "settings", "cloud:mqtt-url", targetMQTTURL).Err()
	if err != nil {
		return fmt.Errorf("failed to set cloud:mqtt-url: %v", err)
	}

	// Handle CA certificate - either use provided path or write embedded cert to file
	caCertPath, err := ensureCACertificate(config)
	if err != nil {
		return fmt.Errorf("failed to ensure CA certificate: %v", err)
	}

	if caCertPath != "" {
		err = redisClient.HSet(ctx, "settings", "cloud:mqtt-ca", caCertPath).Err()
		if err != nil {
			return fmt.Errorf("failed to set cloud:mqtt-ca: %v", err)
		}
		log.Printf("Updated MQTT CA certificate path to: %s", caCertPath)
	}

	log.Println("Successfully updated unu-uplink Redis configuration")

	// Restart mdb-uplink-service (required for config changes to take effect)
	err = restartUnuUplinkService()
	if err != nil {
		log.Printf("Warning: Failed to restart mdb-uplink-service: %v", err)
		log.Println("You may need to manually restart the service for changes to take effect")
	} else {
		log.Println("Successfully restarted mdb-uplink-service")
	}

	return nil
}

// ensureCACertificate ensures the CA certificate is available as a file for unu-uplink
// Returns the path to the certificate file, or empty string if no certificate is configured
func ensureCACertificate(config *models.Config) (string, error) {
	// Priority 1: If radio-gaga is using an embedded certificate, write it to a file for unu-uplink
	if config.MQTT.CACertEmbedded != "" {
		certPath := "/etc/keys/sunshine-mqtt-ca-unu.crt"

		// Ensure the directory exists
		certDir := filepath.Dir(certPath)
		if err := os.MkdirAll(certDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create certificate directory %s: %v", certDir, err)
		}

		// Write the embedded certificate to file
		err := os.WriteFile(certPath, []byte(config.MQTT.CACertEmbedded), 0644)
		if err != nil {
			return "", fmt.Errorf("failed to write CA certificate to %s: %v", certPath, err)
		}

		log.Printf("Wrote embedded CA certificate to %s for unu-uplink", certPath)
		return certPath, nil
	}

	// Priority 2: If radio-gaga is using a certificate file path, reuse it
	if config.MQTT.CACert != "" {
		// Verify the file exists
		if _, err := os.Stat(config.MQTT.CACert); err != nil {
			return "", fmt.Errorf("radio-gaga CA certificate file does not exist: %s", config.MQTT.CACert)
		}
		log.Printf("Reusing radio-gaga CA certificate for unu-uplink: %s", config.MQTT.CACert)
		return config.MQTT.CACert, nil
	}

	// No certificate configured
	log.Println("No CA certificate configured for unu-uplink")
	return "", nil
}

// restartUnuUplinkService attempts to restart the mdb-uplink-service systemd unit
func restartUnuUplinkService() error {
	// Try common service names
	serviceNames := []string{
		"mdb-uplink-service",
		"mdb-uplink",
		"unu-uplink",
	}

	for _, serviceName := range serviceNames {
		// Check if service exists
		checkCmd := exec.Command("systemctl", "status", serviceName)
		if err := checkCmd.Run(); err != nil {
			continue // Service doesn't exist, try next
		}

		// Service exists, restart it
		log.Printf("Restarting %s...", serviceName)
		restartCmd := exec.Command("systemctl", "restart", serviceName)
		output, err := restartCmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to restart %s: %v, output: %s", serviceName, err, string(output))
		}
		return nil
	}

	return fmt.Errorf("no unu-uplink service found (tried: %s)", strings.Join(serviceNames, ", "))
}
