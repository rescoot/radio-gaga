package client

import (
	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/handlers"
)

// handleCommand processes incoming MQTT commands
func (s *ScooterMQTTClient) handleCommand(client mqtt.Client, msg mqtt.Message) {
	// Create a client implementation that can be used by command handlers
	clientImpl := &handlers.ClientImplementation{
		Config:      s.config,
		MQTTClient:  s.mqttClient,
		RedisClient: s.redisClient,
		Ctx:         s.ctx,
		Version:     s.version,
	}

	// Delegate to the common command handler
	handlers.HandleCommand(clientImpl, s.mqttClient, s.redisClient, s.ctx, s.config, s.version, msg)
}

// SendCommandResponse sends a response to a command
func (s *ScooterMQTTClient) SendCommandResponse(requestID, status, errorMsg string) {
	clientImpl := &handlers.ClientImplementation{
		Config:      s.config,
		MQTTClient:  s.mqttClient,
		RedisClient: s.redisClient,
		Ctx:         s.ctx,
		Version:     s.version,
	}
	clientImpl.SendCommandResponse(requestID, status, errorMsg)
}

// GetCommandParam retrieves a command parameter from configuration
func (s *ScooterMQTTClient) GetCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	return s.getCommandParam(cmd, param, defaultValue)
}

// CleanRetainedMessage removes a retained message by publishing an empty payload
func (s *ScooterMQTTClient) CleanRetainedMessage(topic string) error {
	return s.cleanRetainedMessage(topic)
}