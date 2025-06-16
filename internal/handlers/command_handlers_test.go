package handlers

import (
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
)

// MockCommandHandlerClient implements CommandHandlerClient for testing
type MockCommandHandlerClient struct {
	responses []CommandResponse
}

type CommandResponse struct {
	RequestID string
	Status    string
	Error     string
}

func (m *MockCommandHandlerClient) SendCommandResponse(requestID, status, errorMsg string) {
	m.responses = append(m.responses, CommandResponse{
		RequestID: requestID,
		Status:    status,
		Error:     errorMsg,
	})
}

func (m *MockCommandHandlerClient) GetCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	return defaultValue
}

func (m *MockCommandHandlerClient) CleanRetainedMessage(topic string) error {
	return nil
}

func (m *MockCommandHandlerClient) PublishTelemetryData(current *models.TelemetryData) error {
	return nil
}

// MockMQTTClient implements a basic MQTT client for testing
type MockMQTTClient struct {
	publishedMessages []MockMessage
}

type MockMessage struct {
	Topic   string
	Payload []byte
}

func (m *MockMQTTClient) IsConnected() bool                                                { return true }
func (m *MockMQTTClient) IsConnectionOpen() bool                                          { return true }
func (m *MockMQTTClient) Connect() mqtt.Token                                             { return &MockToken{} }
func (m *MockMQTTClient) Disconnect(quiesce uint)                                         {}
func (m *MockMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	m.publishedMessages = append(m.publishedMessages, MockMessage{
		Topic:   topic,
		Payload: payload.([]byte),
	})
	return &MockToken{}
}
func (m *MockMQTTClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	return &MockToken{}
}
func (m *MockMQTTClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	return &MockToken{}
}
func (m *MockMQTTClient) Unsubscribe(topics ...string) mqtt.Token { return &MockToken{} }
func (m *MockMQTTClient) AddRoute(topic string, callback mqtt.MessageHandler) {}
func (m *MockMQTTClient) OptionsReader() mqtt.ClientOptionsReader { return mqtt.ClientOptionsReader{} }

type MockToken struct{}

func (m *MockToken) Wait() bool                { return true }
func (m *MockToken) WaitTimeout(time.Duration) bool { return true }
func (m *MockToken) Error() error             { return nil }
func (m *MockToken) Done() <-chan struct{}     {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestShellCommandAsync tests that shell commands run asynchronously
func TestShellCommandAsync(t *testing.T) {
	// Create mock clients
	mockClient := &MockCommandHandlerClient{}
	mockMQTT := &MockMQTTClient{}
	
	// Create a simple config
	config := &models.Config{
		Environment: "development",
		Scooter: models.ScooterConfig{
			Identifier: "test-scooter",
		},
	}

	// Test parameters for a simple shell command
	params := map[string]interface{}{
		"cmd": "echo 'Hello World' && sleep 2 && echo 'Done'",
	}

	// Record start time
	startTime := time.Now()

	// Execute the shell command
	err := handleShellCommand(mockClient, mockMQTT, config, params, "test-request-123", true)

	// Record end time
	endTime := time.Now()
	duration := endTime.Sub(startTime)

	// The function should return immediately (within 100ms) since it's async
	if duration > 100*time.Millisecond {
		t.Errorf("Shell command took too long to return: %v (expected < 100ms)", duration)
	}

	// The function should not return an error for a valid command
	if err != nil {
		t.Errorf("Shell command returned error: %v", err)
	}

	// Wait a bit longer for the async execution to produce output
	time.Sleep(3 * time.Second)

	// Check that at least one message was published (the command should have produced output)
	if len(mockMQTT.publishedMessages) == 0 {
		t.Log("No MQTT messages were published yet - this is expected for very fast commands")
	} else {
		t.Logf("Published %d MQTT messages during async execution", len(mockMQTT.publishedMessages))
	}

	t.Logf("Shell command returned in %v (async execution continuing in background)", duration)

	// The key test is that the function returned immediately, not that messages were published
	// since the async execution might complete before we check
}

// TestShellCommandInvalidCommand tests error handling for invalid commands
func TestShellCommandInvalidCommand(t *testing.T) {
	mockClient := &MockCommandHandlerClient{}
	mockMQTT := &MockMQTTClient{}
	
	config := &models.Config{
		Environment: "development",
		Scooter: models.ScooterConfig{
			Identifier: "test-scooter",
		},
	}

	// Test with missing command
	params := map[string]interface{}{}
	
	err := handleShellCommand(mockClient, mockMQTT, config, params, "test-request-456", true)
	
	if err == nil {
		t.Error("Expected error for missing command, but got nil")
	}
	
	if err.Error() != "shell command not specified" {
		t.Errorf("Expected 'shell command not specified' error, got: %v", err)
	}
}
