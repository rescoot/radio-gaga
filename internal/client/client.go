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
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
	"radio-gaga/internal/telemetry"
	"radio-gaga/internal/utils"
)

// ScooterMQTTClient manages the MQTT and Redis connections
type ScooterMQTTClient struct {
	config      *models.Config
	mqttClient  mqtt.Client
	redisClient *redis.Client
	ctx         context.Context
	cancel      context.CancelFunc
	version     string
}

// NewScooterMQTTClient creates a new MQTT client
func NewScooterMQTTClient(config *models.Config, version string) (*ScooterMQTTClient, error) {
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
		err = redisClient.HSet(ctx, "system", "mdb-flavor", mdbFlavor).Err()
		if err != nil {
			log.Printf("Failed to store MDB flavor '%s' in Redis: %v", mdbFlavor, err)
		} else {
			log.Printf("Stored MDB flavor as '%s' based on hostname '%s'", mdbFlavor, mdbHostname)
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

	// Use VIN as client ID and username
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

	return &ScooterMQTTClient{
		config:      config,
		mqttClient:  mqttClient,
		redisClient: redisClient,
		ctx:         ctx,
		cancel:      cancel,
		version:     version,
	}, nil
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
	go s.publishTelemetry()
	go s.watchDashboardStatus()

	return nil
}

// Stop stops the MQTT client and closes connections
func (s *ScooterMQTTClient) Stop() {
	// Unsubscribe from command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	s.mqttClient.Unsubscribe(commandTopic)

	// Cancel context and close connections
	s.cancel()
	s.mqttClient.Disconnect(250)
	s.redisClient.Close()
}

// watchDashboardStatus monitors dashboard status changes
func (s *ScooterMQTTClient) watchDashboardStatus() {
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

// checkAndStoreDBCFlavor checks the DBC and MDB information and stores flavor and version
func (s *ScooterMQTTClient) checkAndStoreDBCFlavor() {
	// Execute ssh command to get DBC os-release
	// Note: Ensure ssh keys are set up for passwordless login from MDB to DBC
	// Use a timeout for the SSH command
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second) // 10-second timeout
	defer cancel()

	// Use Dropbear-compatible options: -y -y attempts to auto-accept host key. Timeout handled by Go context.
	cmd := exec.CommandContext(ctx, "ssh", "-y", "-y", "root@192.168.7.2", "cat /etc/os-release")
	output, err := cmd.Output()

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("SSH command timed out while checking DBC os-release")
		s.redisClient.HSet(s.ctx, "system", "dbc-flavor", "unknown_timeout").Err()
		return
	}
	if err != nil {
		log.Printf("Failed to SSH to DBC or get os-release: %v", err)
		// Set unknown states in Redis
		s.redisClient.HSet(s.ctx, "system", "dbc-flavor", "unknown_ssh_error").Err()
		s.redisClient.HSet(s.ctx, "system", "dbc-version", "unknown_ssh_error").Err()
		return
	}

	// Parse os-release output
	osRelease := string(output)
	var dbcFlavor string
	var dbcVersion string

	// Extract ID for flavor
	idMatch := strings.Split(osRelease, "ID=")
	if len(idMatch) > 1 {
		id := strings.Split(idMatch[1], "\n")[0]
		id = strings.Trim(id, "\"'")

		if strings.Contains(id, "librescoot") {
			dbcFlavor = "librescoot"
		} else if strings.Contains(id, "unu") {
			dbcFlavor = "stock"
		} else {
			dbcFlavor = id // Use the actual ID if it doesn't match known patterns
			log.Printf("Unrecognized DBC ID: %s", id)
		}
	} else {
		dbcFlavor = "unknown"
		log.Printf("Could not find ID in os-release")
	}

	// Extract VERSION_ID for version
	versionMatch := strings.Split(osRelease, "VERSION_ID=")
	if len(versionMatch) > 1 {
		dbcVersion = strings.Split(versionMatch[1], "\n")[0]
		dbcVersion = strings.Trim(dbcVersion, "\"'")
	} else {
		dbcVersion = "unknown"
		log.Printf("Could not find VERSION_ID in os-release")
	}

	// Store DBC flavor and version in Redis
	err = s.redisClient.HSet(s.ctx, "system", "dbc-flavor", dbcFlavor).Err()
	if err != nil {
		log.Printf("Failed to store DBC flavor '%s' in Redis: %v", dbcFlavor, err)
	} else {
		log.Printf("Stored DBC flavor as '%s'", dbcFlavor)
	}

	err = s.redisClient.HSet(s.ctx, "system", "dbc-version", dbcVersion).Err()
	if err != nil {
		log.Printf("Failed to store DBC version '%s' in Redis: %v", dbcVersion, err)
	} else {
		log.Printf("Stored DBC version as '%s'", dbcVersion)
	}

	// Now get MDB version from local os-release
	mdbCmd := exec.CommandContext(ctx, "sh", "-c", "cat /etc/os-release")
	mdbOutput, err := mdbCmd.Output()

	if err != nil {
		log.Printf("Failed to get MDB os-release: %v", err)
		s.redisClient.HSet(s.ctx, "system", "mdb-version", "unknown_error").Err()
	} else {
		mdbOsRelease := string(mdbOutput)
		var mdbVersion string

		// Extract VERSION_ID for MDB version
		versionMatch := strings.Split(mdbOsRelease, "VERSION_ID=")
		if len(versionMatch) > 1 {
			mdbVersion = strings.Split(versionMatch[1], "\n")[0]
			mdbVersion = strings.Trim(mdbVersion, "\"'")
		} else {
			mdbVersion = "unknown"
			log.Printf("Could not find VERSION_ID in MDB os-release")
		}

		// Store MDB version in Redis
		err = s.redisClient.HSet(s.ctx, "system", "mdb-version", mdbVersion).Err()
		if err != nil {
			log.Printf("Failed to store MDB version '%s' in Redis: %v", mdbVersion, err)
		} else {
			log.Printf("Stored MDB version as '%s'", mdbVersion)
		}
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
	return nil
}

// publishTelemetry periodically collects and publishes telemetry data
func (s *ScooterMQTTClient) publishTelemetry() {
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
				if err != context.Canceled {
					log.Printf("Error receiving message: %v", err)
				}
				return
			}

			switch msg.Channel {
			case "vehicle":
				// Get new interval when state changes
				newInterval, reason := telemetry.GetTelemetryInterval(s.ctx, s.redisClient, s.config)
				if newInterval != interval {
					log.Printf("Updating telemetry interval to %v (%s)", newInterval, reason)
					ticker.Reset(newInterval)
					interval = newInterval
				}
			case "power-manager":
				// Check if we need to handle power state change
				powerState, err := s.redisClient.HGet(s.ctx, "power-manager", "state").Result()
				if err != nil {
					log.Printf("Error getting power state: %v", err)
					continue
				}

				if powerState == "suspend" {
					log.Printf("Power manager entering suspend state")

					// Create a brief inhibitor to give us time for final telemetry
					if err := s.redisClient.Set(s.ctx, "power-manager:inhibit:radio-gaga", "final telemetry", time.Second*2).Err(); err != nil {
						log.Printf("Failed to set power manager inhibit: %v", err)
					}

					// Get and publish final telemetry
					if current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version); err == nil {
						if s.config.Telemetry.Buffer.Enabled {
							if err := s.addTelemetryToBuffer(current); err != nil {
								log.Printf("Failed to add final telemetry to buffer: %v", err)
							} else {
								log.Printf("Added final telemetry to buffer before suspend")
								// Force transmit buffer
								if err := s.transmitBuffer(); err != nil {
									log.Printf("Failed to transmit buffer before suspend: %v", err)
								} else {
									log.Printf("Transmitted buffer before suspend")
								}
							}
						} else {
							if err := s.publishTelemetryData(current); err != nil {
								log.Printf("Failed to publish final telemetry: %v", err)
							} else {
								log.Printf("Published final telemetry before suspend")
							}
						}
					} else {
						log.Printf("Failed to get final telemetry: %v", err)
					}

					// The inhibitor will automatically expire after 2 seconds
				}
			}
		}
	}()

	// Publish initial telemetry immediately
	if current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version); err == nil {
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
			return
		case <-ticker.C:
			current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version)
			if err != nil {
				log.Printf("Failed to get telemetry: %v", err)
				continue
			}

			// Check if state changed
			if current.VehicleState.State != lastState {
				newInterval, reason := telemetry.GetTelemetryInterval(s.ctx, s.redisClient, s.config)
				if newInterval != interval {
					log.Printf("State changed to %s, updating telemetry interval to %v (%s)",
						current.VehicleState.State, newInterval, reason)
					ticker.Reset(newInterval)
					interval = newInterval
				}
				lastState = current.VehicleState.State
			}

			if err := s.collectAndPublishTelemetry(); err != nil {
				log.Printf("Failed to collect and publish telemetry: %v", err)
				continue
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
	}

	log.Printf("Published response to %s: %s", topic, string(responseJSON))
}
