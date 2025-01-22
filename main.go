// main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
	"golang.org/x/net/context"
)

type Config struct {
	VIN           string
	MQTTBrokerURL string
	MQTTToken     string
	RedisAddr     string
	RedisPassword string
}

type TelemetryData struct {
	// Vehicle state
	State       string `json:"state"`     // from vehicle hash - state
	Kickstand   string `json:"kickstand"` // from vehicle hash - kickstand
	SeatboxLock string `json:"seatbox"`   // from vehicle hash - seatbox:lock
	Blinkers    string `json:"blinkers"`  // from vehicle hash - blinker:switch

	// Engine ECU data
	Speed        int `json:"speed"`         // from engine-ecu hash - speed
	Odometer     int `json:"odometer"`      // from engine-ecu hash - odometer
	MotorVoltage int `json:"motor_voltage"` // from engine-ecu hash - motor:voltage
	MotorCurrent int `json:"motor_current"` // from engine-ecu hash - motor:current
	Temperature  int `json:"temperature"`   // from engine-ecu hash - temperature

	// Battery data
	Battery0Level   int  `json:"battery0_level"`   // from battery:0 hash - charge
	Battery1Level   int  `json:"battery1_level"`   // from battery:1 hash - charge
	Battery0Present bool `json:"battery0_present"` // from battery:0 hash - present
	Battery1Present bool `json:"battery1_present"` // from battery:1 hash - present

	// Auxiliary batteries
	AuxBatteryLevel   int `json:"aux_battery_level"`   // from aux-battery hash - charge
	AuxBatteryVoltage int `json:"aux_battery_voltage"` // from aux-battery hash - voltage
	CbbBatteryLevel   int `json:"cbb_battery_level"`   // from cb-battery hash - charge
	CbbBatteryCurrent int `json:"cbb_battery_current"` // from cb-battery hash - current

	// GPS data
	Lat float64 `json:"lat"` // from gps hash - latitude
	Lng float64 `json:"lng"` // from gps hash - longitude

	LastSeenAt string `json:"last_seen_at"`
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

func loadConfig() (*Config, error) {
	vin := os.Getenv("SCOOTER_VIN")
	if vin == "" {
		return nil, fmt.Errorf("SCOOTER_VIN environment variable not set")
	}

	tokenBytes, err := os.ReadFile("/etc/scooter/mqtt-token")
	if err != nil {
		tokenBytes := os.Getenv("MQTT_PASSWORD")
		if tokenBytes == "" {
			return nil, fmt.Errorf("failed to read MQTT token: %v", err)
		}
	}
	token := strings.TrimSpace(string(tokenBytes))
	token = os.Getenv("MQTT_PASSWORD")

	return &Config{
		VIN:           vin,
		MQTTBrokerURL: os.Getenv("MQTT_BROKER_URL"),
		MQTTToken:     token,
		RedisAddr:     os.Getenv("REDIS_ADDR"),
		RedisPassword: "",
	}, nil
}

type ScooterMQTTClient struct {
	config      *Config
	mqttClient  mqtt.Client
	redisClient *redis.Client
	ctx         context.Context
	cancel      context.CancelFunc
}

func NewScooterMQTTClient(config *Config) (*ScooterMQTTClient, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Setup Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr:     config.RedisAddr,
		Password: config.RedisPassword,
		DB:       0,
	})

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		cancel()
		return nil, fmt.Errorf("redis connection failed: %v", err)
	}

	// Setup MQTT options
	opts := mqtt.NewClientOptions().
		AddBroker(config.MQTTBrokerURL).
		SetClientID(fmt.Sprintf("scooter-%s", config.VIN)).
		SetUsername(config.VIN).
		SetPassword(config.MQTTToken).
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
	// Subscribe to command topic
	commandTopic := fmt.Sprintf("scooters/%s/commands", s.config.VIN)
	if token := s.mqttClient.Subscribe(commandTopic, 1, s.handleCommand); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to commands: %v", token.Error())
	}

	// Start telemetry publishing
	go s.publishTelemetry()

	return nil
}

func (s *ScooterMQTTClient) Stop() {
	s.cancel()
	s.mqttClient.Disconnect(250)
	s.redisClient.Close()
}

func (s *ScooterMQTTClient) publishTelemetry() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			telemetry, err := s.getTelemetryFromRedis()
			if err != nil {
				log.Printf("Failed to get telemetry: %v", err)
				continue
			}

			telemetryJSON, err := json.Marshal(telemetry)
			if err != nil {
				log.Printf("Failed to marshal telemetry: %v", err)
				continue
			}

			topic := fmt.Sprintf("scooters/%s/telemetry", s.config.VIN)
			if token := s.mqttClient.Publish(topic, 1, false, telemetryJSON); token.Wait() && token.Error() != nil {
				log.Printf("Failed to publish telemetry: %v", token.Error())
			}

			log.Printf("Sent telemetry update")
		}
	}
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

	// Set current timestamp
	telemetry.LastSeenAt = time.Now().UTC().Format(time.RFC3339)

	return telemetry, nil
}

func (s *ScooterMQTTClient) handleCommand(client mqtt.Client, msg mqtt.Message) {
	var command CommandMessage
	if err := json.Unmarshal(msg.Payload(), &command); err != nil {
		log.Printf("Failed to parse command: %v", err)
		s.sendCommandResponse(command.RequestID, "error", "Invalid command format")
		return
	}

	log.Printf("Received command: %s", command.Command)

	var err error
	switch command.Command {
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
	case "play_sound":
		err = s.handleHonkCommand()
		// err = s.handlePlaySoundCommand(command.Params)
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

func (s *ScooterMQTTClient) handleLockCommand() error {
	// Update Redis vehicle state
	err := s.redisClient.HSet(s.ctx, "vehicle", "state", "locked").Err()
	if err != nil {
		return fmt.Errorf("failed to update state: %v", err)
	}

	// Signal to hardware control - "scooter:state lock" as per BLE control interface
	return s.redisClient.LPush(s.ctx, "scooter:commands", "scooter:state lock").Err()
}

func (s *ScooterMQTTClient) handleUnlockCommand() error {
	err := s.redisClient.HSet(s.ctx, "vehicle", "state", "unlocked").Err()
	if err != nil {
		return fmt.Errorf("failed to update state: %v", err)
	}

	return s.redisClient.LPush(s.ctx, "scooter:commands", "scooter:state unlock").Err()
}

func (s *ScooterMQTTClient) handleBlinkersCommand(params map[string]interface{}) error {
	state, ok := params["state"].(string)
	if !ok {
		return fmt.Errorf("invalid blinker state")
	}

	// Validate blinker state
	validStates := map[string]bool{"left": true, "right": true, "both": true, "off": true}
	if !validStates[state] {
		return fmt.Errorf("invalid blinker state: %s", state)
	}

	// Update Redis vehicle state
	err := s.redisClient.HSet(s.ctx, "vehicle", "blinker:switch", state).Err()
	if err != nil {
		return fmt.Errorf("failed to update blinkers: %v", err)
	}

	// Push command as per BLE control interface format
	return s.redisClient.LPush(s.ctx, "scooter:blinker", state).Err()
}

func (s *ScooterMQTTClient) handleSeatboxCommand() error {
	return s.redisClient.LPush(s.ctx, "scooter:seatbox", "open").Err()
}

func (s *ScooterMQTTClient) handleHonkCommand() error {
	return s.redisClient.HSet(s.ctx, "vehicle", "horn:button", "on").Err()
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
}

func main() {
	config, err := loadConfig()
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

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	client.Stop()
}
