package client

import (
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"radio-gaga/internal/models"
)

// fakeToken is a minimal mqtt.Token used to drive subscribeCommands in tests.
type fakeToken struct {
	err error
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeToken) Error() error { return t.err }

// fakeMQTTClient embeds mqtt.Client so only the methods we exercise need real
// implementations; any other call would panic, which is fine for these tests.
type fakeMQTTClient struct {
	mqtt.Client
	subTopic string
	subQoS   byte
	subErr   error
	subCalls int
}

func (c *fakeMQTTClient) Subscribe(topic string, qos byte, _ mqtt.MessageHandler) mqtt.Token {
	c.subCalls++
	c.subTopic = topic
	c.subQoS = qos
	return &fakeToken{err: c.subErr}
}

func newTestClient() *ScooterMQTTClient {
	cfg := &models.Config{}
	cfg.Scooter.Identifier = "test-scooter"
	return &ScooterMQTTClient{config: cfg}
}

func TestSubscribeCommands_Success(t *testing.T) {
	s := newTestClient()
	fc := &fakeMQTTClient{}

	if err := s.subscribeCommands(fc); err != nil {
		t.Fatalf("subscribeCommands returned error: %v", err)
	}

	if fc.subCalls != 1 {
		t.Fatalf("expected 1 Subscribe call, got %d", fc.subCalls)
	}
	if want := "scooters/test-scooter/commands"; fc.subTopic != want {
		t.Fatalf("subscribed to %q, want %q", fc.subTopic, want)
	}
	if fc.subQoS != 1 {
		t.Fatalf("subscribed with QoS %d, want 1", fc.subQoS)
	}
}

func TestSubscribeCommands_TokenError(t *testing.T) {
	s := newTestClient()
	fc := &fakeMQTTClient{subErr: errors.New("broker refused")}

	err := s.subscribeCommands(fc)
	if err == nil {
		t.Fatal("expected error from subscribeCommands, got nil")
	}
}
