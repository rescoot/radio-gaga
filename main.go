package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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

type TelemetryData struct {
	// Vehicle state
	State       string `json:"state"`
	Kickstand   string `json:"kickstand"`
	SeatboxLock string `json:"seatbox"`
	Blinkers    string `json:"blinkers"`

	// Engine ECU data
	Speed        int `json:"speed"`
	Odometer     int `json:"odometer"`
	MotorVoltage int `json:"motor_voltage"`
	MotorCurrent int `json:"motor_current"`
	Temperature  int `json:"temperature"`

	// Battery data
	Battery0Level   int  `json:"battery0_level"`
	Battery1Level   int  `json:"battery1_level"`
	Battery0Present bool `json:"battery0_present"`
	Battery1Present bool `json:"battery1_present"`

	// Auxiliary batteries
	AuxBatteryLevel   int `json:"aux_battery_level"`
	AuxBatteryVoltage int `json:"aux_battery_voltage"`
	CbbBatteryLevel   int `json:"cbb_battery_level"`
	CbbBatteryCurrent int `json:"cbb_battery_current"`

	// GPS data
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`

	Timestamp string `json:"timestamp"`
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

		// Load CA cert if specified
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

	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		cancel()
		return nil, fmt.Errorf("MQTT connection failed: %v", token.Error())
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
	telemetry := &TelemetryData{}

	// Get vehicle state
	vehicle, err := s.redisClient.HGetAll(s.ctx, "vehicle").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get vehicle state: %v", err)
	}
	telemetry.State = vehicle["state"]
	telemetry.Kickstand = vehicle["kickstand"]
	telemetry.SeatboxLock = vehicle["seatbox:lock"]
	telemetry.Blinkers = vehicle["blinker:switch"]

	// Get engine ECU data
	engineEcu, err := s.redisClient.HGetAll(s.ctx, "engine-ecu").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get engine ECU data: %v", err)
	}
	telemetry.Speed, _ = strconv.Atoi(engineEcu["speed"])
	telemetry.Odometer, _ = strconv.Atoi(engineEcu["odometer"])
	telemetry.MotorVoltage, _ = strconv.Atoi(engineEcu["motor:voltage"])
	telemetry.MotorCurrent, _ = strconv.Atoi(engineEcu["motor:current"])
	telemetry.Temperature, _ = strconv.Atoi(engineEcu["temperature"])

	// Get battery data
	battery0, err := s.redisClient.HGetAll(s.ctx, "battery:0").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 0 data: %v", err)
	}
	telemetry.Battery0Level, _ = strconv.Atoi(battery0["charge"])
	telemetry.Battery0Present = battery0["present"] == "true"

	battery1, err := s.redisClient.HGetAll(s.ctx, "battery:1").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 1 data: %v", err)
	}
	telemetry.Battery1Level, _ = strconv.Atoi(battery1["charge"])
	telemetry.Battery1Present = battery1["present"] == "true"

	// Get auxiliary battery data
	auxBattery, err := s.redisClient.HGetAll(s.ctx, "aux-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get aux battery data: %v", err)
	}
	telemetry.AuxBatteryLevel, _ = strconv.Atoi(auxBattery["charge"])
	telemetry.AuxBatteryVoltage, _ = strconv.Atoi(auxBattery["voltage"])

	// Get CBB data
	cbbBattery, err := s.redisClient.HGetAll(s.ctx, "cb-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get CBB data: %v", err)
	}
	telemetry.CbbBatteryLevel, _ = strconv.Atoi(cbbBattery["charge"])
	telemetry.CbbBatteryCurrent, _ = strconv.Atoi(cbbBattery["current"])

	// Get GPS data
	gps, err := s.redisClient.HGetAll(s.ctx, "gps").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get GPS data: %v", err)
	}
	telemetry.Lat, _ = strconv.ParseFloat(gps["latitude"], 64)
	telemetry.Lng, _ = strconv.ParseFloat(gps["longitude"], 64)

	telemetry.Timestamp = time.Now().UTC().Format(time.RFC3339)

	return telemetry, nil
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
			lastState = current.State
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
			if current.State != lastState {
				newInterval, reason := s.getTelemetryInterval()
				if newInterval != interval {
					log.Printf("State changed to %s, updating telemetry interval to %v (%s)",
						current.State, newInterval, reason)
					ticker.Reset(newInterval)
					interval = newInterval
				}
				lastState = current.State
			}

			if err := s.publishTelemetryData(current); err != nil {
				log.Printf("Failed to publish telemetry: %v", err)
				continue
			}
		}
	}
}

func (s *ScooterMQTTClient) cleanRetainedMessage(topic string) error {
	if token := s.mqttClient.Publish(topic, 1, true, ""); token.Wait() && token.Error() != nil {
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
		s.sendCommandResponse(command.RequestID, "error", "Invalid command format")
		s.cleanRetainedMessage(msg.Topic())
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

	// Restrict redis and shell commands to development environment
	if (command.Command == "redis" || command.Command == "shell") && s.config.Environment != "development" {
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
	case "update":
		err = s.handleUpdateCommand()
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

func (s *ScooterMQTTClient) handleUpdateCommand() error {
	cmd := exec.Command("./radio-whats-new.sh")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Run in new process group
	}
	return cmd.Start()
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
	err = s.honkHorn(honk_time)
	time.Sleep(honk_interval)
	err = s.honkHorn(honk_time)

	time.Sleep(interval)

	err = s.honkHorn(honk_time)
	time.Sleep(honk_interval)
	err = s.honkHorn(honk_time)

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
