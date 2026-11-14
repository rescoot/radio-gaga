package handlers

import (
	"context"
	"testing"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"radio-gaga/internal/models"
)

type retainedCommandMessage struct {
	payload []byte
	topic   string
}

func (m retainedCommandMessage) Duplicate() bool   { return false }
func (m retainedCommandMessage) Qos() byte         { return 1 }
func (m retainedCommandMessage) Retained() bool    { return true }
func (m retainedCommandMessage) Topic() string     { return m.topic }
func (m retainedCommandMessage) MessageID() uint16 { return 1 }
func (m retainedCommandMessage) Payload() []byte   { return m.payload }
func (retainedCommandMessage) Ack()                {}

var _ mqtt.Message = retainedCommandMessage{}

func TestDeniedRetainedCommandsAreCleared(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		config  *models.Config
	}{
		{
			name:    "disabled command",
			payload: `{"command":"lock","request_id":"disabled"}`,
			config: &models.Config{Commands: map[string]models.Command{
				"lock": {Disabled: true},
			}},
		},
		{
			name:    "production shell command",
			payload: `{"command":"shell","request_id":"shell"}`,
			config:  &models.Config{Environment: "production"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &MockCommandHandlerClient{}
			message := retainedCommandMessage{payload: []byte(tc.payload), topic: "scooters/test/commands"}
			if err := HandleCommand(client, nil, nil, context.Background(), tc.config, "test", message); err != nil {
				t.Fatalf("HandleCommand: %v", err)
			}
			if len(client.cleanedTopics) != 1 || client.cleanedTopics[0] != message.Topic() {
				t.Fatalf("cleaned topics = %#v", client.cleanedTopics)
			}
		})
	}
}
