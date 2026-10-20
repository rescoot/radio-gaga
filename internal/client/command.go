package client

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"time"

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
	s.startCommandWorkerWithSelfUpdate(func(client handlers.CommandHandlerClient, message mqtt.Message) {
		handlers.HandleCommand(client, nil, s.redisClient, s.ctx, s.config, s.version, message)
	})
}

func (s *ScooterMQTTClient) startCommandWorkerWithSelfUpdate(run func(handlers.CommandHandlerClient, mqtt.Message)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var updateDone <-chan struct{}
		var updateKey string
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-updateDone:
				updateDone = nil
			case command := <-s.commandQueue:
				if s.ctx.Err() != nil {
					return
				}
				if command.client != s.activeMQTTClient() {
					continue
				}
				select {
				case <-updateDone:
					updateDone = nil
				default:
				}
				var parsed models.CommandMessage
				_ = json.Unmarshal(command.message.Payload(), &parsed)
				if parsed.Command == "self_update" {
					params, _ := json.Marshal(parsed.Params)
					key := fmt.Sprintf("%s:%x", parsed.RequestID, sha256.Sum256(params))
					if updateDone != nil {
						if key != updateKey {
							s.rejectDuringSelfUpdate(command.message, parsed.RequestID)
						}
						// Delivery ACK is not application success. A duplicate shares
						// the original request's eventual post-install response.
						command.message.Ack()
						continue
					}
					updateKey = key
					done := make(chan struct{})
					updateDone = done
					s.wg.Add(1)
					go func(command queuedCommand) {
						defer s.wg.Done()
						defer close(done)
						s.updateCloudStatus()
						run(s.selfUpdateCommandClient(), command.message)
						if s.ctx.Err() == nil && command.client == s.activeMQTTClient() {
							command.message.Ack()
						}
					}(command)
					continue
				}
				if updateDone != nil && selfUpdateConflictsWithCommand(parsed.Command) {
					s.rejectDuringSelfUpdate(command.message, parsed.RequestID)
					command.message.Ack()
					continue
				}
				s.processCommand(command.client, command.message)
				command.message.Ack()
			}
		}
	}()
}

func selfUpdateConflictsWithCommand(command string) bool {
	switch command {
	case "txn:replace", "config:set", "config:del", "config:save", "update_ca_cert", "migrate_broker":
		return true
	}
	return false
}

func (s *ScooterMQTTClient) rejectDuringSelfUpdate(message mqtt.Message, requestID string) {
	client := s.selfUpdateCommandClient()
	client.SendCommandResponse(requestID, "error", "self-update in progress; retry with a new request ID after it completes")
	if err := client.CleanRetainedMessage(message.Topic()); err != nil {
		log.Printf("Failed to clean busy self-update command: %v", err)
	}
}

// A download can span connection generations. Resolve the response connection
// at publish time, and never let the captured callback client queue a stale ACK.
type selfUpdateCommandClient struct {
	*handlers.ClientImplementation
	owner *ScooterMQTTClient
}

func (s *ScooterMQTTClient) selfUpdateCommandClient() *selfUpdateCommandClient {
	return &selfUpdateCommandClient{owner: s, ClientImplementation: &handlers.ClientImplementation{
		Config: s.config, ConfigPath: s.configPath, RedisClient: s.redisClient,
		Ctx: s.ctx, Version: s.version, ReconnectFunc: s.RequestReconnect,
	}}
}

func (c *selfUpdateCommandClient) SendCommandResponse(requestID, status, message string) {
	c.SendCommandResponseWithPID(requestID, status, message, 0)
}

func (c *selfUpdateCommandClient) SendCommandResponseWithPID(requestID, status, message string, pid int) {
	response := models.CommandResponse{RequestID: requestID, Status: status, Error: message, PID: pid}
	payload, err := json.Marshal(response)
	if err == nil {
		err = c.owner.publishSelfUpdate(fmt.Sprintf("scooters/%s/acks", c.Config.Scooter.Identifier), false, payload)
	}
	if err != nil {
		log.Printf("Failed to publish self-update response: %v", err)
	}
}

func (c *selfUpdateCommandClient) CleanRetainedMessage(topic string) error {
	return c.owner.publishSelfUpdate(topic, true, []byte{})
}

func (s *ScooterMQTTClient) publishSelfUpdate(topic string, retained bool, payload []byte) error {
	s.mqttClientMu.RLock()
	if s.ctx.Err() != nil {
		s.mqttClientMu.RUnlock()
		return s.ctx.Err()
	}
	client := s.mqttClient
	if client == nil || !client.IsConnectionOpen() {
		s.mqttClientMu.RUnlock()
		return fmt.Errorf("no active MQTT connection")
	}
	// Only the enqueue is protected. Waiting for PUBACK must not hold up a
	// reconnect, and shutdown must cancel that wait.
	token := client.Publish(topic, 1, retained, payload)
	s.mqttClientMu.RUnlock()
	timer := time.NewTimer(models.MQTTPublishTimeout)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		return fmt.Errorf("self-update publish timed out")
	case <-token.Done():
		return token.Error()
	}
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
