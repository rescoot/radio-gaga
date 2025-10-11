package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
	"radio-gaga/internal/telemetry"
	"radio-gaga/internal/utils"
)

// ScooterMQTTClient manages the MQTT and Redis connections
type ScooterMQTTClient struct {
	config           *models.Config
	configPath       string
	mqttClient       mqtt.Client
	redisClient      *redis.Client
	ctx              context.Context
	cancel           context.CancelFunc
	version          string
	serviceStartTime time.Time
	wg               sync.WaitGroup
	bufferMu         sync.Mutex // Protects telemetry buffer operations
}

// parseOSRelease extracts ID and VERSION_ID from /etc/os-release content
func parseOSRelease(content string) (id, versionID string) {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "ID=") {
			id = strings.Trim(strings.TrimPrefix(line, "ID="), "\"'")
		} else if strings.HasPrefix(line, "VERSION_ID=") {
			versionID = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"'")
		}
	}
	return id, versionID
}

// NewScooterMQTTClient creates a new MQTT client
func NewScooterMQTTClient(config *models.Config, configPath string, version string) (*ScooterMQTTClient, error) {
	ctx, cancel := context.WithCancel(context.Background())

	redisOptions, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid redis URL: %v", err)
	}

	redisClient := redis.NewClient(redisOptions)

	// Test Redis connection
	_, err = redisClient.Ping(ctx).Result()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("redis connection failed: %v", err)
	}

	// --- MDB Flavor and Version Handling ---
	// Determine and store MDB flavor (SUN-47)
	mdbHostname, err := os.Hostname()
	if err != nil {
		log.Printf("Warning: Failed to get MDB hostname: %v", err)
		redisClient.HSet(ctx, "system", "mdb-flavor", "unknown_error").Err()
	} else {
		var mdbFlavor string
		if strings.HasPrefix(mdbHostname, "librescoot-") {
			mdbFlavor = "librescoot"
		} else if strings.HasPrefix(mdbHostname, "mdb-") {
			mdbFlavor = "stock"
		} else {
			mdbFlavor = "unknown"
			log.Printf("Unrecognized MDB hostname format: %s", mdbHostname)
		}
		if storeErr := redisClient.HSet(ctx, "system", "mdb-flavor", mdbFlavor).Err(); storeErr != nil {
			log.Printf("Failed to store MDB flavor '%s' in Redis: %v", mdbFlavor, storeErr)
		} else {
			log.Printf("Stored MDB flavor as '%s' based on hostname '%s'", mdbFlavor, mdbHostname)
		}
	}

	// MDB Version
	log.Println("Checking MDB version information at startup...")
	mdbVersionInfo, err := redisClient.HGetAll(ctx, "version:mdb").Result()
	mdbVersion := ""
	if err == nil && mdbVersionInfo["version_id"] != "" {
		mdbVersion = mdbVersionInfo["version_id"]
		log.Printf("Found MDB version_id in version:mdb Redis hash: %s", mdbVersion)
	} else {
		if err != redis.Nil && err != nil {
			log.Printf("Error reading version:mdb from Redis: %v. Falling back to local os-release.", err)
		} else {
			log.Println("MDB version_id not found in version:mdb Redis hash or hash missing. Reading local /etc/os-release.")
		}
		mdbOsReleaseBytes, readErr := os.ReadFile("/etc/os-release")
		if readErr != nil {
			log.Printf("Failed to read local /etc/os-release for MDB version: %v", readErr)
			mdbVersion = "unknown_os_release_read_error"
		} else {
			mdbOsID, mdbOsVersionID := parseOSRelease(string(mdbOsReleaseBytes))

			if mdbOsVersionID == "" {
				log.Println("Could not find VERSION_ID in MDB /etc/os-release")
				mdbVersion = "unknown_os_release_parse_error"
			} else {
				mdbVersion = mdbOsVersionID
				fieldsToSet := map[string]interface{}{"version_id": mdbOsVersionID}
				if mdbOsID != "" {
					fieldsToSet["id"] = mdbOsID
				} else {
					log.Println("Could not find ID in MDB /etc/os-release")
				}
				if pipeErr := redisClient.HSet(ctx, "version:mdb", fieldsToSet).Err(); pipeErr != nil {
					log.Printf("Failed to populate version:mdb Redis hash: %v", pipeErr)
				} else {
					log.Printf("Populated version:mdb Redis hash with ID: %s, VersionID: %s", mdbOsID, mdbOsVersionID)
				}
			}
		}
	}
	if storeErr := redisClient.HSet(ctx, "system", "mdb-version", mdbVersion).Err(); storeErr != nil {
		log.Printf("Failed to store MDB version '%s' in Redis: %v", mdbVersion, storeErr)
	} else {
		log.Printf("Stored MDB version as '%s' in system hash", mdbVersion)
	}

	// --- Initial DBC Info (from Redis only) ---
	log.Println("Checking initial DBC information from Redis at startup...")
	dbcVersionInfo, err := redisClient.HGetAll(ctx, "version:dbc").Result()
	if err == nil && dbcVersionInfo["version_id"] != "" && dbcVersionInfo["id"] != "" {
		dbcRedisVersion := dbcVersionInfo["version_id"]
		dbcRedisID := dbcVersionInfo["id"]
		log.Printf("Found DBC info in version:dbc Redis hash - ID: %s, Version: %s", dbcRedisID, dbcRedisVersion)

		var dbcFlavor string
		if strings.Contains(dbcRedisID, "librescoot") {
			dbcFlavor = "librescoot"
		} else if strings.Contains(dbcRedisID, "scooteros") {
			dbcFlavor = "stock"
		} else {
			dbcFlavor = dbcRedisID
			log.Printf("Unrecognized DBC ID from version:dbc Redis hash: %s", dbcRedisID)
		}

		if storeErr := redisClient.HSet(ctx, "system", "dbc-flavor", dbcFlavor).Err(); storeErr != nil {
			log.Printf("Failed to store initial DBC flavor '%s' from Redis: %v", dbcFlavor, storeErr)
		} else {
			log.Printf("Stored initial DBC flavor as '%s' from Redis", dbcFlavor)
		}
		if storeErr := redisClient.HSet(ctx, "system", "dbc-version", dbcRedisVersion).Err(); storeErr != nil {
			log.Printf("Failed to store initial DBC version '%s' from Redis: %v", dbcRedisVersion, storeErr)
		} else {
			log.Printf("Stored initial DBC version as '%s' from Redis", dbcRedisVersion)
		}
	} else {
		if err != redis.Nil && err != nil {
			log.Printf("Error reading version:dbc from Redis at startup: %v. DBC info will be fetched later.", err)
		} else {
			log.Println("Initial DBC info not found or incomplete in version:dbc Redis hash. Will be fetched when dashboard is ready.")
		}
	}

	// Check Redis if values aren't already set in config
	if config.MQTT.BrokerURL == "" {
		if brokerURL, err := redisClient.HGet(ctx, "settings", "cloud:mqtt-url").Result(); err == nil && brokerURL != "" {
			log.Printf("Using MQTT broker URL from Redis: %s", brokerURL)
			config.MQTT.BrokerURL = brokerURL
		} else {
			cancel()
			return nil, fmt.Errorf("MQTT broker URL not set and not found in Redis")
		}
	}

	if config.MQTT.CACert == "" {
		if caCertPath, err := redisClient.HGet(ctx, "settings", "cloud:mqtt-ca").Result(); err == nil && caCertPath != "" {
			log.Printf("Using CA certificate path from Redis: %s", caCertPath)
			config.MQTT.CACert = caCertPath
		}
	}

	keepAlive, err := time.ParseDuration(config.MQTT.KeepAlive)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not parse keepalive interval: %v", err)
	}

	log.Println("Setting initial cloud status to disconnected")
	if err := redisClient.HSet(ctx, "internet", "unu-cloud", "disconnected").Err(); err != nil {
		log.Printf("Failed to set initial unu-cloud status: %v", err)
	}
	if err := redisClient.Publish(ctx, "internet", "unu-cloud").Err(); err != nil {
		log.Printf("Failed to publish initial unu-cloud status: %v", err)
	}

	clientID := fmt.Sprintf("radio-gaga-%s", config.Scooter.Identifier)

	willTopic := fmt.Sprintf("scooters/%s/status", config.Scooter.Identifier)
	willMessage := []byte(`{"status": "disconnected"}`)

	opts := mqtt.NewClientOptions().
		AddBroker(config.MQTT.BrokerURL).
		SetClientID(clientID).
		SetUsername(config.Scooter.Identifier).
		SetPassword(config.Scooter.Token).
		SetKeepAlive(keepAlive).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30*time.Second).
		SetConnectTimeout(30*time.Second).
		SetWriteTimeout(30*time.Second).
		SetPingTimeout(30*time.Second).
		SetCleanSession(false).                           // Maintain session for message queueing
		SetWill(willTopic, string(willMessage), 1, true). // QoS 1 and retained
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("Connection lost: %v", err)
			if err := redisClient.HSet(ctx, "internet", "unu-cloud", "disconnected").Err(); err != nil {
				log.Printf("Failed to set unu-cloud status: %v", err)
			}
			if err := redisClient.Publish(ctx, "internet", "unu-cloud").Err(); err != nil {
				log.Printf("Failed to publish unu-cloud status: %v", err)
			}
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("Connected to MQTT broker at %s", config.MQTT.BrokerURL)

			// Say hello to the cloud
			statusTopic := fmt.Sprintf("scooters/%s/status", config.Scooter.Identifier)
			statusMessage := []byte(`{"status": "connected"}`)
			if token := c.Publish(statusTopic, 1, true, statusMessage); token.Wait() && token.Error() != nil {
				log.Printf("Failed to publish connection status: %v", token.Error())
			}

			// Update cloud status
			if err := redisClient.HSet(ctx, "internet", "unu-cloud", "connected").Err(); err != nil {
				log.Printf("Failed to set unu-cloud status: %v", err)
			}
			if err := redisClient.Publish(ctx, "internet", "unu-cloud").Err(); err != nil {
				log.Printf("Failed to publish unu-cloud status: %v", err)
			}
		})

	if utils.IsTLSURL(config.MQTT.BrokerURL) {
		tlsConfig := new(tls.Config)

		// Check if we have a CA certificate to use
		if config.MQTT.CACertEmbedded != "" {
			// Use the embedded certificate
			log.Printf("Using embedded CA certificate")
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM([]byte(config.MQTT.CACertEmbedded)); !ok {
				cancel()
				return nil, fmt.Errorf("failed to parse embedded CA certificate")
			}
			tlsConfig.RootCAs = caCertPool
		} else if config.MQTT.CACert != "" {
			// Use the certificate from file
			log.Printf("Using CA certificate from file: %s", config.MQTT.CACert)
			caCert, err := os.ReadFile(config.MQTT.CACert)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("failed to read CA certificate: %v", err)
			}

			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
				cancel()
				return nil, fmt.Errorf("failed to parse CA certificate")
			}

			tlsConfig.RootCAs = caCertPool
		}
		opts.SetTLSConfig(tlsConfig)
	}

	mqttClient, err := createMQTTClient(config, opts)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("MQTT connection failed: %v", err)
	}

	serviceStartTime := time.Now().UTC()

	client := &ScooterMQTTClient{
		config:           config,
		configPath:       configPath,
		mqttClient:       mqttClient,
		redisClient:      redisClient,
		ctx:              ctx,
		cancel:           cancel,
		version:          version,
		serviceStartTime: serviceStartTime,
	}


	return client, nil
}

// createMQTTClient creates and connects an MQTT client
func createMQTTClient(config *models.Config, opts *mqtt.ClientOptions) (mqtt.Client, error) {
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		err := token.Error()
		if strings.Contains(err.Error(), "certificate has expired or is not yet valid") {
			log.Printf("Certificate validity period error, attempting NTP sync...")

			// Try NTP sync
			ntpErr := utils.SyncTimeNTP(&config.NTP)
			if ntpErr == nil {
				// Try connecting again after time sync
				if token := client.Connect(); token.Wait() && token.Error() != nil {
					log.Printf("Connection failed after NTP sync: %v, falling back to insecure...", token.Error())
				} else {
					return client, nil
				}
			} else {
				log.Printf("NTP sync failed: %v, falling back to insecure...", ntpErr)
			}

			// If we get here, both normal connection and NTP sync failed
			// Create new client with insecure TLS
			insecureOpts := opts

			var tlsConfig *tls.Config
			var err error

			// Check if we have an embedded certificate or a file path
			if config.MQTT.CACertEmbedded != "" {
				tlsConfig, err = utils.CreateInsecureTLSConfigWithEmbeddedCert(config.MQTT.CACertEmbedded)
			} else {
				tlsConfig, err = utils.CreateInsecureTLSConfig(config.MQTT.CACert)
			}

			if err == nil {
				insecureOpts.SetTLSConfig(tlsConfig)
				insecureClient := mqtt.NewClient(insecureOpts)
				if token := insecureClient.Connect(); token.Wait() && token.Error() != nil {
					return nil, fmt.Errorf("all connection attempts failed, last error: %v", token.Error())
				}
				log.Printf("Warning: Connected with insecure TLS configuration")
				return insecureClient, nil
			} else {
				return nil, fmt.Errorf("failed to create insecure TLS config: %v", err)
			}
		}
		return nil, fmt.Errorf("connection failed: %v", token.Error())
	}
	return client, nil
}

// Start starts the MQTT client and subscribes to command topic
func (s *ScooterMQTTClient) Start() error {
	// Subscribe to command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	if token := s.mqttClient.Subscribe(commandTopic, 1, s.handleCommand); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to commands: %v", token.Error())
	}

	log.Printf("Subscribed to commands channel %s", commandTopic)

	// Initialize telemetry buffer if enabled
	if s.config.Telemetry.Buffer.Enabled {
		log.Printf("Initializing telemetry buffer")
		s.initTelemetryBuffer()
	}

	// Start telemetry and dashboard watcher goroutines
	s.wg.Add(4)
	go s.publishTelemetry()
	go s.watchDashboardStatus()
	go s.watchInternetStatus()
	go s.watchAlarmStatus()

	return nil
}

// Stop stops the MQTT client and closes connections
func (s *ScooterMQTTClient) Stop() {
	// Ensure any buffered telemetry is sent before shutting down
	if s.config.Telemetry.Buffer.Enabled {
		log.Println("Flushing telemetry buffer before shutdown...")
		if err := s.transmitBuffer(); err != nil {
			log.Printf("Error flushing telemetry buffer during shutdown: %v", err)
		} else {
			log.Println("Telemetry buffer flushed.")
		}
	}

	// Unsubscribe from command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	if s.mqttClient.IsConnected() {
		log.Printf("Unsubscribing from %s", commandTopic)
		if token := s.mqttClient.Unsubscribe(commandTopic); token.WaitTimeout(2*time.Second) && token.Error() != nil {
			log.Printf("Error unsubscribing from command topic: %v", token.Error())
		}
	}


	// Cancel context
	log.Println("Cancelling client context...")
	s.cancel()

	log.Println("Waiting for goroutines to finish...")
	s.wg.Wait()

	log.Println("Setting cloud status to disconnected before shutdown")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := s.redisClient.HSet(shutdownCtx, "internet", "unu-cloud", "disconnected").Err(); err != nil {
		log.Printf("Failed to set unu-cloud status on shutdown: %v", err)
	}
	if err := s.redisClient.Publish(shutdownCtx, "internet", "unu-cloud").Err(); err != nil {
		log.Printf("Failed to publish unu-cloud status on shutdown: %v", err)
	}

	if s.mqttClient.IsConnected() {
		log.Println("Disconnecting MQTT client...")
		s.mqttClient.Disconnect(500)
	}

	// Close Redis client
	log.Println("Closing Redis client...")
	if err := s.redisClient.Close(); err != nil {
		log.Printf("Error closing Redis client: %v", err)
	}

	log.Println("ScooterMQTTClient stopped.")
}

// watchInternetStatus monitors internet connectivity and updates unu-cloud status
func (s *ScooterMQTTClient) watchInternetStatus() {
	defer s.wg.Done()
	pubsub := s.redisClient.Subscribe(s.ctx, "internet")
	defer pubsub.Close()

	log.Println("Subscribed to internet status channel")

	// Check initial state on startup
	internetStatus, _ := s.redisClient.HGet(s.ctx, "internet", "status").Result()
	if internetStatus == "disconnected" {
		log.Println("Internet status is disconnected on startup, setting unu-cloud to disconnected")
		if err := s.redisClient.HSet(s.ctx, "internet", "unu-cloud", "disconnected").Err(); err != nil {
			log.Printf("Failed to set unu-cloud status on startup: %v", err)
		}
		if err := s.redisClient.Publish(s.ctx, "internet", "unu-cloud").Err(); err != nil {
			log.Printf("Failed to publish unu-cloud status on startup: %v", err)
		}
	}

	for {
		msg, err := pubsub.ReceiveMessage(s.ctx)
		if err != nil {
			// Check if the error is due to context cancellation (expected on shutdown)
			if s.ctx.Err() != nil {
				log.Println("Internet status watcher stopping due to context cancellation.")
				return
			}
			log.Printf("Error receiving internet message: %v", err)
			// Avoid busy-looping on persistent errors
			time.Sleep(5 * time.Second)
			continue
		}

		if msg.Channel == "internet" && msg.Payload == "status" {
			internetStatus, err := s.redisClient.HGet(s.ctx, "internet", "status").Result()
			if err != nil {
				log.Printf("Error getting internet status: %v", err)
				continue
			}

			log.Printf("Internet status changed to: %s", internetStatus)

			// If internet is disconnected, ensure unu-cloud is also marked as disconnected
			if internetStatus == "disconnected" {
				currentCloudStatus, _ := s.redisClient.HGet(s.ctx, "internet", "unu-cloud").Result()
				if currentCloudStatus != "disconnected" {
					log.Println("Internet disconnected, setting unu-cloud to disconnected")
					if err := s.redisClient.HSet(s.ctx, "internet", "unu-cloud", "disconnected").Err(); err != nil {
						log.Printf("Failed to set unu-cloud status: %v", err)
					}
					if err := s.redisClient.Publish(s.ctx, "internet", "unu-cloud").Err(); err != nil {
						log.Printf("Failed to publish unu-cloud status: %v", err)
					}
				}
			}
		}
	}
}

// watchDashboardStatus monitors dashboard status changes
func (s *ScooterMQTTClient) watchDashboardStatus() {
	defer s.wg.Done()
	pubsub := s.redisClient.Subscribe(s.ctx, "dashboard")
	defer pubsub.Close()

	log.Println("Subscribed to dashboard status channel")

	// Check initial state on startup in case dashboard is already ready
	ready, _ := s.redisClient.HGet(s.ctx, "dashboard", "ready").Result()
	if ready == "true" {
		log.Println("Dashboard already ready on startup, checking hostname...")
		go s.checkAndStoreDBCFlavor()
	}

	for {
		msg, err := pubsub.ReceiveMessage(s.ctx)
		if err != nil {
			// Check if the error is due to context cancellation (expected on shutdown)
			if s.ctx.Err() != nil {
				log.Println("Dashboard status watcher stopping due to context cancellation.")
				return
			}
			log.Printf("Error receiving dashboard message: %v", err)
			// Avoid busy-looping on persistent errors
			time.Sleep(5 * time.Second)
			continue // Attempt to resubscribe or handle error
		}

		if msg.Channel == "dashboard" && msg.Payload == "ready" {
			log.Println("Dashboard reported ready, checking hostname...")
			go s.checkAndStoreDBCFlavor()
		}
	}
}

// watchAlarmStatus monitors alarm status changes and publishes events to MQTT
func (s *ScooterMQTTClient) watchAlarmStatus() {
	defer s.wg.Done()
	pubsub := s.redisClient.Subscribe(s.ctx, "alarm")
	defer pubsub.Close()

	log.Println("Subscribed to alarm status channel")

	for {
		msg, err := pubsub.ReceiveMessage(s.ctx)
		if err != nil {
			// Check if the error is due to context cancellation (expected on shutdown)
			if s.ctx.Err() != nil {
				log.Println("Alarm status watcher stopping due to context cancellation.")
				return
			}
			log.Printf("Error receiving alarm message: %v", err)
			// Avoid busy-looping on persistent errors
			time.Sleep(5 * time.Second)
			continue
		}

		if msg.Channel == "alarm" {
			log.Printf("Alarm status changed to: %s", msg.Payload)
			s.publishAlarmEvent(msg.Payload)
		}
	}
}

// publishAlarmEvent publishes an alarm event to MQTT
func (s *ScooterMQTTClient) publishAlarmEvent(status string) {
	event := map[string]interface{}{
		"event_type": "alarm",
		"status":     status,
		"timestamp":  time.Now().Unix(),
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		log.Printf("Failed to marshal alarm event: %v", err)
		return
	}

	topic := fmt.Sprintf("scooters/%s/events", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, eventJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish alarm event: %v", token.Error())
	} else {
		log.Printf("Published alarm event to %s: %s", topic, string(eventJSON))
	}
}

// checkAndStoreDBCFlavor is called when the dashboard signals readiness.
// It checks DBC information, prioritizing Redis, then SSH, and updates Redis hashes.
func (s *ScooterMQTTClient) checkAndStoreDBCFlavor() {
	// Use a timeout for the whole operation, including SSH if needed
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second) // 15-second overall timeout for this function
	defer cancel()

	var dbcFlavor, dbcVersionID, dbcID string
	var fetchedViaSSH bool = false

	log.Println("Dashboard ready: Checking/Fetching DBC information...")
	dbcVersionInfo, err := s.redisClient.HGetAll(ctx, "version:dbc").Result()

	if err == nil && dbcVersionInfo["version_id"] != "" && dbcVersionInfo["id"] != "" {
		log.Printf("Found complete DBC info in version:dbc Redis hash: %v", dbcVersionInfo)
		dbcVersionID = dbcVersionInfo["version_id"]
		dbcID = dbcVersionInfo["id"]
	} else {
		if err != redis.Nil && err != nil {
			log.Printf("Error reading version:dbc from Redis: %v. Proceeding with SSH.", err)
		} else {
			log.Println("DBC version info not found or incomplete in version:dbc Redis hash, attempting SSH to DBC.")
		}

		sshCtx, sshCancel := context.WithTimeout(ctx, 10*time.Second)
		defer sshCancel()

		cmd := exec.CommandContext(sshCtx, "ssh", "-y", "root@192.168.7.2", "cat /etc/os-release")
		output, sshErr := cmd.Output()

		if sshCtx.Err() == context.DeadlineExceeded {
			log.Printf("SSH command timed out while checking DBC os-release")
			dbcID = "unknown_timeout"
			dbcVersionID = "unknown_timeout"
		} else if sshErr != nil {
			log.Printf("Failed to SSH to DBC or get os-release: %v", sshErr)
			dbcID = "unknown_ssh_error"
			dbcVersionID = "unknown_ssh_error"
		} else {
			fetchedViaSSH = true
			dbcID, dbcVersionID = parseOSRelease(string(output))

			if dbcID == "" {
				dbcID = "unknown_os_release_id"
				log.Printf("Could not find ID in DBC os-release")
			}
			if dbcVersionID == "" {
				dbcVersionID = "unknown_os_release_version"
				log.Printf("Could not find VERSION_ID in DBC os-release")
			}

			if dbcID != "" && dbcVersionID != "" &&
				!strings.HasPrefix(dbcID, "unknown_") && !strings.HasPrefix(dbcVersionID, "unknown_") {
				fieldsToSet := map[string]interface{}{
					"id":         dbcID,
					"version_id": dbcVersionID,
				}
				if pipeErr := s.redisClient.HSet(ctx, "version:dbc", fieldsToSet).Err(); pipeErr != nil {
					log.Printf("Failed to populate version:dbc Redis hash after SSH: %v", pipeErr)
				} else {
					log.Printf("Populated version:dbc Redis hash with ID: %s, VersionID: %s from SSH", dbcID, dbcVersionID)
				}
			}
		}
	}

	// Determine flavor from dbcID (either from Redis or SSH)
	if strings.Contains(dbcID, "librescoot") {
		dbcFlavor = "librescoot"
	} else if strings.Contains(dbcID, "scooteros") {
		dbcFlavor = "stock"
	} else if dbcID != "" && !strings.HasPrefix(dbcID, "unknown_") {
		dbcFlavor = dbcID // Use the actual ID if it doesn't match known patterns and isn't an error placeholder
		log.Printf("Unrecognized DBC ID '%s', using as flavor.", dbcID)
	} else {
		dbcFlavor = dbcID // This will be "unknown_..." if there was an error
		if dbcID == "" {  // Should not happen if logic above is correct, but as a fallback
			dbcFlavor = "unknown"
		}
		log.Printf("DBC ID is '%s', resulting in flavor '%s'", dbcID, dbcFlavor)
	}

	// Store final DBC flavor and version in Redis 'system' hash
	if storeErr := s.redisClient.HSet(ctx, "system", "dbc-flavor", dbcFlavor).Err(); storeErr != nil {
		log.Printf("Failed to store DBC flavor '%s' in system hash: %v", dbcFlavor, storeErr)
	} else {
		log.Printf("Stored DBC flavor as '%s' in system hash (fetched via SSH: %t)", dbcFlavor, fetchedViaSSH)
	}

	if storeErr := s.redisClient.HSet(ctx, "system", "dbc-version", dbcVersionID).Err(); storeErr != nil {
		log.Printf("Failed to store DBC version '%s' in system hash: %v", dbcVersionID, storeErr)
	} else {
		log.Printf("Stored DBC version as '%s' in system hash (fetched via SSH: %t)", dbcVersionID, fetchedViaSSH)
	}
}

// publishTelemetryData publishes a telemetry payload to MQTT
func (s *ScooterMQTTClient) publishTelemetryData(current *models.TelemetryData) error {
	telemetryJSON, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("failed to marshal telemetry: %v", err)
	}

	// Only show the detailed telemetry packet when debug is enabled
	if s.config.Debug {
		// Pretty print the JSON for detailed debugging
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, telemetryJSON, "", "  "); err != nil {
			log.Printf("Warning: Failed to format telemetry JSON: %v", err)
		} else {
			// Log complete telemetry packet
			log.Printf("Telemetry packet to be transmitted:\n%s", prettyJSON.String())

			// Also check if Config is present
			if current.Config != nil {
				log.Printf("Config section is present with %d entries", len(current.Config))

				// Check if scooter config exists specifically
				if scooter, ok := current.Config["scooter"]; ok {
					log.Printf("Scooter config is present: %+v", scooter)
				} else {
					log.Printf("Scooter config is missing from Config map")
				}
			} else {
				log.Printf("Config section is nil or empty")
			}
		}
	}

	topic := fmt.Sprintf("scooters/%s/telemetry", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, telemetryJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish telemetry: %v", token.Error())
	}

	log.Printf("Published telemetry to %s", topic)

	// Update cloud status since we successfully published to MQTT
	s.updateCloudStatus()

	return nil
}

// publishTelemetry periodically collects and publishes telemetry data
func (s *ScooterMQTTClient) publishTelemetry() {
	defer s.wg.Done()
	// Get initial interval
	interval, reason := telemetry.GetTelemetryInterval(s.ctx, s.redisClient, s.config)
	log.Printf("Initial telemetry interval: %v (%s)", interval, reason)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastState string

	// Subscribe to state changes
	pubsub := s.redisClient.Subscribe(s.ctx, "vehicle", "power-manager")
	defer pubsub.Close()

	// Start goroutine to handle state change notifications
	go func() {
		for {
			msg, err := pubsub.ReceiveMessage(s.ctx)
			if err != nil {
				if err != context.Canceled && s.ctx.Err() == nil {
					log.Printf("Error receiving pub/sub message: %v", err)
				}
				return
			}

			switch msg.Channel {
			case "vehicle":
				log.Printf("Received message on 'vehicle' channel. Payload: %s", msg.Payload)
				currentVehicleState, err := s.redisClient.HGet(s.ctx, "vehicle", "state").Result()
				if err != nil {
					log.Printf("Error getting vehicle state from Redis: %v", err)
					continue
				}

				if currentVehicleState != lastState {
					log.Printf("Vehicle state changed from '%s' to '%s' (detected via pub/sub). Publishing telemetry.", lastState, currentVehicleState)
					if err := s.collectAndPublishTelemetry(); err != nil {
						log.Printf("Failed to publish telemetry on vehicle state change (pub/sub): %v", err)
					}
					lastState = currentVehicleState

					// Also update telemetry interval if necessary
					newInterval, reason := telemetry.GetTelemetryInterval(s.ctx, s.redisClient, s.config)
					if newInterval != interval {
						log.Printf("Updating telemetry interval to %v (%s) due to vehicle state change to '%s'", newInterval, reason, currentVehicleState)
						ticker.Reset(newInterval)
						interval = newInterval
					}
				}
			case "power-manager":
				log.Printf("Received message on 'power-manager' channel. Payload: %s", msg.Payload)
				// Fetch the detailed power state from the hash
				powerState, err := s.redisClient.HGet(s.ctx, "power-manager", "state").Result()
				if err != nil {
					log.Printf("Error getting power state: %v", err)
					continue
				}

				log.Printf("Power manager state is now: %s", powerState)

				switch powerState {
				case "running":
					if !s.mqttClient.IsConnected() {
						log.Printf("Power state changed to running, reconnecting MQTT client")
						if token := s.mqttClient.Connect(); token.Wait() && token.Error() != nil {
							log.Printf("Failed to reconnect MQTT client: %v", token.Error())
						} else {
							log.Printf("MQTT client reconnected successfully")
						}
					}
				case "suspending-imminent", "hibernating-imminent", "hibernating-manual-imminent", "hibernating-timer-imminent", "reboot-imminent":
					log.Printf("Power manager entering critical state '%s', sending final telemetry", powerState)

					currentData, telErr := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.serviceStartTime)
					if telErr == nil {
						if s.config.Telemetry.Buffer.Enabled {
							if addErr := s.addTelemetryToBuffer(currentData); addErr == nil {
								if transErr := s.transmitBuffer(); transErr != nil {
									log.Printf("Failed to transmit buffer for state '%s': %v", powerState, transErr)
								}
							} else {
								log.Printf("Failed to add final telemetry to buffer for state '%s': %v", powerState, addErr)
							}
						} else {
							if pubErr := s.publishTelemetryData(currentData); pubErr != nil {
								log.Printf("Failed to publish final telemetry for state '%s': %v", powerState, pubErr)
							}
						}
					} else {
						log.Printf("Failed to get telemetry data for state '%s': %v", powerState, telErr)
					}

					log.Printf("Disconnecting MQTT client gracefully for state '%s'", powerState)
					if s.mqttClient.IsConnected() {
						s.mqttClient.Disconnect(1000)
					}
				}
			}
		}
	}()

	// Publish initial telemetry immediately
	if current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.serviceStartTime); err == nil {
		log.Println("Publishing initial telemetry...")
		if err := s.collectAndPublishTelemetry(); err == nil {
			lastState = current.VehicleState.State
		} else {
			log.Printf("Failed to publish initial telemetry: %v", err)
		}
	} else {
		log.Printf("Failed to get initial telemetry: %v", err)
	}

	for {
		select {
		case <-s.ctx.Done():
			log.Println("Telemetry publisher stopping due to context cancellation.")
			return
		case <-ticker.C:
			current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.serviceStartTime)
			if err != nil {
				log.Printf("Failed to get telemetry on ticker: %v", err)
				continue
			}

			// Ensure VehicleState is not nil before accessing State
			var currentVehicleState string
			currentVehicleState = current.VehicleState.State

			// Check if vehicle state changed (detected by polling)
			if currentVehicleState != lastState {
				log.Printf("Vehicle state changed from '%s' to '%s' (detected via ticker). Publishing telemetry.", lastState, currentVehicleState)
				// Publish telemetry due to state change FIRST
				if err := s.collectAndPublishTelemetry(); err != nil {
					log.Printf("Failed to publish telemetry on vehicle state change (ticker): %v", err)
					// Continue to update interval and lastState even if publish fails
				}
				lastState = currentVehicleState

				// Then, update telemetry interval if necessary
				newInterval, reason := telemetry.GetTelemetryInterval(s.ctx, s.redisClient, s.config)
				if newInterval != interval {
					log.Printf("Updating telemetry interval to %v (%s) due to vehicle state change to '%s'", newInterval, reason, currentVehicleState)
					ticker.Reset(newInterval)
					interval = newInterval
				}
			} else {
				// State hasn't changed, just publish normally per interval
				if err := s.collectAndPublishTelemetry(); err != nil {
					log.Printf("Failed to collect and publish telemetry on ticker: %v", err)
				}
			}
		}
	}
}

// cleanRetainedMessage removes a retained message by publishing an empty payload
func (s *ScooterMQTTClient) cleanRetainedMessage(topic string) error {
	log.Printf("Attempting to clean retained message on topic: %s", topic)

	emptyPayload := []byte{}
	log.Printf("Publishing empty payload with retain=true to topic %s", topic)

	token := s.mqttClient.Publish(topic, 1, true, emptyPayload)
	token.Wait()

	if err := token.Error(); err != nil {
		log.Printf("MQTT publish token error details: %+v", token)
		log.Printf("MQTT client connection status: %v", s.mqttClient.IsConnectionOpen())
		log.Printf("Failed to clean retained message. Topic: %s, Error: %v", topic, err)
		return fmt.Errorf("failed to clean retained message: %v", err)
	}

	log.Printf("Successfully cleaned retained message on topic: %s", topic)
	return nil
}

// getCommandParam retrieves a command parameter from configuration
func (s *ScooterMQTTClient) getCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	if cmdConfig, ok := s.config.Commands[cmd]; ok {
		if params, ok := cmdConfig.Params[param]; ok {
			return params
		}
	}
	return defaultValue
}

// updateCloudStatus sets unu-cloud status to connected and publishes notification
func (s *ScooterMQTTClient) updateCloudStatus() {
	if err := s.redisClient.HSet(s.ctx, "internet", "unu-cloud", "connected").Err(); err != nil {
		log.Printf("Failed to set unu-cloud status: %v", err)
	}
	if err := s.redisClient.Publish(s.ctx, "internet", "unu-cloud").Err(); err != nil {
		log.Printf("Failed to publish unu-cloud status: %v", err)
	}
}

// sendCommandResponse sends a response to a command
func (s *ScooterMQTTClient) sendCommandResponse(requestID, status, errorMsg string) {
	response := models.CommandResponse{
		Status:    status,
		Error:     errorMsg,
		RequestID: requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal response: %v", err)
		return
	}

	topic := fmt.Sprintf("scooters/%s/acks", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish response: %v", token.Error())
	} else {
		// Update cloud status since we successfully published to MQTT
		s.updateCloudStatus()
	}

	log.Printf("Published response to %s: %s", topic, string(responseJSON))
}
