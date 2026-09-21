package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/events"
	"radio-gaga/internal/models"
	"radio-gaga/internal/modeminfo"
	"radio-gaga/internal/redisbus"
	"radio-gaga/internal/sms"
	locsync "radio-gaga/internal/sync"
	"radio-gaga/internal/telegram"
	"radio-gaga/internal/telemetry"
	"radio-gaga/internal/utils"
)

// redisBusDebounce coalesces bursts of pub/sub notifications on the same hash
// into a single HGETALL fan-out. 10ms is well below all telemetry/event
// deadlines and cuts redundant Redis traffic on chatty hashes like cb-battery.
const redisBusDebounce = 10 * time.Millisecond

const (
	commandSubscriptionAttempts   = 3
	commandSubscriptionRetryDelay = time.Second
	commandSubscriptionRejected   = byte(0x80)
)

var (
	errCommandSubscriptionRejected = errors.New("command subscription rejected by broker")
	errInactiveMQTTClient          = errors.New("MQTT callback client is no longer active")
)

func writeCloudStatus(ctx context.Context, client redis.Cmdable, status string) error {
	_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, "remote-access", "radio-gaga", status)
		pipe.HDel(ctx, "remote-access", "status")
		pipe.Publish(ctx, "remote-access", "radio-gaga")
		pipe.HSet(ctx, "internet", "unu-cloud", status)
		pipe.Publish(ctx, "internet", "unu-cloud")
		return nil
	})
	return err
}

// ScooterMQTTClient manages the MQTT and Redis connections
type ScooterMQTTClient struct {
	config           *models.Config
	configPath       string
	mqttClientMu     sync.RWMutex
	mqttClient       mqtt.Client
	redisClient      *redis.Client
	ctx              context.Context
	cancel           context.CancelFunc
	version          string
	serviceStartTime time.Time
	monotonicRef     time.Time   // captured at process start, preserves monotonic reading
	clockValid       atomic.Bool // true once clock is validated via NTP or wall-clock check
	sessionID        string      // unique per process lifecycle
	wg               sync.WaitGroup
	bufferMu         sync.Mutex
	buffer           *models.TelemetryBuffer // In-memory buffer cache
	pubsubsMu        sync.Mutex
	pubsubs          []*redis.PubSub

	// Priority-based telemetry monitor
	monitor *telemetry.Monitor

	// Event detector
	eventDetector *events.Detector

	// Single pub/sub fan-out for monitor + detector
	bus *redisbus.Bus

	// Telegram notifier
	telegramNotifier *telegram.Notifier

	// SMS notifier
	smsNotifier *sms.Notifier

	// Location pusher
	locationPusher *locsync.LocationPusher

	consecutivePublishFailures int32       // atomic counter for publish failure tracking
	tlsConfig                  *tls.Config // reference to active TLS config (for insecure fallback)
	reconnectMu                sync.Mutex  // serialises client rebuilds so concurrent reconnects can't orphan a client

	// remote-access readiness means the current MQTT connection has received a
	// successful SUBACK for the command topic. The generation prevents a stale
	// subscribe completion from restoring readiness after a connection loss.
	commandSubscriptionMu         sync.Mutex
	commandSubscriptionClient     mqtt.Client
	commandSubscriptionReady      bool
	commandSubscriptionGeneration uint64

	// Delta telemetry state. The server ingests type:"delta" messages and
	// deep-merges them into current_telemetry_state, so we send only changed
	// leaves instead of a full ~6KB snapshot every time. telMu serialises
	// telemetry sends so lastSentMap stays consistent across the ticker,
	// monitor-flush and state-change paths.
	telMu            sync.Mutex
	lastSentMap      map[string]any // last full state we sent, for diffing
	lastSentState    string         // last vehicle state we sent (a change forces a full)
	flushesSinceFull int            // deltas since the last full snapshot (resync cadence)
	lastGPSLat       float64        // last REPORTED position, for parked-GPS smoothing
	lastGPSLng       float64
	lastGPSValid     bool
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
	mdbOsReleaseBytes, readErr := os.ReadFile("/etc/os-release")
	var mdbFlavor string
	var mdbFlavorSource string

	if readErr == nil {
		mdbOsID, _ := parseOSRelease(string(mdbOsReleaseBytes))
		if strings.Contains(mdbOsID, "librescoot") {
			mdbFlavor = "librescoot"
			mdbFlavorSource = "os-release ID"
		} else if strings.Contains(mdbOsID, "scooteros") {
			mdbFlavor = "stock"
			mdbFlavorSource = "os-release ID"
		} else if mdbOsID != "" {
			mdbFlavor = mdbOsID
			mdbFlavorSource = "os-release ID (unrecognized)"
			log.Printf("Using unrecognized MDB ID from os-release as flavor: %s", mdbOsID)
		}
	}

	if mdbFlavor == "" {
		if readErr != nil {
			log.Printf("Warning: Failed to read /etc/os-release: %v, falling back to hostname", readErr)
		}
		mdbHostname, err := os.Hostname()
		if err != nil {
			log.Printf("Warning: Failed to get MDB hostname: %v", err)
			mdbFlavor = "unknown_error"
			mdbFlavorSource = "error"
		} else {
			if strings.HasPrefix(mdbHostname, "librescoot-") {
				mdbFlavor = "librescoot"
				mdbFlavorSource = "hostname"
			} else if strings.HasPrefix(mdbHostname, "mdb-") {
				mdbFlavor = "stock"
				mdbFlavorSource = "hostname"
			} else {
				mdbFlavor = "unknown"
				mdbFlavorSource = "hostname (unrecognized)"
				log.Printf("Unrecognized MDB hostname format: %s", mdbHostname)
			}
		}
	}

	if storeErr := redisClient.HSet(ctx, "system", "mdb-flavor", mdbFlavor).Err(); storeErr != nil {
		log.Printf("Failed to store MDB flavor '%s' in Redis: %v", mdbFlavor, storeErr)
	} else {
		log.Printf("Stored MDB flavor as '%s' (detected via %s)", mdbFlavor, mdbFlavorSource)
	}

	log.Println("Checking MDB version information at startup...")
	mdbVersionInfo, err := redisClient.HGetAll(ctx, "version:mdb").Result()
	mdbVersion := ""
	if err == nil && mdbVersionInfo["version_id"] != "" {
		mdbVersion = mdbVersionInfo["version_id"]
		log.Printf("Found MDB version_id in version:mdb Redis hash: %s", mdbVersion)
	} else {
		if err != redis.Nil && err != nil {
			log.Printf("Error reading version:mdb from Redis: %v. Using os-release data.", err)
		} else {
			log.Println("MDB version_id not found in version:mdb Redis hash, using os-release data.")
		}

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

	if config.API.BaseURL == "" {
		config.API.BaseURL = "https://sunshine.rescoot.org"
		log.Printf("Using default API base URL: %s", config.API.BaseURL)
	}

	keepAlive, err := time.ParseDuration(config.MQTT.KeepAlive)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not parse keepalive interval: %v", err)
	}

	log.Println("Setting initial cloud status to disconnected")
	if err := writeCloudStatus(ctx, redisClient, "disconnected"); err != nil {
		log.Printf("Failed to set initial cloud status: %v", err)
	}

	clientID := fmt.Sprintf("radio-gaga-%s", config.Scooter.Identifier)

	willTopic := fmt.Sprintf("scooters/%s/status", config.Scooter.Identifier)
	willMessage := []byte(`{"status": "disconnected"}`)
	commandTopic := fmt.Sprintf("scooters/%s/commands", config.Scooter.Identifier)

	// Declared up front so MQTT callbacks can use the fully initialized service
	// object. createMQTTClient registers the command route before Connect, then
	// OnConnect unconditionally subscribes on the first connect and every
	// reconnect.
	var client *ScooterMQTTClient

	opts := mqtt.NewClientOptions().
		AddBroker(config.MQTT.BrokerURL).
		SetClientID(clientID).
		SetUsername(config.Scooter.Identifier).
		SetPassword(config.Scooter.Token).
		SetKeepAlive(keepAlive).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(models.MQTTPublishTimeout).
		SetConnectTimeout(models.MQTTPublishTimeout).
		SetWriteTimeout(models.MQTTPublishTimeout).
		SetPingTimeout(models.MQTTPublishTimeout).
		SetCleanSession(false).                           // Maintain session for message queueing
		SetWill(willTopic, string(willMessage), 1, true). // QoS 1 and retained
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			if client != nil {
				client.handleMQTTConnectionLost(c, err)
			}
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			if client != nil {
				client.handleMQTTConnected(c)
			}
		})

	var activeTLSConfig *tls.Config
	if utils.IsTLSURL(config.MQTT.BrokerURL) {
		activeTLSConfig = new(tls.Config)

		if config.MQTT.CACertEmbedded != "" {
			log.Printf("Using embedded CA certificate")
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM([]byte(config.MQTT.CACertEmbedded)); !ok {
				cancel()
				return nil, fmt.Errorf("failed to parse embedded CA certificate")
			}
			activeTLSConfig.RootCAs = caCertPool
		} else if config.MQTT.CACert != "" {
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

			activeTLSConfig.RootCAs = caCertPool
		}
		opts.SetTLSConfig(activeTLSConfig)
	}

	opts.SetReconnectingHandler(func(c mqtt.Client, opts *mqtt.ClientOptions) {
		if client != nil {
			client.handleMQTTReconnectStart(c)
		}
	})

	// Capture monotonic reference BEFORE MQTT setup (which may trigger NTP)
	monotonicRef := time.Now()

	// Proactively query NTP for a valid clock reference
	var clockIsValid bool
	if _, ntpErr := utils.QueryNTPTime(&config.NTP); ntpErr == nil {
		// NTP query succeeded — attempt to set system clock too
		if syncErr := utils.SyncTimeNTP(&config.NTP); syncErr != nil {
			log.Printf("NTP query succeeded but system clock sync failed: %v", syncErr)
		}
		clockIsValid = true
		log.Printf("Clock validated via proactive NTP query")
	} else {
		// NTP failed — check if wall clock is already valid
		if telemetry.ValidateTimestamp(time.Now()) {
			clockIsValid = true
			log.Printf("Clock appears valid based on wall-clock check")
		} else {
			log.Printf("Clock not yet valid, NTP query failed: %v", ntpErr)
		}
	}

	// Generate unique session ID
	sessionID, err := generateRandomID()
	if err != nil {
		log.Printf("Failed to generate session ID: %v", err)
		sessionID = fmt.Sprintf("session-%d", time.Now().UnixNano())
	}

	// Build the struct before connecting so the route and callbacks always see a
	// fully initialized service. Command handling uses the callback-provided MQTT
	// client, so delivery during Connect does not depend on the active-client field.
	client = &ScooterMQTTClient{
		config:           config,
		configPath:       configPath,
		redisClient:      redisClient,
		ctx:              ctx,
		cancel:           cancel,
		version:          version,
		serviceStartTime: monotonicRef,
		monotonicRef:     monotonicRef,
		sessionID:        sessionID,
		tlsConfig:        activeTLSConfig,
	}
	if clockIsValid {
		client.clockValid.Store(true)
	}

	_, err = createMQTTClient(config, opts, commandTopic, client.handleCommand, client.setActiveMQTTClient)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("MQTT connection failed: %v", err)
	}
	// Single Redis pub/sub fan-out shared by monitor and event detector
	client.bus = redisbus.New(redisClient, redisBusDebounce)

	// Initialize telemetry monitor
	client.monitor = telemetry.NewMonitor(redisClient, config)
	client.monitor.SetFlusher(client)
	client.monitor.Register(client.bus)

	// Initialize event detector
	client.eventDetector = events.NewDetector(redisClient, config)
	client.eventDetector.SetPublisher(client)
	client.eventDetector.SetTelemetryFlusher(client.monitor)
	client.eventDetector.Register(client.bus)

	// Initialize Telegram notifier if enabled
	if config.Telegram.Enabled {
		notifier, err := telegram.NewNotifier(&config.Telegram, &config.Scooter)
		if err != nil {
			log.Printf("Failed to initialize Telegram notifier: %v", err)
		} else {
			client.telegramNotifier = notifier
			client.eventDetector.AddListener(notifier)
			log.Println("Telegram notifier initialized")
		}
	}

	// Initialize SMS notifier if enabled
	if config.Notifications.SMS.Enabled {
		smsNotifier, err := sms.NewNotifier(&config.Notifications.SMS, &config.Scooter)
		if err != nil {
			log.Printf("Failed to initialize SMS notifier: %v", err)
		} else {
			client.smsNotifier = smsNotifier
			client.eventDetector.AddListener(smsNotifier)
			log.Println("SMS notifier initialized")
		}
	}

	// Initialize location pusher if API is configured
	if config.API.BaseURL != "" && config.API.ScooterID != "" {
		apiTimeout, err := time.ParseDuration(config.API.Timeout)
		if err != nil {
			log.Printf("Invalid API timeout %q, using 10s: %v", config.API.Timeout, err)
			apiTimeout = 10 * time.Second
		}
		client.locationPusher = locsync.NewLocationPusher(
			redisClient, config.API.BaseURL, config.API.ScooterID,
			config.Scooter.Token, apiTimeout,
		)
		log.Printf("Location pusher initialized (API: %s, scooter: %s)", config.API.BaseURL, config.API.ScooterID)
	}

	return client, nil
}

// createMQTTClient creates and connects an MQTT client. The command route and
// active-client reference are installed before Connect so queued persistent-
// session commands can be handled safely before OnConnect re-subscribes.
func createMQTTClient(config *models.Config, opts *mqtt.ClientOptions, commandTopic string, commandHandler mqtt.MessageHandler, activate func(mqtt.Client)) (mqtt.Client, error) {
	client := mqtt.NewClient(opts)
	if commandHandler != nil {
		client.AddRoute(commandTopic, commandHandler)
	}
	if activate != nil {
		activate(client)
	}
	token := client.Connect()
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		err := token.Error()
		if strings.Contains(err.Error(), "certificate has expired or is not yet valid") {
			log.Printf("Certificate validity period error, attempting NTP sync...")

			// Try NTP sync
			ntpErr := utils.SyncTimeNTP(&config.NTP)
			if ntpErr == nil {
				// Try connecting again after time sync
				token := client.Connect()
				if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
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
				if commandHandler != nil {
					insecureClient.AddRoute(commandTopic, commandHandler)
				}
				if activate != nil {
					activate(insecureClient)
				}
				token := insecureClient.Connect()
				if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
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

// subscribeCommands subscribes to the command topic on the given client. It is
// called from the OnConnect handler on every connect (the first one and every
// reconnect), so a broker-side session loss (e.g. broker restart) can't leave
// the scooter without a command subscription while telemetry keeps flowing.
// With CleanSession(false) the broker is supposed to retain the subscription
// across reconnects, but a broker that lost all sessions will not, hence the
// unconditional (re)subscribe on each connect.
func (s *ScooterMQTTClient) subscribeCommands(c mqtt.Client) error {
	_, err := s.subscribeCommandsAttempt(c)
	return err
}

func (s *ScooterMQTTClient) subscribeCommandsAttempt(c mqtt.Client) (uint64, error) {
	generation, active := s.beginCommandSubscription(c)
	if !active {
		return 0, fmt.Errorf("failed to subscribe to commands: %w", errInactiveMQTTClient)
	}
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	token := c.Subscribe(commandTopic, 1, s.handleCommand)
	if !token.WaitTimeout(models.MQTTPublishTimeout) {
		s.failCommandSubscription(c, generation)
		return generation, fmt.Errorf("failed to subscribe to commands: timeout")
	}
	if err := token.Error(); err != nil {
		s.failCommandSubscription(c, generation)
		return generation, fmt.Errorf("failed to subscribe to commands: %v", err)
	}

	if err := validateCommandSubscriptionResult(token, commandTopic); err != nil {
		s.failCommandSubscription(c, generation)
		return generation, err
	}

	if !s.completeCommandSubscription(c, generation) {
		return generation, fmt.Errorf("failed to subscribe to commands: MQTT connection changed before subscription became ready")
	}

	log.Printf("Subscribed to commands channel %s", commandTopic)
	return generation, nil
}

func validateCommandSubscriptionResult(token mqtt.Token, commandTopic string) error {
	var (
		result          map[string]byte
		resultAvailable bool
	)
	switch token := token.(type) {
	case *mqtt.SubscribeToken:
		result = token.Result()
		resultAvailable = true
	case interface{ Result() map[string]byte }:
		// Allows focused token doubles while production uses SubscribeToken.
		result = token.Result()
		resultAvailable = true
	}
	if !resultAvailable {
		return nil
	}
	returnCode, ok := result[commandTopic]
	if !ok {
		return fmt.Errorf("%w: SUBACK omitted command topic", errCommandSubscriptionRejected)
	}
	if returnCode == commandSubscriptionRejected || returnCode > 2 {
		return fmt.Errorf("%w: SUBACK rejected topic with code 0x%02x", errCommandSubscriptionRejected, returnCode)
	}
	return nil
}

func (s *ScooterMQTTClient) subscribeCommandsWithRetry(c mqtt.Client, attempts int, delay time.Duration) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		generation, err := s.subscribeCommandsAttempt(c)
		if err != nil {
			lastErr = err
			log.Printf("Command subscription attempt %d/%d failed: %v", attempt, attempts, err)
		} else {
			return nil
		}

		// SUBACK refusal is a broker policy/protocol result, not a transport
		// failure. Keep readiness disconnected without reconnect churn.
		if errors.Is(lastErr, errCommandSubscriptionRejected) || errors.Is(lastErr, errInactiveMQTTClient) || attempt == attempts || !c.IsConnectionOpen() {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			if !s.commandSubscriptionAttemptCurrent(c, generation) {
				return lastErr
			}
		case <-s.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return s.ctx.Err()
		}
	}
	return lastErr
}

func (s *ScooterMQTTClient) beginCommandSubscription(c mqtt.Client) (uint64, bool) {
	// Hold the active-client read lock through the readiness mutation. A rebuild
	// cannot replace the active client between validation and generation update.
	s.mqttClientMu.RLock()
	defer s.mqttClientMu.RUnlock()
	if c != s.mqttClient {
		return 0, false
	}

	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()
	s.commandSubscriptionGeneration++
	s.commandSubscriptionClient = c
	s.commandSubscriptionReady = false
	s.writeCloudStatusLocked(s.ctx, "disconnected")
	return s.commandSubscriptionGeneration, true
}

func (s *ScooterMQTTClient) failCommandSubscription(c mqtt.Client, generation uint64) {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	if c != s.commandSubscriptionClient || generation != s.commandSubscriptionGeneration {
		return
	}
	s.commandSubscriptionReady = false
	s.writeCloudStatusLocked(s.ctx, "disconnected")
}

func (s *ScooterMQTTClient) commandSubscriptionAttemptCurrent(c mqtt.Client, generation uint64) bool {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()
	return c == s.commandSubscriptionClient && generation == s.commandSubscriptionGeneration && !s.commandSubscriptionReady
}

func (s *ScooterMQTTClient) completeCommandSubscription(c mqtt.Client, generation uint64) bool {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	if c != s.commandSubscriptionClient || generation != s.commandSubscriptionGeneration {
		return false
	}
	s.commandSubscriptionReady = true
	s.writeCloudStatusLocked(s.ctx, "connected")
	return true
}

func (s *ScooterMQTTClient) clearCommandSubscriptionReadiness(ctx context.Context) {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	s.commandSubscriptionGeneration++
	s.commandSubscriptionClient = nil
	s.commandSubscriptionReady = false
	s.writeCloudStatusLocked(ctx, "disconnected")
}

func (s *ScooterMQTTClient) clearCommandSubscriptionReadinessForClient(ctx context.Context, c mqtt.Client) {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	// OnConnectionLost is scheduled asynchronously. Ignore it if a different
	// client is now ready, or this client has already reconnected successfully.
	if s.commandSubscriptionClient != nil && c != s.commandSubscriptionClient {
		return
	}
	if c.IsConnectionOpen() {
		return
	}

	s.commandSubscriptionGeneration++
	s.commandSubscriptionClient = c
	s.commandSubscriptionReady = false
	s.writeCloudStatusLocked(ctx, "disconnected")
}

func (s *ScooterMQTTClient) writeCloudStatusLocked(ctx context.Context, status string) {
	if err := writeCloudStatus(ctx, s.redisClient, status); err != nil {
		log.Printf("Failed to set cloud status to %s: %v", status, err)
	}
}

func (s *ScooterMQTTClient) activeMQTTClient() mqtt.Client {
	s.mqttClientMu.RLock()
	defer s.mqttClientMu.RUnlock()
	return s.mqttClient
}

func (s *ScooterMQTTClient) setActiveMQTTClient(client mqtt.Client) {
	s.mqttClientMu.Lock()
	s.mqttClient = client
	s.mqttClientMu.Unlock()
}

func (s *ScooterMQTTClient) isCommandSubscriptionReady() bool {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()
	return s.commandSubscriptionReady
}

func (s *ScooterMQTTClient) handleMQTTConnectionLost(c mqtt.Client, err error) {
	log.Printf("Connection lost: %v", err)
	s.clearCommandSubscriptionReadinessForClient(s.ctx, c)
}

func (s *ScooterMQTTClient) handleMQTTReconnectStart(c mqtt.Client) {
	log.Printf("MQTT auto-reconnect attempting...")
	s.clearCommandSubscriptionReadinessForClient(s.ctx, c)
}

func (s *ScooterMQTTClient) handleMQTTConnected(c mqtt.Client) {
	log.Printf("Connected to MQTT broker at %s", s.config.MQTT.BrokerURL)

	if err := s.subscribeCommandsWithRetry(c, commandSubscriptionAttempts, commandSubscriptionRetryDelay); err != nil {
		log.Printf("Failed to establish command subscription: %v", err)
		if !errors.Is(err, errCommandSubscriptionRejected) && !errors.Is(err, errInactiveMQTTClient) {
			s.reconnectAfterCommandSubscriptionFailure(c)
		}
		return
	}
	s.publishConnectedStatus(c)
}

func (s *ScooterMQTTClient) reconnectAfterCommandSubscriptionFailure(c mqtt.Client) {
	if !s.commandSubscriptionNeedsReconnect(c) {
		return
	}

	go func() {
		// A concurrent OnConnect may have recovered after this rebuild was queued.
		if !s.commandSubscriptionNeedsReconnect(c) {
			return
		}
		if err := s.rebuildClient("command subscription failure"); err != nil {
			log.Printf("Failed to rebuild MQTT client after command subscription failure: %v", err)
		}
	}()
}

func (s *ScooterMQTTClient) commandSubscriptionNeedsReconnect(c mqtt.Client) bool {
	s.commandSubscriptionMu.Lock()
	needsReconnect := c == s.commandSubscriptionClient && !s.commandSubscriptionReady && c.IsConnectionOpen()
	s.commandSubscriptionMu.Unlock()
	return needsReconnect && s.activeMQTTClient() == c
}

func (s *ScooterMQTTClient) publishConnectedStatus(c mqtt.Client) {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	if !s.commandSubscriptionReady || c != s.commandSubscriptionClient {
		return
	}

	// Advertise the connection only while the command subscription has a
	// successful SUBACK. A telemetry-only connection is not remote-access ready.
	statusTopic := fmt.Sprintf("scooters/%s/status", s.config.Scooter.Identifier)
	statusMessage := []byte(`{"status": "connected"}`)
	token := c.Publish(statusTopic, 1, true, statusMessage)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		if !token.WaitTimeout(0) {
			log.Printf("Failed to publish connection status: timeout")
		} else {
			log.Printf("Failed to publish connection status: %v", token.Error())
		}
	}
}

// Start starts the MQTT client background workers. The command-topic
// subscription is handled by the OnConnect handler (see NewScooterMQTTClient),
// which runs on the first connect and every reconnect, so there is no explicit
// subscribe here.
func (s *ScooterMQTTClient) Start() error {
	// Fetch static modem identity in the background — the USB modem may not
	// have enumerated yet at this point.
	modeminfo.StartPoller(s.ctx)

	// Initialize telemetry buffer if enabled
	if s.config.Telemetry.Buffer.Enabled {
		log.Printf("Initializing telemetry buffer")
		s.initTelemetryBuffer()
	}

	// Initialize baselines for monitor and detector
	s.monitor.InitializeBaseline(s.ctx)
	s.eventDetector.InitializeBaseline(s.ctx)

	// Start telemetry and dashboard watcher goroutines
	s.wg.Add(2)
	go s.publishTelemetry()
	go s.watchDashboardStatus()

	// Start monitor, event detector, and the shared pub/sub bus
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		s.monitor.Start(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.eventDetector.Start(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.bus.Start(s.ctx)
	}()

	// Start Telegram notifier if initialized
	if s.telegramNotifier != nil {
		s.telegramNotifier.Start(s.ctx)
	}

	// Start SMS notifier if initialized
	if s.smsNotifier != nil {
		s.smsNotifier.Start(s.ctx)
	}

	// Flush any buffered events from previous session
	go s.eventDetector.FlushBufferedEvents(s.ctx)

	// Start location pusher watcher if configured
	if s.locationPusher != nil {
		s.wg.Add(1)
		go s.watchSavedLocationChanges()
	}

	return nil
}

// registerPubSub registers a pubsub connection for cleanup on shutdown
func (s *ScooterMQTTClient) registerPubSub(ps *redis.PubSub) {
	s.pubsubsMu.Lock()
	defer s.pubsubsMu.Unlock()
	s.pubsubs = append(s.pubsubs, ps)
}

// watchSavedLocationChanges subscribes to the Redis "settings" pub/sub
// channel and pushes saved locations to the Sunshine API when they change.
func (s *ScooterMQTTClient) watchSavedLocationChanges() {
	defer s.wg.Done()
	pubsub := s.redisClient.Subscribe(s.ctx, "settings")
	s.registerPubSub(pubsub)
	defer pubsub.Close()

	log.Println("Watching for saved location changes...")

	ch := pubsub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if strings.HasPrefix(msg.Payload, "dashboard.saved-locations") && s.locationPusher.ShouldPush() {
				log.Println("Local saved locations changed, pushing to server...")
				pushCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
				if err := s.locationPusher.Push(pushCtx); err != nil {
					log.Printf("Location push failed: %v", err)
				}
				cancel()
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// Stop stops the MQTT client and closes connections
func (s *ScooterMQTTClient) Stop() {
	// Stop Telegram notifier first
	if s.telegramNotifier != nil {
		log.Println("Stopping Telegram notifier...")
		s.telegramNotifier.Stop()
	}

	// Stop SMS notifier
	if s.smsNotifier != nil {
		log.Println("Stopping SMS notifier...")
		s.smsNotifier.Stop()
	}

	// Stop monitor and event detector
	log.Println("Stopping monitor and event detector...")
	s.monitor.Stop()
	s.eventDetector.Stop()

	if s.config.Telemetry.Buffer.Enabled {
		log.Println("Flushing telemetry buffer before shutdown...")
		if err := s.transmitBuffer(); err != nil {
			log.Printf("Error flushing telemetry buffer during shutdown: %v", err)
		} else {
			log.Println("Telemetry buffer flushed.")
		}
	}

	mqttClient := s.activeMQTTClient()
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	if mqttClient != nil && mqttClient.IsConnected() {
		log.Printf("Unsubscribing from %s", commandTopic)
		if token := mqttClient.Unsubscribe(commandTopic); token.WaitTimeout(2*time.Second) && token.Error() != nil {
			log.Printf("Error unsubscribing from command topic: %v", token.Error())
		}
	}

	log.Println("Cancelling client context...")
	s.cancel()

	log.Println("Closing pubsub connections...")
	s.pubsubsMu.Lock()
	for _, ps := range s.pubsubs {
		if err := ps.Close(); err != nil {
			log.Printf("Error closing pubsub: %v", err)
		}
	}
	s.pubsubsMu.Unlock()

	log.Println("Waiting for goroutines to finish...")
	s.wg.Wait()

	log.Println("Setting cloud status to disconnected before shutdown")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	s.clearCommandSubscriptionReadiness(shutdownCtx)

	if mqttClient != nil && mqttClient.IsConnected() {
		// Publish disconnected status before clean disconnect
		// (LWT is only sent on unclean disconnects, so we need to do this explicitly)
		statusTopic := fmt.Sprintf("scooters/%s/status", s.config.Scooter.Identifier)
		statusMessage := []byte(`{"status": "disconnected"}`)
		if token := mqttClient.Publish(statusTopic, 1, true, statusMessage); token.WaitTimeout(500*time.Millisecond) && token.Error() != nil {
			log.Printf("Failed to publish disconnected status on shutdown: %v", token.Error())
		} else {
			log.Printf("Published disconnected status to %s", statusTopic)
		}
		log.Println("Disconnecting MQTT client...")
		mqttClient.Disconnect(500)
	}

	// Close Redis client
	log.Println("Closing Redis client...")
	if err := s.redisClient.Close(); err != nil {
		log.Printf("Error closing Redis client: %v", err)
	}

	log.Println("ScooterMQTTClient stopped.")
}

// watchDashboardStatus monitors dashboard status changes
func (s *ScooterMQTTClient) watchDashboardStatus() {
	defer s.wg.Done()
	pubsub := s.redisClient.Subscribe(s.ctx, "dashboard")
	s.registerPubSub(pubsub)
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

	var dbcSN, dbcSNReal string

	if err == nil && dbcVersionInfo["version_id"] != "" && dbcVersionInfo["id"] != "" {
		log.Printf("Found complete DBC info in version:dbc Redis hash: %v", dbcVersionInfo)
		dbcVersionID = dbcVersionInfo["version_id"]
		dbcID = dbcVersionInfo["id"]
		dbcSN = dbcVersionInfo["serial_number"]
		dbcSNReal = dbcVersionInfo["serial_number_real"]
	} else {
		if err != redis.Nil && err != nil {
			log.Printf("Error reading version:dbc from Redis: %v. Proceeding with SSH.", err)
		} else {
			log.Println("DBC version info not found or incomplete in version:dbc Redis hash, attempting SSH to DBC.")
		}

		sshCtx, sshCancel := context.WithTimeout(ctx, 10*time.Second)
		defer sshCancel()

		// Read os-release and OTP register files in one SSH call
		cmd := exec.CommandContext(sshCtx, "ssh", "-y", "root@192.168.7.2",
			"cat /etc/os-release; cat /sys/fsl_otp/HW_OCOTP_CFG0 /sys/fsl_otp/HW_OCOTP_CFG1 2>/dev/null")
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
			outputStr := string(output)
			dbcID, dbcVersionID = parseOSRelease(outputStr)

			if dbcID == "" {
				dbcID = "unknown_os_release_id"
				log.Printf("Could not find ID in DBC os-release")
			}
			if dbcVersionID == "" {
				dbcVersionID = "unknown_os_release_version"
				log.Printf("Could not find VERSION_ID in DBC os-release")
			}

			// Extract OTP hex lines (format: "0x...") and compute DBC serial numbers
			var otpLines []string
			for _, line := range strings.Split(outputStr, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "0x") {
					otpLines = append(otpLines, strings.TrimSpace(line))
				}
			}
			if len(otpLines) >= 2 {
				legacySN, realSN, snErr := utils.ParseOTPStrings(otpLines[0], otpLines[1])
				if snErr != nil {
					log.Printf("Failed to parse DBC serial numbers: %v", snErr)
				} else {
					dbcSN = legacySN
					dbcSNReal = realSN
					log.Printf("Read DBC serial numbers via SSH: legacy=%s real=%s", dbcSN, dbcSNReal)
				}
			}

			if !strings.HasPrefix(dbcID, "unknown_") && !strings.HasPrefix(dbcVersionID, "unknown_") {
				fieldsToSet := map[string]interface{}{
					"id":                 dbcID,
					"version_id":         dbcVersionID,
					"serial_number":      dbcSN,
					"serial_number_real": dbcSNReal,
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

	if dbcSN != "" {
		if storeErr := s.redisClient.HSet(ctx, "system", "dbc-sn", dbcSN).Err(); storeErr != nil {
			log.Printf("Failed to store DBC serial number in system hash: %v", storeErr)
		}
	}
	if dbcSNReal != "" {
		if storeErr := s.redisClient.HSet(ctx, "system", "dbc-sn-real", dbcSNReal).Err(); storeErr != nil {
			log.Printf("Failed to store DBC real serial number in system hash: %v", storeErr)
		}
	}
}

// forceReconnect forces a full MQTT reconnect by rebuilding the client. Used by
// the publish-failure path when paho's auto-reconnect is stuck (expired CA, or a
// wedged connection where publishes fail with "not Connected").
func (s *ScooterMQTTClient) forceReconnect() {
	_ = s.rebuildClient("consecutive publish failures")
}

// rebuildClient tears down the current MQTT client and builds a fresh one from
// the current config, serialised by reconnectMu.
//
// Serialisation is load-bearing: without it, concurrent callers (the
// publish-failure path, RequestReconnect, migrate_broker, and duplicate command
// delivery) each disconnect-and-rebuild, and whichever client gets overwritten
// in s.mqttClient is orphaned. With SetAutoReconnect(true) that orphan keeps its
// goroutines alive and reconnects under the same client-id, so it and the live
// client repeatedly "session taken over" each other forever. We always
// Disconnect the previous client first -- even when it is only "reconnecting",
// not connected -- to stop those goroutines.
//
// We rebuild rather than Disconnect+Connect the same client: with auto-reconnect
// paho is often mid-flight, so reusing the client races its state machine
// ("status can only transition to connecting from disconnected") and can wedge.
// createMQTTClient performs the robust connect (insecure-TLS fallback) and
// buildMQTTOptions' OnConnect re-subscribes to the command topic.
func (s *ScooterMQTTClient) rebuildClient(reason string) error {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	log.Printf("Rebuilding MQTT client (%s)", reason)
	s.clearCommandSubscriptionReadiness(s.ctx)
	oldClient := s.activeMQTTClient()
	if oldClient != nil {
		oldClient.Disconnect(250)
	}

	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	_, err := createMQTTClient(s.config, s.buildMQTTOptions(), commandTopic, s.handleCommand, s.setActiveMQTTClient)
	if err != nil {
		log.Printf("Reconnect failed (%s): %v", reason, err)
		return err
	}

	atomic.StoreInt32(&s.consecutivePublishFailures, 0)
	log.Printf("MQTT client rebuilt (%s)", reason)
	return nil
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
	mqttClient := s.activeMQTTClient()
	if mqttClient == nil {
		return fmt.Errorf("failed to publish telemetry: MQTT client is not initialized")
	}
	token := mqttClient.Publish(topic, 1, false, telemetryJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		failures := atomic.AddInt32(&s.consecutivePublishFailures, 1)
		log.Printf("Publish failure #%d: %v", failures, token.Error())
		if failures >= models.MaxConsecutivePublishFailures {
			log.Printf("Reached %d consecutive publish failures, forcing reconnect", failures)
			atomic.StoreInt32(&s.consecutivePublishFailures, 0)
			go s.forceReconnect()
		}
		return fmt.Errorf("failed to publish telemetry: %v", token.Error())
	}

	atomic.StoreInt32(&s.consecutivePublishFailures, 0)
	log.Printf("Published telemetry to %s", topic)
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
	s.registerPubSub(pubsub)
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
				// Read both state and hop-on-active so we can detect the
				// effective (cloud-facing) state: while hop-on-active=true
				// vehicle-service publishes state="parked", but we report
				// "stand-by" to match GetTelemetryFromRedis.
				vehicleFields, err := s.redisClient.HMGet(s.ctx, "vehicle", "state", "hop-on-active").Result()
				if err != nil {
					log.Printf("Error getting vehicle state from Redis: %v", err)
					continue
				}
				rawState, _ := vehicleFields[0].(string)
				hopOnActive, _ := vehicleFields[1].(string)
				currentVehicleState := rawState
				if hopOnActive == "true" {
					currentVehicleState = "stand-by"
				}

				if currentVehicleState != lastState {
					log.Printf("Vehicle state changed from '%s' to '%s' (detected via pub/sub, payload: %s). Flushing telemetry.", lastState, currentVehicleState, msg.Payload)
					if err := s.collectAndFlushTelemetry(); err != nil {
						log.Printf("Failed to flush telemetry on vehicle state change (pub/sub): %v", err)
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
					mqttClient := s.activeMQTTClient()
					if mqttClient == nil || !mqttClient.IsConnectionOpen() {
						log.Printf("Power state changed to running, forcing MQTT reconnect")
						s.forceReconnect()
					}
				case "suspending-imminent", "hibernating-imminent", "hibernating-manual-imminent", "hibernating-timer-imminent", "reboot-imminent":
					log.Printf("Power manager entering critical state '%s', sending final telemetry", powerState)

					currentData, telErr := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.monotonicRef, s.clockValid.Load())
					if telErr == nil {
						// Final pre-disconnect send: force a full snapshot so the
						// server has complete state if the scooter stays offline.
						if pubErr := s.publishTelemetrySmart(currentData, true); pubErr != nil {
							log.Printf("Failed to publish final telemetry for state '%s': %v", powerState, pubErr)
						}
					} else {
						log.Printf("Failed to get telemetry data for state '%s': %v", powerState, telErr)
					}

					log.Printf("Disconnecting MQTT client gracefully for state '%s'", powerState)
					s.clearCommandSubscriptionReadiness(s.ctx)
					mqttClient := s.activeMQTTClient()
					if mqttClient != nil && mqttClient.IsConnected() {
						// Publish disconnected status before clean disconnect
						// (LWT is only sent on unclean disconnects, so we need to do this explicitly)
						statusTopic := fmt.Sprintf("scooters/%s/status", s.config.Scooter.Identifier)
						statusMessage := []byte(`{"status": "disconnected"}`)
						if token := mqttClient.Publish(statusTopic, 1, true, statusMessage); token.WaitTimeout(models.MQTTPublishTimeout) && token.Error() != nil {
							log.Printf("Failed to publish disconnected status: %v", token.Error())
						} else {
							log.Printf("Published disconnected status to %s", statusTopic)
						}
						mqttClient.Disconnect(1000)
					}
				}
			}
		}
	}()

	// Publish initial telemetry immediately
	if current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.monotonicRef, s.clockValid.Load()); err == nil {
		log.Println("Publishing initial telemetry...")
		if err := s.collectAndFlushTelemetry(); err == nil {
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
			// Check if clock has become valid since last tick
			if !s.clockValid.Load() && telemetry.ValidateTimestamp(time.Now()) {
				log.Printf("Clock is now valid, reprojecting buffered timestamps")
				s.clockValid.Store(true)
				if s.config.Telemetry.Buffer.Enabled {
					s.reprojectBufferedTimestamps()
				}
			}

			current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.monotonicRef, s.clockValid.Load())
			if err != nil {
				log.Printf("Failed to get telemetry on ticker: %v", err)
				continue
			}

			// Ensure VehicleState is not nil before accessing State
			var currentVehicleState string
			currentVehicleState = current.VehicleState.State

			// Check if vehicle state changed (detected by polling)
			if currentVehicleState != lastState {
				log.Printf("Vehicle state changed from '%s' to '%s' (detected via ticker). Flushing telemetry.", lastState, currentVehicleState)
				// Publish telemetry due to state change FIRST
				if err := s.collectAndFlushTelemetry(); err != nil {
					log.Printf("Failed to flush telemetry on vehicle state change (ticker): %v", err)
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

	mqttClient := s.activeMQTTClient()
	if mqttClient == nil {
		return fmt.Errorf("failed to clean retained message: MQTT client is not initialized")
	}
	token := mqttClient.Publish(topic, 1, true, emptyPayload)
	if !token.WaitTimeout(models.MQTTPublishTimeout) {
		log.Printf("Timeout waiting to clean retained message on topic: %s", topic)
		return fmt.Errorf("timeout cleaning retained message")
	}

	if err := token.Error(); err != nil {
		log.Printf("MQTT publish token error details: %+v", token)
		log.Printf("MQTT client connection status: %v", mqttClient.IsConnectionOpen())
		log.Printf("Failed to clean retained message. Topic: %s, Error: %v", topic, err)
		return fmt.Errorf("failed to clean retained message: %v", err)
	}

	log.Printf("Successfully cleaned retained message on topic: %s", topic)
	return nil
}

// getCommandParam retrieves a command parameter from configuration
func (s *ScooterMQTTClient) getCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	if cmdConfig, ok := s.config.Commands[cmd]; ok {
		return utils.LookupCommandParam(cmdConfig.Params, param, defaultValue)
	}
	return defaultValue
}

// updateCloudStatus refreshes reachability after an outbound publish, but only
// while the current MQTT connection has a confirmed command subscription.
func (s *ScooterMQTTClient) updateCloudStatus() {
	s.commandSubscriptionMu.Lock()
	defer s.commandSubscriptionMu.Unlock()

	if !s.commandSubscriptionReady {
		return
	}
	s.writeCloudStatusLocked(s.ctx, "connected")
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
	mqttClient := s.activeMQTTClient()
	if mqttClient == nil {
		log.Printf("Failed to publish response: MQTT client is not initialized")
		return
	}
	token := mqttClient.Publish(topic, 1, false, responseJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		log.Printf("Failed to publish response: %v", token.Error())
	} else {
		// Update cloud status since we successfully published to MQTT
		s.updateCloudStatus()
	}

	log.Printf("Published response to %s: %s", topic, string(responseJSON))
}

// GetRedisClient returns the Redis client for external use
func (s *ScooterMQTTClient) GetRedisClient() *redis.Client {
	return s.redisClient
}

// FlushTelemetry implements the TelemetryFlusher interface for the monitor.
// Collects a fresh snapshot and transmits the buffer immediately, ensuring
// priority-triggered flushes (e.g. Immediate on state change) actually send.
func (s *ScooterMQTTClient) FlushTelemetry() error {
	return s.collectAndFlushTelemetry()
}

// PublishEvent publishes an event to MQTT (implements EventPublisher interface)
func (s *ScooterMQTTClient) PublishEvent(event events.Event) error {
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/events", s.config.Scooter.Identifier)
	mqttClient := s.activeMQTTClient()
	if mqttClient == nil {
		return fmt.Errorf("failed to publish event: MQTT client is not initialized")
	}
	token := mqttClient.Publish(topic, 1, false, eventJSON)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		return fmt.Errorf("failed to publish event: %v", token.Error())
	}

	log.Printf("Published event to %s: %s", topic, event.EventType)
	s.updateCloudStatus()
	return nil
}

// IsConnected returns whether the MQTT client has an active connection
// (implements EventPublisher interface). Uses IsConnectionOpen() instead of
// IsConnected() because paho's IsConnected() returns true during the
// "reconnecting" state, which masks stuck reconnection loops.
func (s *ScooterMQTTClient) IsConnected() bool {
	mqttClient := s.activeMQTTClient()
	return mqttClient != nil && mqttClient.IsConnectionOpen()
}

// RequestReconnect disconnects and reconnects the MQTT client after a short delay.
// This is used after updating the CA certificate so the new cert is picked up.
// The delay allows the command response to be sent on the current connection first.
func (s *ScooterMQTTClient) RequestReconnect() {
	go func() {
		// Delay so the triggering command's response leaves on the current
		// connection before we tear it down.
		time.Sleep(2 * time.Second)
		_ = s.rebuildClient("configuration update")
	}()
}

// buildMQTTOptions constructs MQTT client options from current config.
func (s *ScooterMQTTClient) buildMQTTOptions() *mqtt.ClientOptions {
	keepAlive, err := time.ParseDuration(s.config.MQTT.KeepAlive)
	if err != nil {
		keepAlive = 30 * time.Second
	}

	clientID := fmt.Sprintf("radio-gaga-%s", s.config.Scooter.Identifier)
	willTopic := fmt.Sprintf("scooters/%s/status", s.config.Scooter.Identifier)
	willMessage := `{"status": "disconnected"}`

	opts := mqtt.NewClientOptions().
		AddBroker(s.config.MQTT.BrokerURL).
		SetClientID(clientID).
		SetUsername(s.config.Scooter.Identifier).
		SetPassword(s.config.Scooter.Token).
		SetKeepAlive(keepAlive).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(models.MQTTPublishTimeout).
		SetConnectTimeout(models.MQTTPublishTimeout).
		SetWriteTimeout(models.MQTTPublishTimeout).
		SetPingTimeout(models.MQTTPublishTimeout).
		SetCleanSession(false).
		SetWill(willTopic, willMessage, 1, true).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			s.handleMQTTConnectionLost(c, err)
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			s.handleMQTTConnected(c)
		}).
		SetReconnectingHandler(func(c mqtt.Client, opts *mqtt.ClientOptions) {
			s.handleMQTTReconnectStart(c)
		})

	if utils.IsTLSURL(s.config.MQTT.BrokerURL) {
		tlsConfig := new(tls.Config)

		if s.config.MQTT.CACertEmbedded != "" {
			log.Printf("Using embedded CA certificate")
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM([]byte(s.config.MQTT.CACertEmbedded)); ok {
				tlsConfig.RootCAs = caCertPool
			} else {
				log.Printf("Warning: failed to parse embedded CA certificate for reconnection")
			}
		} else if s.config.MQTT.CACert != "" {
			log.Printf("Using CA certificate from file: %s", s.config.MQTT.CACert)
			caCert, err := os.ReadFile(s.config.MQTT.CACert)
			if err == nil {
				caCertPool := x509.NewCertPool()
				if ok := caCertPool.AppendCertsFromPEM(caCert); ok {
					tlsConfig.RootCAs = caCertPool
				} else {
					log.Printf("Warning: failed to parse CA certificate from file for reconnection")
				}
			} else {
				log.Printf("Warning: failed to read CA certificate file for reconnection: %v", err)
			}
		}
		opts.SetTLSConfig(tlsConfig)
	}

	return opts
}
