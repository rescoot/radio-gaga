package client

import (
	"encoding/json"
	"log"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/handlers"
	"radio-gaga/internal/models"
)

const commandQueueCapacity = 64

type queuedCommand struct {
	client  mqtt.Client
	message mqtt.Message
}

// MQTT callbacks must return without waiting for packets on the same connection.
// Unacknowledged commands remain in the broker's persistent session on overflow.
func (s *ScooterMQTTClient) handleCommand(client mqtt.Client, msg mqtt.Message) {
	if s.ctx.Err() != nil {
		return
	}
	select {
	case s.commandQueue <- queuedCommand{client: client, message: msg}:
	default:
		log.Printf("Command queue full; leaving MQTT message %d unacknowledged", msg.MessageID())
	}
}

func (s *ScooterMQTTClient) startCommandWorker() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.ctx.Done():
				return
			case command := <-s.commandQueue:
				if s.ctx.Err() != nil {
					return
				}
				if command.client != s.activeMQTTClient() {
					continue
				}
				s.processCommand(command.client, command.message)
				command.message.Ack()
			}
		}
	}()
}

func (s *ScooterMQTTClient) processCommand(client mqtt.Client, msg mqtt.Message) {
	// Update cloud status since we successfully received an MQTT message
	s.updateCloudStatus()

	// First, unmarshal the command message to access parameters for logging
	var command models.CommandMessage
	if err := json.Unmarshal(msg.Payload(), &command); err == nil {
		// Log command parameters if they exist
		if len(command.Params) > 0 {
			paramsJSON, err := json.Marshal(command.Params)
			if err == nil {
				log.Printf("Command %s (requestID: %s) parameters: %s",
					command.Command, command.RequestID, string(paramsJSON))
			} else {
				log.Printf("Command %s (requestID: %s) has parameters but failed to marshal: %v",
					command.Command, command.RequestID, err)
			}
		} else {
			log.Printf("Command %s (requestID: %s) has no parameters",
				command.Command, command.RequestID)
		}
	}

	// Use the callback-provided client. Commands queued in a persistent session
	// can arrive during Connect, before the active-client field is assigned, and
	// commands from an old client must not publish through a newer connection.
	clientImpl := &handlers.ClientImplementation{
		Config:        s.config,
		ConfigPath:    s.configPath,
		MQTTClient:    client,
		RedisClient:   s.redisClient,
		Ctx:           s.ctx,
		Version:       s.version,
		ReconnectFunc: s.RequestReconnect,
	}

	// Delegate to the common command handler
	handlers.HandleCommand(clientImpl, client, s.redisClient, s.ctx, s.config, s.version, msg)
}

// SendCommandResponse sends a response to a command
func (s *ScooterMQTTClient) SendCommandResponse(requestID, status, errorMsg string) {
	mqttClient := s.activeMQTTClient()
	if mqttClient == nil {
		log.Printf("Cannot publish command response: MQTT client is not initialized")
		return
	}
	clientImpl := &handlers.ClientImplementation{
		Config:      s.config,
		ConfigPath:  s.configPath,
		MQTTClient:  mqttClient,
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
