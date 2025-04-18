package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beevik/ntp"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
	"gopkg.in/yaml.v2"
)

type commandLineFlags struct {
	configPath    string
	identifier    string
	token         string
	mqttBrokerURL string
	mqttCACert    string
	mqttKeepAlive string
	redisURL      string
	environment   string
	// Telemetry intervals
	drivingInterval          string
	standbyInterval          string
	standbyNoBatteryInterval string
	hibernateInterval        string
}

type Config struct {
	Scooter     ScooterConfig      `yaml:"scooter"`
	Environment string             `yaml:"environment"`
	MQTT        MQTTConfig         `yaml:"mqtt"`
	RedisURL    string             `yaml:"redis_url"`
	Telemetry   TelemetryConfig    `yaml:"telemetry"`
	Commands    map[string]Command `yaml:"commands"`
	ServiceName string             `yaml:"service_name,omitempty"`
}

// detectServiceName tries to determine the systemd service name from the current process
func detectServiceName() string {
	// Try to read the process's cgroup to find the service name
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.Contains(line, ".service") {
				parts := strings.Split(line, ".service")
				if len(parts) > 0 {
					serviceParts := strings.Split(parts[0], "/")
					if len(serviceParts) > 0 {
						serviceName := serviceParts[len(serviceParts)-1] + ".service"
						if serviceName != "" {
							return serviceName
						}
					}
				}
			}
		}
	}

	// Default to rescoot-radio-gaga.service if we can't detect it
	return "rescoot-radio-gaga.service"
}

type ScooterConfig struct {
	Identifier string `yaml:"identifier"`
	Token      string `yaml:"token"`
}

type MQTTConfig struct {
	BrokerURL string `yaml:"broker_url"`
	CACert    string `yaml:"ca_cert"`
	KeepAlive string `yaml:"keepalive"`
}

type TelemetryConfig struct {
	Intervals TelemetryIntervals `yaml:"intervals"`
}

type TelemetryIntervals struct {
	Driving          string `yaml:"driving"`
	Standby          string `yaml:"standby"`
	StandbyNoBattery string `yaml:"standby_no_battery"`
	Hibernate        string `yaml:"hibernate"`
}

type Command struct {
	Disabled bool                   `yaml:"disabled"`
	Params   map[string]interface{} `yaml:"params,omitempty"`
}

type VehicleState struct {
	State         string `json:"state"`
	MainPower     string `json:"main_power"`
	HandlebarLock string `json:"handlebar_lock"`
	HandlebarPos  string `json:"handlebar_position"`
	BrakeLeft     string `json:"brake_left"`
	BrakeRight    string `json:"brake_right"`
	SeatboxLock   string `json:"seatbox"`
	Kickstand     string `json:"kickstand"`
	BlinkerSwitch string `json:"blinker_switch"`
	BlinkerState  string `json:"blinker_state"`
	SeatboxButton string `json:"seatbox_button"`
	HornButton    string `json:"horn_button"`
}

type EngineData struct {
	Speed         int    `json:"speed"`
	Odometer      int    `json:"odometer"`
	MotorVoltage  int    `json:"motor_voltage"`
	MotorCurrent  int    `json:"motor_current"`
	Temperature   int    `json:"temperature"`
	EngineState   string `json:"engine_state"`
	KersState     string `json:"kers_state"`
	KersReasonOff string `json:"kers_reason_off"`
	MotorRPM      int    `json:"motor_rpm"`
	ThrottleState string `json:"throttle_state"`
	EngineFWVer   string `json:"engine_fw_version"`
}

type BatteryData struct {
	Level             int    `json:"level"`
	Present           bool   `json:"present"`
	Voltage           int    `json:"voltage"`
	Current           int    `json:"current"`
	State             string `json:"state"`
	TemperatureState  string `json:"temp_state"`
	SOH               int    `json:"soh"`
	Temps             []int  `json:"temps"`
	CycleCount        int    `json:"cycle_count"`
	FWVersion         string `json:"fw_version"`
	ManufacturingDate string `json:"manufacturing_date"`
	SerialNumber      string `json:"serial_number"`
}

type AuxBatteryData struct {
	Level        int    `json:"level"`
	Voltage      int    `json:"voltage"`
	ChargeStatus string `json:"charge_status"`
}

type CBBatteryData struct {
	Level             int    `json:"level"`
	Current           int    `json:"current"`
	Temperature       int    `json:"temp"`
	SOH               int    `json:"soh"`
	ChargeStatus      string `json:"charge_status"`
	CellVoltage       int    `json:"cell_voltage"`
	CycleCount        int    `json:"cycle_count"`
	FullCapacity      int    `json:"full_capacity"`
	PartNumber        string `json:"part_number"`
	Present           bool   `json:"present"`
	RemainingCapacity int    `json:"remaining_capacity"`
	SerialNumber      string `json:"serial_number"`
	TimeToEmpty       int    `json:"time_to_empty"`
	TimeToFull        int    `json:"time_to_full"`
	UniqueID          string `json:"unique_id"`
}

type SystemInfo struct {
	MdbVersion   string `json:"mdb_version"`
	Environment  string `json:"environment"`
	NrfFWVersion string `json:"nrf_fw_version"`
	DbcVersion   string `json:"dbc_version"`
}

type ConnectivityStatus struct {
	ModemState     string `json:"modem_state"`
	InternetStatus string `json:"internet_status"`
	CloudStatus    string `json:"cloud_status"`
	IPAddress      string `json:"ip_address"`
	AccessTech     string `json:"access_tech"`
	SignalQuality  int    `json:"signal_quality"`
}

type GPSData struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Altitude float64 `json:"altitude"`
	GpsSpeed float64 `json:"gps_speed"`
	Course   float64 `json:"course"`
}

type PowerStatus struct {
	PowerState    string `json:"power_state"`
	PowerMuxInput string `json:"power_mux_input"`
	WakeupSource  string `json:"wakeup_source"`
}

type BLEStatus struct {
	MacAddress string `json:"mac_address"`
	Status     string `json:"status"`
}

type KeycardStatus struct {
	Authentication string `json:"authentication"`
	UID            string `json:"uid"`
	Type           string `json:"type"`
}

type DashboardStatus struct {
	Mode         string `json:"mode"`
	Ready        bool   `json:"ready"`
	SerialNumber string `json:"serial_number"`
}

// Main telemetry data structure
type TelemetryData struct {
	Version      int                `json:"version"`
	VehicleState VehicleState       `json:"vehicle_state"`
	Engine       EngineData         `json:"engine"`
	Battery0     BatteryData        `json:"battery0"`
	Battery1     BatteryData        `json:"battery1"`
	AuxBattery   AuxBatteryData     `json:"aux_battery"`
	CBBattery    CBBatteryData      `json:"cbb_battery"`
	System       SystemInfo         `json:"system"`
	Connectivity ConnectivityStatus `json:"connectivity"`
	GPS          GPSData            `json:"gps"`
	Power        PowerStatus        `json:"power"`
	BLE          BLEStatus          `json:"ble"`
	Keycard      KeycardStatus      `json:"keycard"`
	Dashboard    DashboardStatus    `json:"dashboard"`
	Timestamp    string             `json:"timestamp"`
}

type CommandMessage struct {
	Command   string                 `json:"command"`
	Params    map[string]interface{} `json:"params"`
	Timestamp int64                  `json:"timestamp"`
	RequestID string                 `json:"request_id"`
	Stream    bool                   `json:"stream,omitempty"` // For shell commands, stream output
}

type CommandResponse struct {
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	RequestID string `json:"request_id"`
}

type ScooterMQTTClient struct {
	config      *Config
	mqttClient  mqtt.Client
	redisClient *redis.Client
	ctx         context.Context
	cancel      context.CancelFunc
}

func parseFlags() *commandLineFlags {
	flags := &commandLineFlags{}

	// Basic configuration
	flag.StringVar(&flags.configPath, "config", "", "path to config file (defaults to radio-gaga.yml if not specified)")
	flag.StringVar(&flags.environment, "environment", "", "environment (production or development)")
	flag.StringVar(&flags.identifier, "identifier", "", "vehicle identifier (MQTT username)")
	flag.StringVar(&flags.token, "token", "", "authentication token (MQTT password)")
	flag.StringVar(&flags.mqttBrokerURL, "mqtt-broker", "", "MQTT broker URL")
	flag.StringVar(&flags.mqttCACert, "mqtt-cacert", "", "path to MQTT CA certificate")
	flag.StringVar(&flags.mqttKeepAlive, "mqtt-keepalive", "30s", "MQTT keepalive duration")
	flag.StringVar(&flags.redisURL, "redis-url", "redis://localhost:6379", "Redis URL")

	// Telemetry intervals
	flag.StringVar(&flags.drivingInterval, "driving-interval", "1s", "telemetry interval while driving")
	flag.StringVar(&flags.standbyInterval, "standby-interval", "5m", "telemetry interval in standby")
	flag.StringVar(&flags.standbyNoBatteryInterval, "standby-no-battery-interval", "8h", "telemetry interval in standby without battery")
	flag.StringVar(&flags.hibernateInterval, "hibernate-interval", "24h", "telemetry interval in hibernate mode")

	flag.Parse()
	return flags
}

func loadConfig(flags *commandLineFlags) (*Config, error) {
	var config *Config

	// Try to load config file
	configPath := flags.configPath
	if configPath == "" {
		configPath = "radio-gaga.yml"
	}

	// Try to read the config file
	if data, err := os.ReadFile(configPath); err == nil {
		config = &Config{}
		if err := yaml.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %v", err)
		}
		log.Printf("Loaded configuration from %s", configPath)
	} else if flags.configPath != "" {
		// Only return error if config file was explicitly specified
		return nil, fmt.Errorf("failed to read config file: %v", err)
	} else {
		// Initialize with default values
		config = &Config{
			Scooter:     ScooterConfig{},
			Environment: "production",
			MQTT: MQTTConfig{
				KeepAlive: "180s",
			},
			RedisURL: "redis://127.0.0.1:6379",
			Telemetry: TelemetryConfig{
				Intervals: TelemetryIntervals{
					Driving:          "1m",
					Standby:          "5m",
					StandbyNoBattery: "8h",
					Hibernate:        "24h",
				},
			},
		}
	}

	// Override with command line flags
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "identifier":
			config.Scooter.Identifier = flags.identifier
		case "token":
			config.Scooter.Token = flags.token
		case "environment":
			config.Environment = flags.environment
		case "mqtt-broker":
			config.MQTT.BrokerURL = flags.mqttBrokerURL
		case "mqtt-cacert":
			config.MQTT.CACert = flags.mqttCACert
		case "mqtt-keepalive":
			config.MQTT.KeepAlive = flags.mqttKeepAlive
		case "redis-url":
			config.RedisURL = flags.redisURL
		case "driving-interval":
			config.Telemetry.Intervals.Driving = flags.drivingInterval
		case "standby-interval":
			config.Telemetry.Intervals.Standby = flags.standbyInterval
		case "standby-no-battery-interval":
			config.Telemetry.Intervals.StandbyNoBattery = flags.standbyNoBatteryInterval
		case "hibernate-interval":
			config.Telemetry.Intervals.Hibernate = flags.hibernateInterval
		}
	})

	// Validate the final configuration
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %v", err)
	}

	return config, nil
}

func validateConfig(config *Config) error {
	var errors []string

	if config.Scooter.Identifier == "" {
		errors = append(errors, "scooter identifier is required")
	}
	if config.Scooter.Token == "" {
		errors = append(errors, "scooter token is required")
	}
	if config.Environment != "" && config.Environment != "production" && config.Environment != "development" {
		errors = append(errors, fmt.Sprintf("invalid environment: %s (must be 'production' or 'development')", config.Environment))
	}
	if config.RedisURL == "" {
		errors = append(errors, "redis URL is required")
	}

	// Validate telemetry intervals
	if config.Telemetry.Intervals.Driving == "" {
		config.Telemetry.Intervals.Driving = "1s"
	}
	if config.Telemetry.Intervals.Standby == "" {
		config.Telemetry.Intervals.Standby = "5m"
	}
	if config.Telemetry.Intervals.StandbyNoBattery == "" {
		config.Telemetry.Intervals.StandbyNoBattery = "8h"
	}
	if config.Telemetry.Intervals.Hibernate == "" {
		config.Telemetry.Intervals.Hibernate = "24h"
	}

	// Parse and validate durations
	durations := map[string]string{
		"mqtt.keep_alive":    config.MQTT.KeepAlive,
		"driving":            config.Telemetry.Intervals.Driving,
		"standby":            config.Telemetry.Intervals.Standby,
		"standby_no_battery": config.Telemetry.Intervals.StandbyNoBattery,
		"hibernate":          config.Telemetry.Intervals.Hibernate,
	}
	for name, value := range durations {
		if _, err := time.ParseDuration(value); err != nil {
			errors = append(errors, fmt.Sprintf("invalid %s: %v", name, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration validation failed:\n- %s", strings.Join(errors, "\n- "))
	}

	return nil
}

func isTLSURL(url string) bool {
	return strings.HasPrefix(url, "ssl://") || strings.HasPrefix(url, "tls://")
}

func createMQTTClient(config *Config, opts *mqtt.ClientOptions) (mqtt.Client, error) {
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		err := token.Error()
		if strings.Contains(err.Error(), "certificate has expired or is not yet valid") {
			log.Printf("Certificate validity period error, attempting NTP sync...")

			// Try NTP sync
			ntpErr := syncTimeNTP()
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
			if tlsConfig, err := createInsecureTLSConfig(config); err == nil {
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

func syncTimeNTP() error {
	ntpServers := []string{
		"pool.ntp.rescoot.org",
	}

	var lastErr error
	for _, server := range ntpServers {
		ntpTime, err := ntp.Time(server)
		if err != nil {
			lastErr = err
			log.Printf("Failed to get time from %s: %v", server, err)
			continue
		}

		// Convert time to timeval
		tv := syscall.NsecToTimeval(ntpTime.UnixNano())

		// Set system time (requires root privileges)
		if err := syscall.Settimeofday(&tv); err != nil {
			lastErr = fmt.Errorf("failed to set system time: %v", err)
			log.Printf("Warning: %v", lastErr)
			// Even if we can't set the system time, return success
			// as we at least got a valid time from NTP
			return nil
		}

		log.Printf("Successfully synchronized time with %s", server)
		return nil
	}

	return fmt.Errorf("failed to sync time with any NTP server: %v", lastErr)
}

func createInsecureTLSConfig(config *Config) (*tls.Config, error) {
	tlsConfig := new(tls.Config)
	tlsConfig.InsecureSkipVerify = true

	// If we have a CA cert, still load it for basic verification
	if config.MQTT.CACert != "" {
		caCert, err := os.ReadFile(config.MQTT.CACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %v", err)
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}

		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

func NewScooterMQTTClient(config *Config) (*ScooterMQTTClient, error) {
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

	if isTLSURL(config.MQTT.BrokerURL) {
		tlsConfig := new(tls.Config)
		if config.MQTT.CACert != "" {
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
	}, nil
}

func (s *ScooterMQTTClient) Start() error {
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	if token := s.mqttClient.Subscribe(commandTopic, 1, s.handleCommand); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to commands: %v", token.Error())
	}

	log.Printf("Subscribed to commands channel %s", commandTopic)

	go s.publishTelemetry()

	return nil
}

func (s *ScooterMQTTClient) Stop() {
	// Unsubscribe from command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.Scooter.Identifier)
	s.mqttClient.Unsubscribe(commandTopic)

	s.cancel()
	s.mqttClient.Disconnect(250)
	s.redisClient.Close()
}

func (s *ScooterMQTTClient) getTelemetryInterval() (time.Duration, string) {
	// Get current vehicle state from Redis
	vehicle, err := s.redisClient.HGet(s.ctx, "vehicle", "state").Result()
	if err != nil {
		log.Printf("Failed to get vehicle state: %v", err)
		return time.Minute, "fallback" // Default fallback
	}

	// Check battery state
	battery0Charge := 0
	battery0Present := false

	battery0, err := s.redisClient.HGetAll(s.ctx, "battery:0").Result()
	if err == nil {
		battery0Present = battery0["present"] == "true"
		if charge, err := strconv.Atoi(battery0["charge"]); err == nil {
			battery0Charge = charge
		}
	}

	// Determine interval based on state
	var intervalStr string
	var reason string

	switch vehicle {
	case "ready-to-drive":
		intervalStr = s.config.Telemetry.Intervals.Driving
		reason = "driving mode"
	case "hibernating":
		// should set a wakeup trigger, is this possible outside librescoot?
		intervalStr = s.config.Telemetry.Intervals.Hibernate
		reason = "hibernate mode"
	case "parked", "locked", "stand-by":
		if battery0Present && battery0Charge > 0 {
			intervalStr = s.config.Telemetry.Intervals.Standby
			reason = "standby mode with charged main battery"
		} else {
			intervalStr = s.config.Telemetry.Intervals.StandbyNoBattery
			reason = "standby mode without battery or with empty battery"
		}
	default:
		intervalStr = s.config.Telemetry.Intervals.Standby
		reason = "default standby mode"
	}

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		log.Printf("Failed to parse interval %s: %v", intervalStr, err)
		return time.Minute, "fallback" // Default fallback
	}

	return interval, reason
}

func (s *ScooterMQTTClient) getTelemetryFromRedis() (*TelemetryData, error) {
	telemetry := &TelemetryData{Version: 2}

	// Get vehicle state
	vehicle, err := s.redisClient.HGetAll(s.ctx, "vehicle").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get vehicle state: %v", err)
	}
	telemetry.VehicleState = VehicleState{
		State:         vehicle["state"],
		Kickstand:     vehicle["kickstand"],
		SeatboxLock:   vehicle["seatbox:lock"],
		BlinkerSwitch: vehicle["blinker:switch"],
		HandlebarLock: vehicle["handlebar:lock-sensor"],
		HandlebarPos:  vehicle["handlebar:position"],
		MainPower:     vehicle["main-power"],
		SeatboxButton: vehicle["seatbox:button"],
		HornButton:    vehicle["horn:button"],
		BrakeLeft:     vehicle["brake:left"],
		BrakeRight:    vehicle["brake:right"],
		BlinkerState:  vehicle["blinker:state"],
	}

	// Get engine ECU data
	engineEcu, err := s.redisClient.HGetAll(s.ctx, "engine-ecu").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get engine ECU data: %v", err)
	}
	telemetry.Engine = EngineData{
		Speed:         parseInt(engineEcu["speed"]),
		Odometer:      parseInt(engineEcu["odometer"]),
		MotorVoltage:  parseInt(engineEcu["motor:voltage"]),
		MotorCurrent:  parseInt(engineEcu["motor:current"]),
		Temperature:   parseInt(engineEcu["temperature"]),
		EngineState:   engineEcu["state"],
		KersState:     engineEcu["kers"],
		KersReasonOff: engineEcu["kers-reason-off"],
		MotorRPM:      parseInt(engineEcu["rpm"]),
		ThrottleState: engineEcu["throttle"],
		EngineFWVer:   engineEcu["fw-version"],
	}

	// Get battery data
	telemetry.Battery0, err = getBatteryData(s, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 0 data: %v", err)
	}

	telemetry.Battery1, err = getBatteryData(s, 1)
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 1 data: %v", err)
	}

	// Get auxiliary battery data
	auxBattery, err := s.redisClient.HGetAll(s.ctx, "aux-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get aux battery data: %v", err)
	}
	telemetry.AuxBattery = AuxBatteryData{
		Level:        parseInt(auxBattery["charge"]),
		Voltage:      parseInt(auxBattery["voltage"]),
		ChargeStatus: auxBattery["charge-status"],
	}

	// Get CBB data
	cbbBattery, err := s.redisClient.HGetAll(s.ctx, "cb-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get CBB data: %v", err)
	}
	telemetry.CBBattery = CBBatteryData{
		Level:             parseInt(cbbBattery["charge"]),
		Current:           parseInt(cbbBattery["current"]),
		Temperature:       parseInt(cbbBattery["temperature"]),
		SOH:               parseInt(cbbBattery["state-of-health"]),
		ChargeStatus:      cbbBattery["charge-status"],
		CellVoltage:       parseInt(cbbBattery["cell-voltage"]),
		CycleCount:        parseInt(cbbBattery["cycle-count"]),
		FullCapacity:      parseInt(cbbBattery["full-capacity"]),
		PartNumber:        cbbBattery["part-number"],
		Present:           cbbBattery["present"] == "true",
		RemainingCapacity: parseInt(cbbBattery["remaining-capacity"]),
		SerialNumber:      cbbBattery["serial-number"],
		TimeToEmpty:       parseInt(cbbBattery["time-to-empty"]),
		TimeToFull:        parseInt(cbbBattery["time-to-full"]),
		UniqueID:          cbbBattery["unique-id"],
	}

	// Get system information
	system, err := s.redisClient.HGetAll(s.ctx, "system").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %v", err)
	}
	telemetry.System = SystemInfo{
		Environment:  system["environment"],
		DbcVersion:   system["dbc-version"],
		MdbVersion:   system["mdb-version"],
		NrfFWVersion: system["nrf-fw-version"],
	}

	// Get internet connectivity status
	internet, err := s.redisClient.HGetAll(s.ctx, "internet").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get internet status: %v", err)
	}
	telemetry.Connectivity = ConnectivityStatus{
		ModemState:     internet["modem-state"],
		AccessTech:     internet["access-tech"],
		SignalQuality:  parseInt(internet["signal-quality"]),
		InternetStatus: internet["status"],
		IPAddress:      internet["ip-address"],
		CloudStatus:    internet["unu-cloud"],
	}

	// Get GPS data
	gps, err := s.redisClient.HGetAll(s.ctx, "gps").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get GPS data: %v", err)
	}
	telemetry.GPS = GPSData{
		Lat:      parseFloat(gps["latitude"]),
		Lng:      parseFloat(gps["longitude"]),
		Altitude: parseFloat(gps["altitude"]),
		GpsSpeed: parseFloat(gps["speed"]),
		Course:   parseFloat(gps["course"]),
	}

	// Get power management and mux status
	powerManager, err := s.redisClient.HGetAll(s.ctx, "power-manager").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get power manager status: %v", err)
	}
	powerMux, err := s.redisClient.HGetAll(s.ctx, "power-mux").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get power mux status: %v", err)
	}
	telemetry.Power = PowerStatus{
		PowerState:    powerManager["state"],
		PowerMuxInput: powerMux["selected-input"],
		WakeupSource:  powerManager["wakeup-source"],
	}

	// Get BLE status
	ble, err := s.redisClient.HGetAll(s.ctx, "ble").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get BLE status: %v", err)
	}
	telemetry.BLE = BLEStatus{
		MacAddress: ble["mac-address"],
		Status:     ble["status"],
	}

	// Get keycard status (usually not populated - expires 10s after tap)
	keycard, err := s.redisClient.HGetAll(s.ctx, "keycard").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get keycard status: %v", err)
	}
	telemetry.Keycard = KeycardStatus{
		Authentication: keycard["authentication"],
		UID:            keycard["uid"],
		Type:           keycard["type"],
	}

	// Get dashboard status
	dashboard, err := s.redisClient.HGetAll(s.ctx, "dashboard").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard status: %v", err)
	}
	telemetry.Dashboard = DashboardStatus{
		Mode:         dashboard["mode"],
		Ready:        dashboard["ready"] == "true",
		SerialNumber: dashboard["serial-number"],
	}

	telemetry.Timestamp = time.Now().UTC().Format(time.RFC3339)

	return telemetry, nil
}

func getBatteryData(s *ScooterMQTTClient, index int) (BatteryData, error) {
	battery, err := s.redisClient.HGetAll(s.ctx, fmt.Sprintf("battery:%d", index)).Result()
	if err != nil {
		return BatteryData{}, fmt.Errorf("failed to get battery %d data: %v", index, err)
	}

	temps := make([]int, 4)
	for i := 0; i < 4; i++ {
		temps[i] = parseInt(battery[fmt.Sprintf("temperature:%d", i)])
	}

	return BatteryData{
		Level:             parseInt(battery["charge"]),
		Present:           battery["present"] == "true",
		Voltage:           parseInt(battery["voltage"]),
		Current:           parseInt(battery["current"]),
		State:             battery["state"],
		TemperatureState:  battery["temperature-state"],
		SOH:               parseInt(battery["state-of-health"]),
		Temps:             temps,
		CycleCount:        parseInt(battery["cycle-count"]),
		FWVersion:         battery["fw-version"],
		ManufacturingDate: battery["manufacturing-date"],
		SerialNumber:      battery["serial-number"],
	}, nil
}

func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func (s *ScooterMQTTClient) publishTelemetryData(current *TelemetryData) error {
	telemetryJSON, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("failed to marshal telemetry: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/telemetry", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, telemetryJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish telemetry: %v", token.Error())
	}

	log.Printf("Published telemetry to %s", topic)
	return nil
}

func (s *ScooterMQTTClient) publishTelemetry() {
	// Get initial interval
	interval, reason := s.getTelemetryInterval()
	log.Printf("Initial telemetry interval: %v (%s)", interval, reason)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastState string

	// Subscribe to state changes
	pubsub := s.redisClient.Subscribe(s.ctx, "vehicle")
	defer pubsub.Close()

	// Start goroutine to handle state change notifications
	go func() {
		for {
			_, err := pubsub.ReceiveMessage(s.ctx)
			if err != nil {
				if err != context.Canceled {
					log.Printf("Error receiving message: %v", err)
				}
				return
			}

			// Get new interval when state changes
			newInterval, reason := s.getTelemetryInterval()
			if newInterval != interval {
				log.Printf("Updating telemetry interval to %v (%s)", newInterval, reason)
				ticker.Reset(newInterval)
				interval = newInterval
			}
		}
	}()

	// Publish initial telemetry immediately
	if current, err := s.getTelemetryFromRedis(); err == nil {
		if err := s.publishTelemetryData(current); err == nil {
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
			current, err := s.getTelemetryFromRedis()
			if err != nil {
				log.Printf("Failed to get telemetry: %v", err)
				continue
			}

			// Check if state changed
			if current.VehicleState.State != lastState {
				newInterval, reason := s.getTelemetryInterval()
				if newInterval != interval {
					log.Printf("State changed to %s, updating telemetry interval to %v (%s)",
						current.VehicleState.State, newInterval, reason)
					ticker.Reset(newInterval)
					interval = newInterval
				}
				lastState = current.VehicleState.State
			}

			if err := s.publishTelemetryData(current); err != nil {
				log.Printf("Failed to publish telemetry: %v", err)
				continue
			}
		}
	}
}

func (s *ScooterMQTTClient) cleanRetainedMessage(topic string) error {
	if token := s.mqttClient.Publish(topic, 1, true, nil); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to clean retained message: %v", token.Error())
	}
	return nil
}

func (s *ScooterMQTTClient) getCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	if cmdConfig, ok := s.config.Commands[cmd]; ok {
		if params, ok := cmdConfig.Params[param]; ok {
			return params
		}
	}
	return defaultValue
}

func (s *ScooterMQTTClient) handleCommand(client mqtt.Client, msg mqtt.Message) {
	var command CommandMessage
	if err := json.Unmarshal(msg.Payload(), &command); err != nil {
		log.Printf("Failed to parse command: %v", err)
		log.Printf("Payload was %v", msg.Payload())
		if len(msg.Payload()) != 0 {
			s.sendCommandResponse(command.RequestID, "error", "Invalid command format")
			if msg.Retained() {
				s.cleanRetainedMessage(msg.Topic())
			}
		}
		return
	}

	log.Printf("Received command: %s with requestID: %s", command.Command, command.RequestID)

	// Check if command is disabled (except for ping and get_state)
	if command.Command != "ping" && command.Command != "get_state" {
		if cmdConfig, ok := s.config.Commands[command.Command]; ok && cmdConfig.Disabled {
			log.Printf("Command %s is disabled in config", command.Command)
			s.sendCommandResponse(command.RequestID, "error", "Command disabled in config")
			return
		}
	}

	// Shell command is restricted to development environment
	if command.Command == "shell" && s.config.Environment != "development" {
		log.Printf("Command %s is not allowed in %s environment", command.Command, s.config.Environment)
		s.sendCommandResponse(command.RequestID, "error", "Command not allowed in this environment")
		return
	}

	var err error
	switch command.Command {
	case "ping":
		s.sendCommandResponse(command.RequestID, "success", "")
		if msg.Retained() {
			if err := s.cleanRetainedMessage(msg.Topic()); err != nil {
				log.Printf("Failed to clean retained message: %v", err)
			}
		}
		return // Skip error handling
	case "get_state":
		err = s.handleGetStateCommand()
	case "self_update":
		err = s.handleSelfUpdateCommand(command.Params, command.RequestID)
	case "lock":
		err = s.handleLockCommand()
	case "unlock":
		err = s.handleUnlockCommand()
	case "blinkers":
		err = s.handleBlinkersCommand(command.Params)
	case "honk":
		err = s.handleHonkCommand()
	case "open_seatbox":
		err = s.handleSeatboxCommand()
	case "locate":
		err = s.handleLocateCommand()
	case "alarm":
		err = s.handleAlarmCommand(command.Params)
	case "redis":
		err = s.handleRedisCommand(command.Params, command.RequestID)
	case "shell":
		err = s.handleShellCommand(command.Params, command.RequestID, command.Stream)
	case "navigate":
		err = s.handleNavigateCommand(command.Params, command.RequestID)
	default:
		err = fmt.Errorf("unknown command: %s", command.Command)
	}

	if err != nil {
		log.Printf("Command failed: %v", err)
		s.sendCommandResponse(command.RequestID, "error", err.Error())
		return
	}

	if msg.Retained() {
		if err := s.cleanRetainedMessage(msg.Topic()); err != nil {
			log.Printf("Failed to clean retained message: %v", err)
		}
	}

	s.sendCommandResponse(command.RequestID, "success", "")
}

func (s *ScooterMQTTClient) handleSelfUpdateCommand(params map[string]interface{}, requestID string) error {
	updateURL, ok := params["url"].(string)
	if !ok || updateURL == "" {
		return fmt.Errorf("update URL not specified or invalid")
	}

	checksum, ok := params["checksum"].(string)
	if !ok || checksum == "" {
		return fmt.Errorf("checksum not specified or invalid")
	}

	// Parse checksum algorithm and value
	parts := strings.SplitN(checksum, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid checksum format. Expected format: algorithm:value")
	}
	algorithm, expectedChecksum := parts[0], parts[1]

	// Download new binary to temporary location
	tempFile, err := os.CreateTemp("", "radio-gaga-*.new")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	log.Printf("Downloading new binary from %s", updateURL)
	resp, err := http.Get(updateURL)
	if err != nil {
		return fmt.Errorf("failed to download new binary: %v", err)
	}
	defer resp.Body.Close()

	// Calculate checksum while downloading
	var hasher hash.Hash
	switch algorithm {
	case "sha256":
		hasher = sha256.New()
	case "sha1":
		hasher = sha1.New()
	default:
		return fmt.Errorf("unsupported checksum algorithm: %s (supported: sha256, sha1)", algorithm)
	}

	writer := io.MultiWriter(tempFile, hasher)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("failed to save new binary: %v", err)
	}
	tempFile.Close()

	// Verify checksum
	calculatedChecksum := fmt.Sprintf("%x", hasher.Sum(nil))
	if calculatedChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch. Expected: %s, got: %s", expectedChecksum, calculatedChecksum)
	}

	// Make new binary executable
	if err := os.Chmod(tempFile.Name(), 0755); err != nil {
		return fmt.Errorf("failed to make new binary executable: %v", err)
	}

	// Get current executable path
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %v", err)
	}

	// Create backup of current binary
	backupPath := currentExe + ".old"
	if err := os.Rename(currentExe, backupPath); err != nil {
		return fmt.Errorf("failed to create backup: %v", err)
	}

	// Move new binary into place
	if err := os.Rename(tempFile.Name(), currentExe); err != nil {
		// Try to restore backup if moving new binary fails
		if restoreErr := os.Rename(backupPath, currentExe); restoreErr != nil {
			return fmt.Errorf("failed to move new binary and restore backup: %v (original error: %v)", restoreErr, err)
		}
		return fmt.Errorf("failed to move new binary into place: %v", err)
	}

	// Get service name from config or detect it
	serviceName := s.config.ServiceName
	if serviceName == "" {
		serviceName = detectServiceName()
	}

	// Start verification process in a goroutine
	go func() {
		// Restart the service
		cmd := exec.Command("systemctl", "restart", serviceName)
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to restart service: %v", err)
			s.rollbackUpdate(currentExe, backupPath)
			return
		}

		// Wait 10 seconds and verify the service is still running
		time.Sleep(10 * time.Second)

		cmd = exec.Command("systemctl", "is-active", serviceName)
		output, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(output)) != "active" {
			log.Printf("New version failed verification: %v", err)
			s.rollbackUpdate(currentExe, backupPath)
			return
		}

		// If we get here, update was successful - remove backup
		os.Remove(backupPath)
		log.Printf("Update successfully completed and verified")
	}()

	return nil
}

func (s *ScooterMQTTClient) rollbackUpdate(currentExe, backupPath string) {
	serviceName := s.config.ServiceName
	if serviceName == "" {
		serviceName = detectServiceName()
	}
	log.Printf("Rolling back update...")

	// Stop the service
	exec.Command("systemctl", "stop", serviceName).Run()

	// Remove failed binary
	os.Remove(currentExe)

	// Restore backup
	if err := os.Rename(backupPath, currentExe); err != nil {
		log.Printf("Failed to restore backup: %v", err)
		return
	}

	// Restart service with old binary
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		log.Printf("Failed to restart service after rollback: %v", err)
		return
	}

	log.Printf("Rollback completed successfully")
}

func (s *ScooterMQTTClient) handleGetStateCommand() error {
	telemetry, err := s.getTelemetryFromRedis()
	if err != nil {
		return err
	}

	telemetryJSON, err := json.Marshal(telemetry)
	if err != nil {
		return err
	}

	topic := fmt.Sprintf("scooters/%s/telemetry", s.config.Scooter.Identifier)
	token := s.mqttClient.Publish(topic, 1, false, telemetryJSON)
	token.Wait()
	return token.Error()
}

func (s *ScooterMQTTClient) handleLockCommand() error {
	return s.redisClient.LPush(s.ctx, "scooter:state", "lock").Err()
}

func (s *ScooterMQTTClient) handleUnlockCommand() error {
	return s.redisClient.LPush(s.ctx, "scooter:state", "unlock").Err()
}

func (s *ScooterMQTTClient) handleBlinkersCommand(params map[string]interface{}) error {
	state, ok := params["state"].(string)
	if !ok {
		return fmt.Errorf("invalid blinker state")
	}

	validStates := map[string]bool{"left": true, "right": true, "both": true, "off": true}
	if !validStates[state] {
		return fmt.Errorf("invalid blinker state: %s", state)
	}

	return s.redisClient.LPush(s.ctx, "scooter:blinker", state).Err()
}

func (s *ScooterMQTTClient) handleSeatboxCommand() error {
	return s.redisClient.LPush(s.ctx, "scooter:seatbox", "open").Err()
}

func (s *ScooterMQTTClient) handleHonkCommand() error {
	onTime := s.getCommandParam("honk", "on_time", "100ms")
	duration, err := time.ParseDuration(onTime.(string))
	if err != nil {
		duration = 100 * time.Millisecond // Default value
	}

	err = s.redisClient.LPush(s.ctx, "scooter:horn", "on").Err()
	if err != nil {
		return err
	}

	time.Sleep(duration)
	return s.redisClient.LPush(s.ctx, "scooter:horn", "off").Err()
}

func (s *ScooterMQTTClient) handleLocateCommand() error {
	// Turn on blinkers
	param_honk_time := s.getCommandParam("locate", "honk_time", "40ms")
	param_honk_interval := s.getCommandParam("locate", "honk_interval", "80ms")
	param_interval := s.getCommandParam("locate", "interval", "4s")

	honk_time, err := time.ParseDuration(param_honk_time.(string))
	if err != nil {
		honk_time = 40 * time.Millisecond // Default value
	}
	honk_interval, err := time.ParseDuration(param_honk_interval.(string))
	if err != nil {
		honk_interval = 80 * time.Millisecond // Default value
	}
	interval, err := time.ParseDuration(param_interval.(string))
	if err != nil {
		interval = 4 * time.Second
	}

	err = s.redisClient.LPush(s.ctx, "scooter:blinker", "both").Err()
	if err != nil {
		return err
	}

	// Honk twice
	s.honkHorn(honk_time)
	time.Sleep(honk_interval)
	s.honkHorn(honk_time)

	time.Sleep(interval)

	s.honkHorn(honk_time)
	time.Sleep(honk_interval)
	s.honkHorn(honk_time)

	// Turn off blinkers
	return s.redisClient.LPush(s.ctx, "scooter:blinker", "off").Err()
}

func (s *ScooterMQTTClient) handleAlarmCommand(params map[string]interface{}) error {
	if state, ok := params["state"].(string); ok && state == "off" {
		return s.stopAlarm()
	}

	duration, err := parseDuration(params["duration"])
	if err != nil {
		return fmt.Errorf("invalid alarm duration: %v", err)
	}

	flashHazards := s.getCommandParam("alarm", "hazards.flash", true).(bool)
	honkHorn := s.getCommandParam("alarm", "horn.honk", true).(bool)
	hornOnTime := s.getCommandParam("alarm", "horn.on_time", "400ms").(string)
	hornOffTime := s.getCommandParam("alarm", "horn.off_time", "400ms").(string)

	return s.startAlarmWithConfig(duration, flashHazards, honkHorn, hornOnTime, hornOffTime)
}

func (s *ScooterMQTTClient) handleNavigateCommand(params map[string]interface{}, requestID string) error {
	lat, latOK := params["latitude"].(float64)
	lng, lngOK := params["longitude"].(float64)

	if !latOK || !lngOK {
		return fmt.Errorf("invalid or missing latitude/longitude parameters")
	}

	// Format coordinates as "latitude,longitude" string
	coords := fmt.Sprintf("%f,%f", lat, lng)

	// Set the destination in Redis
	if err := s.redisClient.HSet(s.ctx, "navigation", "destination", coords).Err(); err != nil {
		return fmt.Errorf("failed to set navigation destination in Redis: %v", err)
	}

	// Publish notification to the navigation channel
	if err := s.redisClient.Publish(s.ctx, "navigation", "destination").Err(); err != nil {
		// Log the error but don't fail the command, as the HSET succeeded
		log.Printf("Warning: Failed to publish navigation destination update: %v", err)
	}

	log.Printf("Navigation target set to: %s", coords)
	return nil
}

func (s *ScooterMQTTClient) handleRedisCommand(params map[string]interface{}, requestID string) error {
	cmd, ok := params["cmd"].(string)
	if !ok {
		return fmt.Errorf("redis command not specified")
	}

	args, ok := params["args"].([]interface{})
	if !ok {
		args = []interface{}{}
	}

	var result interface{}
	var err error

	ctx := context.Background()

	switch cmd {
	case "get":
		if len(args) != 1 {
			return fmt.Errorf("get requires exactly 1 argument")
		}
		result, err = s.redisClient.Get(ctx, args[0].(string)).Result()

	case "set":
		if len(args) != 2 {
			return fmt.Errorf("set requires exactly 2 arguments")
		}
		result, err = s.redisClient.Set(ctx, args[0].(string), args[1], 0).Result()

	case "hget":
		if len(args) != 2 {
			return fmt.Errorf("hget requires exactly 2 arguments")
		}
		result, err = s.redisClient.HGet(ctx, args[0].(string), args[1].(string)).Result()

	case "hset":
		if len(args) != 3 {
			return fmt.Errorf("hset requires exactly 3 arguments")
		}
		result, err = s.redisClient.HSet(ctx, args[0].(string), args[1].(string), args[2]).Result()

	case "hgetall":
		if len(args) != 1 {
			return fmt.Errorf("hgetall requires exactly 1 argument")
		}
		result, err = s.redisClient.HGetAll(ctx, args[0].(string)).Result()

	case "lpush":
		if len(args) < 2 {
			return fmt.Errorf("lpush requires at least 2 arguments")
		}
		key := args[0].(string)
		values := args[1:]
		result, err = s.redisClient.LPush(ctx, key, values...).Result()

	case "lpop":
		if len(args) != 1 {
			return fmt.Errorf("lpop requires exactly 1 argument")
		}
		result, err = s.redisClient.LPop(ctx, args[0].(string)).Result()

	case "publish":
		if len(args) != 2 {
			return fmt.Errorf("publish requires exactly 2 arguments")
		}
		channel, ok := args[0].(string)
		if !ok {
			return fmt.Errorf("publish channel must be a string")
		}
		message, ok := args[1].(string)
		if !ok {
			return fmt.Errorf("publish message must be a string")
		}
		result, err = s.redisClient.Publish(ctx, channel, message).Result()

	default:
		return fmt.Errorf("unsupported redis command: %s", cmd)
	}

	if err != nil {
		return fmt.Errorf("redis command failed: %v", err)
	}

	// Send response on the data topic
	response := map[string]interface{}{
		"type":       "redis",
		"command":    cmd,
		"result":     result,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/data", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish response: %v", token.Error())
	}

	return nil
}

// New function for handling shell commands
func (s *ScooterMQTTClient) handleShellCommand(params map[string]interface{}, requestID string, stream bool) error {
	cmdStr, ok := params["cmd"].(string)
	if !ok {
		return fmt.Errorf("shell command not specified")
	}

	// Split command string into command and arguments
	cmdParts := strings.Fields(cmdStr)
	if len(cmdParts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.Command(cmdParts[0], cmdParts[1:]...)

	// Create pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/data", s.config.Scooter.Identifier)

	// Function to send output
	sendOutput := func(outputType string, data string) error {
		response := map[string]interface{}{
			"type":        "shell",
			"output":      data,
			"stream":      stream,
			"done":        false,
			"output_type": outputType,
			"request_id":  requestID,
		}

		responseJSON, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("failed to marshal response: %v", err)
		}

		if token := s.mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
			return fmt.Errorf("failed to publish response: %v", token.Error())
		}
		return nil
	}

	// Collect output
	var stdoutBuf, stderrBuf bytes.Buffer

	// Read stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			text := scanner.Text()
			if stream {
				sendOutput("stdout", text)
			}
			stdoutBuf.WriteString(text + "\n")
		}
	}()

	// Read stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := scanner.Text()
			if stream {
				sendOutput("stderr", text)
			}
			stderrBuf.WriteString(text + "\n")
		}
	}()

	// Wait for command to complete
	err = cmd.Wait()

	// Send final response
	response := map[string]interface{}{
		"type":       "shell",
		"stdout":     strings.TrimSpace(stdoutBuf.String()),
		"stderr":     strings.TrimSpace(stderrBuf.String()),
		"exit_code":  cmd.ProcessState.ExitCode(),
		"stream":     stream,
		"done":       true,
		"request_id": requestID,
	}

	if err != nil {
		response["error"] = err.Error()
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal final response: %v", err)
	}

	if token := s.mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish final response: %v", token.Error())
	}

	return nil
}

func parseDuration(value interface{}) (time.Duration, error) {
	switch v := value.(type) {
	case string:
		return time.ParseDuration(v)
	case float64:
		return time.Duration(v) * time.Second, nil
	default:
		return 0, fmt.Errorf("invalid duration type: %T", value)
	}
}

func (s *ScooterMQTTClient) startAlarmWithConfig(duration time.Duration, flashHazards, honkHorn bool, hornOnTime, hornOffTime string) error {
	if flashHazards {
		if err := s.redisClient.LPush(s.ctx, "scooter:blinker", "both").Err(); err != nil {
			return err
		}
	}

	if honkHorn {
		onDuration, _ := time.ParseDuration(hornOnTime)
		offDuration, _ := time.ParseDuration(hornOffTime)
		ticker := time.NewTicker(onDuration + offDuration)
		done := make(chan bool)

		go func() {
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					s.honkHorn(onDuration)
					time.Sleep(offDuration)
				}
			}
		}()

		time.Sleep(duration)
		ticker.Stop()
		done <- true
	} else {
		time.Sleep(duration)
	}

	return s.stopAlarm()
}

func (s *ScooterMQTTClient) stopAlarm() error {
	// Turn off blinkers
	err := s.redisClient.LPush(s.ctx, "scooter:blinker", "off").Err()
	if err != nil {
		return err
	}

	// Turn off horn
	return s.redisClient.LPush(s.ctx, "scooter:horn", "off").Err()
}

func (s *ScooterMQTTClient) honkHorn(duration time.Duration) error {
	err := s.redisClient.LPush(s.ctx, "scooter:horn", "on").Err()
	if err != nil {
		return err
	}

	time.Sleep(duration)

	return s.redisClient.LPush(s.ctx, "scooter:horn", "off").Err()
}

func (s *ScooterMQTTClient) sendCommandResponse(requestID, status, errorMsg string) {
	response := CommandResponse{
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

var version string

func main() {
	if version != "" {
		log.Printf("Starting radio-gaga version %s", version)
	} else {
		log.Print("Starting radio-gaga development version")
	}

	flags := parseFlags()

	config, err := loadConfig(flags)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	client, err := NewScooterMQTTClient(config)
	if err != nil {
		log.Fatalf("Failed to create MQTT client: %v", err)
	}

	if err := client.Start(); err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	client.Stop()
}
