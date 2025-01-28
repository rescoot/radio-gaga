package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strconv"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
	"gopkg.in/yaml.v2"
)

type Config struct {
	VIN       string          `yaml:"vin"`
	MQTT      MQTTConfig      `yaml:"mqtt"`
	Redis     RedisConfig     `yaml:"redis"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
}

type MQTTConfig struct {
	BrokerURL string `yaml:"broker_url"`
	Token     string `yaml:"token"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
}

type TelemetryConfig struct {
	CheckInterval string `yaml:"check_interval"`
	MinInterval   string `yaml:"min_interval"`
	MaxInterval   string `yaml:"max_interval"`
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

func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	config := &Config{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %v", err)
	}

	return config, nil
}

func validateConfig(config *Config) error {
	if config.VIN == "" {
		return fmt.Errorf("vehicle ID (VIN) is required")
	}
	if config.MQTT.BrokerURL == "" {
		return fmt.Errorf("mqtt broker URL is required")
	}
	if config.MQTT.Token == "" {
		return fmt.Errorf("mqtt token is required")
	}
	if config.Redis.Addr == "" {
		return fmt.Errorf("redis address is required")
	}
	if config.Telemetry.CheckInterval == "" {
		config.Telemetry.CheckInterval = "1s"
	}
	if _, err := time.ParseDuration(config.Telemetry.CheckInterval); err != nil {
		return fmt.Errorf("invalid check_interval: %v", err)
	}
	if _, err := time.ParseDuration(config.Telemetry.MinInterval); err != nil {
		return fmt.Errorf("invalid min_interval: %v", err)
	}
	if _, err := time.ParseDuration(config.Telemetry.MaxInterval); err != nil {
		return fmt.Errorf("invalid max_interval: %v", err)
	}
	return nil
}

func NewScooterMQTTClient(config *Config) (*ScooterMQTTClient, error) {
	ctx, cancel := context.WithCancel(context.Background())

	redisClient := redis.NewClient(&redis.Options{
		Addr:     config.Redis.Addr,
		Password: config.Redis.Password,
		DB:       0,
	})

	// Test Redis connection
	_, err := redisClient.Ping(ctx).Result()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("redis connection failed: %v", err)
	}

	// Use VIN as client ID and username
	clientID := fmt.Sprintf("radio-gaga-%s", config.VIN)

	opts := mqtt.NewClientOptions().
		AddBroker(config.MQTT.BrokerURL).
		SetClientID(clientID).
		SetUsername(config.VIN). // Use VIN as username
		SetPassword(config.MQTT.Token).
		SetAutoReconnect(true).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("Connection lost: %v", err)
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("Connected to MQTT broker")
		})

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
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.VIN)
	if token := s.mqttClient.Subscribe(commandTopic, 1, s.handleCommand); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to commands: %v", token.Error())
	}

	log.Printf("Subscribed to commands on %s", commandTopic)

	go s.publishTelemetry()

	return nil
}

func (s *ScooterMQTTClient) Stop() {
	// Unsubscribe from command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.VIN)
	s.mqttClient.Unsubscribe(commandTopic)

	s.cancel()
	s.mqttClient.Disconnect(250)
	s.redisClient.Close()
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

func telemetryEqual(a, b *TelemetryData) bool {
	// Make copies to avoid modifying original data
	aCopy := *a
	bCopy := *b
	// Zero out timestamps before comparison
	aCopy.Timestamp = ""
	bCopy.Timestamp = ""
	return reflect.DeepEqual(&aCopy, &bCopy)
}

func (s *ScooterMQTTClient) publishTelemetry() {
	checkInterval, _ := time.ParseDuration(s.config.Telemetry.CheckInterval)
	minInterval, _ := time.ParseDuration(s.config.Telemetry.MinInterval)
	maxInterval, _ := time.ParseDuration(s.config.Telemetry.MaxInterval)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	var lastPublished *TelemetryData
	var lastPublishTime time.Time

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

			shouldPublish := false
			reason := ""

			if lastPublished == nil {
				shouldPublish = true
				reason = "initial telemetry"
			} else if time.Since(lastPublishTime) >= (maxInterval - checkInterval) {
				shouldPublish = true
				reason = fmt.Sprintf("max interval (%s) elapsed", s.config.Telemetry.MaxInterval)
			} else if !telemetryEqual(lastPublished, current) {
				if time.Since(lastPublishTime) >= (minInterval - checkInterval) {
					shouldPublish = true
					reason = "data changed"
				} else {
					log.Printf("data changed, but min_interval (%s) not reached yet", s.config.Telemetry.MinInterval)
				}
			}

			if !shouldPublish {
				continue
			}

			telemetryJSON, err := json.Marshal(current)
			if err != nil {
				log.Printf("Failed to marshal telemetry: %v", err)
				continue
			}

			topic := fmt.Sprintf("scooters/%s/telemetry", s.config.VIN)
			if token := s.mqttClient.Publish(topic, 1, false, telemetryJSON); token.Wait() && token.Error() != nil {
				log.Printf("Failed to publish telemetry: %v", token.Error())
				continue
			}

			log.Printf("Published telemetry to %s (%s)", topic, reason)
			lastPublished = current
			lastPublishTime = time.Now()
		}
	}
}

func (s *ScooterMQTTClient) handleCommand(client mqtt.Client, msg mqtt.Message) {
	var command CommandMessage
	if err := json.Unmarshal(msg.Payload(), &command); err != nil {
		log.Printf("Failed to parse command: %v", err)
		s.sendCommandResponse(command.RequestID, "error", "Invalid command format")
		return
	}

	log.Printf("Received command: %s with requestID: %s", command.Command, command.RequestID)

	var err error
	switch command.Command {
	case "ping":
		s.sendCommandResponse(command.RequestID, "success", "")
		return // Skip error handling
	case "update":
		err = s.handleUpdateCommand()
	case "get_state":
		err = s.handleGetStateCommand()
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
	default:
		err = fmt.Errorf("unknown command: %s", command.Command)
	}

	if err != nil {
		log.Printf("Command failed: %v", err)
		s.sendCommandResponse(command.RequestID, "error", err.Error())
		return
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

	topic := fmt.Sprintf("scooters/%s/telemetry", s.config.VIN)
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
	err := s.redisClient.LPush(s.ctx, "scooter:horn", "on").Err()
	if err != nil {
		return err
	}

	time.Sleep(50 * time.Millisecond)
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

	topic := fmt.Sprintf("scooters/%s/acks", s.config.VIN)
	if token := s.mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish response: %v", token.Error())
	}

	log.Printf("Published response to %s: %s", topic, string(responseJSON))
}

var version string

func main() {
	log.Printf("Starting radio-gaga version %s", version)

	configPath := flag.String("config", "radio-gaga.yml", "path to config file")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
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
