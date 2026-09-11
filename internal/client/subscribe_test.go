package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
	"radio-gaga/internal/models"
)

// fakeToken is a minimal mqtt.Token used to drive subscription and status
// publishing in tests.
type fakeToken struct {
	err    error
	result map[string]byte
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeToken) Error() error            { return t.err }
func (t *fakeToken) Result() map[string]byte { return t.result }

// fakeMQTTClient embeds mqtt.Client so only the methods we exercise need real
// implementations; any other call would panic, which is fine for these tests.
type fakeMQTTClient struct {
	mqtt.Client
	subTopic       string
	subQoS         byte
	subErr         error
	subReturnCodes []byte
	subResults     []map[string]byte
	subErrors      []error
	subCalls       int
	publishErr     error
	publishCalls   int
	connectionOpen bool
	calls          []string
}

func (c *fakeMQTTClient) Subscribe(topic string, qos byte, _ mqtt.MessageHandler) mqtt.Token {
	call := c.subCalls
	c.subCalls++
	c.subTopic = topic
	c.subQoS = qos
	c.calls = append(c.calls, "subscribe")

	err := c.subErr
	if call < len(c.subErrors) {
		err = c.subErrors[call]
	}
	returnCode := qos
	if call < len(c.subReturnCodes) {
		returnCode = c.subReturnCodes[call]
	}
	result := map[string]byte{topic: returnCode}
	if call < len(c.subResults) {
		result = c.subResults[call]
	}
	return &fakeToken{err: err, result: result}
}

func (c *fakeMQTTClient) Publish(string, byte, bool, interface{}) mqtt.Token {
	c.publishCalls++
	c.calls = append(c.calls, "publish")
	return &fakeToken{err: c.publishErr}
}

func (c *fakeMQTTClient) IsConnectionOpen() bool { return c.connectionOpen }

func newTestClient(t *testing.T) (*ScooterMQTTClient, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	cfg := &models.Config{}
	cfg.Scooter.Identifier = "test-scooter"
	return &ScooterMQTTClient{
		config:      cfg,
		redisClient: redisClient,
		ctx:         context.Background(),
	}, server
}

func TestSubscribeCommands_Success(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{}

	s.setActiveMQTTClient(fc)
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
	if !s.isCommandSubscriptionReady() {
		t.Fatal("command subscription is not ready after successful SUBACK")
	}
	assertRadioGagaStatus(t, server, "connected")
}

func TestSubscribeCommands_SUBACKRejection(t *testing.T) {
	tests := []struct {
		name   string
		result map[string]byte
	}{
		{
			name:   "failure code 0x80",
			result: map[string]byte{"scooters/test-scooter/commands": 0x80},
		},
		{
			name:   "omitted topic",
			result: map[string]byte{},
		},
		{
			name:   "invalid code 0x03",
			result: map[string]byte{"scooters/test-scooter/commands": 0x03},
		},
		{
			name:   "invalid code 0x81",
			result: map[string]byte{"scooters/test-scooter/commands": 0x81},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, server := newTestClient(t)
			fc := &fakeMQTTClient{subResults: []map[string]byte{tt.result}}
			s.setActiveMQTTClient(fc)

			err := s.subscribeCommands(fc)
			if err == nil {
				t.Fatal("expected rejected SUBACK to fail subscription")
			}
			if !errors.Is(err, errCommandSubscriptionRejected) {
				t.Fatalf("subscription error = %v, want broker rejection", err)
			}
			if s.isCommandSubscriptionReady() {
				t.Fatal("command subscription is ready after rejected SUBACK")
			}
			assertRadioGagaStatus(t, server, "disconnected")
		})
	}
}

func TestSubscribeCommandsWithRetry_RecoversFromTransientFailure(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{
		connectionOpen: true,
		subErrors:      []error{errors.New("transient transport failure"), nil},
	}

	s.setActiveMQTTClient(fc)
	if err := s.subscribeCommandsWithRetry(fc, 2, 0); err != nil {
		t.Fatalf("subscribeCommandsWithRetry returned error: %v", err)
	}
	if fc.subCalls != 2 {
		t.Fatalf("Subscribe calls = %d, want 2", fc.subCalls)
	}
	if !s.isCommandSubscriptionReady() {
		t.Fatal("command subscription is not ready after retry succeeded")
	}
	assertRadioGagaStatus(t, server, "connected")
}

func TestSubscribeCommandsWithRetry_DoesNotRetryPermanentRejection(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{
		connectionOpen: true,
		subResults: []map[string]byte{
			{"scooters/test-scooter/commands": commandSubscriptionRejected},
			{"scooters/test-scooter/commands": 1},
		},
	}
	s.setActiveMQTTClient(fc)

	err := s.subscribeCommandsWithRetry(fc, commandSubscriptionAttempts, 0)
	if !errors.Is(err, errCommandSubscriptionRejected) {
		t.Fatalf("subscription error = %v, want permanent broker rejection", err)
	}
	if fc.subCalls != 1 {
		t.Fatalf("Subscribe calls = %d, want 1 for permanent rejection", fc.subCalls)
	}
	if s.activeMQTTClient() != fc {
		t.Fatal("permanent rejection replaced the active MQTT client")
	}
	assertRadioGagaStatus(t, server, "disconnected")
}

func TestHandleMQTTConnected_PermanentRejectionDoesNotReconnect(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{
		connectionOpen: true,
		subResults: []map[string]byte{
			{"scooters/test-scooter/commands": commandSubscriptionRejected},
			{"scooters/test-scooter/commands": 1},
		},
	}
	s.setActiveMQTTClient(fc)

	s.handleMQTTConnected(fc)
	if fc.subCalls != 1 {
		t.Fatalf("Subscribe calls = %d, want 1 for permanent rejection", fc.subCalls)
	}
	if fc.publishCalls != 0 {
		t.Fatalf("status publishes = %d, want 0", fc.publishCalls)
	}
	if s.activeMQTTClient() != fc {
		t.Fatal("permanent rejection replaced the active MQTT client")
	}
	assertRadioGagaStatus(t, server, "disconnected")
}

func TestSubscribeCommandsWithRetry_IsBounded(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{
		connectionOpen: true,
		subErrors: []error{
			errors.New("transient transport failure 1"),
			errors.New("transient transport failure 2"),
			errors.New("transient transport failure 3"),
			nil,
		},
	}

	s.setActiveMQTTClient(fc)
	if err := s.subscribeCommandsWithRetry(fc, 3, 0); err == nil {
		t.Fatal("expected bounded retries to return the final subscription error")
	}
	if fc.subCalls != 3 {
		t.Fatalf("Subscribe calls = %d, want bounded total of 3", fc.subCalls)
	}
	if s.isCommandSubscriptionReady() {
		t.Fatal("command subscription is ready after all retries failed")
	}
	assertRadioGagaStatus(t, server, "disconnected")
}

func TestCommandSubscriptionReadiness_FailureThenSuccess(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{subErr: errors.New("broker refused")}

	s.setActiveMQTTClient(fc)
	if err := s.subscribeCommands(fc); err == nil {
		t.Fatal("expected error from subscribeCommands, got nil")
	}
	if s.isCommandSubscriptionReady() {
		t.Fatal("command subscription is ready after failed SUBACK")
	}
	assertRadioGagaStatus(t, server, "disconnected")

	// A successful generic outbound publish must not claim command reachability
	// while the command subscription is unavailable.
	s.updateCloudStatus()
	assertRadioGagaStatus(t, server, "disconnected")

	fc.subErr = nil
	if err := s.subscribeCommands(fc); err != nil {
		t.Fatalf("second subscribeCommands returned error: %v", err)
	}
	if !s.isCommandSubscriptionReady() {
		t.Fatal("command subscription is not ready after later successful SUBACK")
	}
	assertRadioGagaStatus(t, server, "connected")
}

func TestHandleMQTTConnected_PublishesConnectedOnlyAfterSubscription(t *testing.T) {
	s, server := newTestClient(t)
	fc := &fakeMQTTClient{subErr: errors.New("broker refused")}

	s.setActiveMQTTClient(fc)
	s.handleMQTTConnected(fc)
	if fc.publishCalls != 0 {
		t.Fatalf("published connected status after failed subscription: %d publishes", fc.publishCalls)
	}
	assertRadioGagaStatus(t, server, "disconnected")

	fc.subErr = nil
	s.handleMQTTConnected(fc)
	if fc.publishCalls != 1 {
		t.Fatalf("connected status publishes = %d, want 1", fc.publishCalls)
	}
	if got, want := fc.calls, []string{"subscribe", "subscribe", "publish"}; !equalStrings(got, want) {
		t.Fatalf("MQTT call order = %v, want %v", got, want)
	}
	assertRadioGagaStatus(t, server, "connected")
}

func TestCommandSubscriptionReadiness_ClearedByConnectionLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		clear func(*ScooterMQTTClient, mqtt.Client)
	}{
		{
			name: "connection lost",
			clear: func(s *ScooterMQTTClient, client mqtt.Client) {
				s.handleMQTTConnectionLost(client, errors.New("connection reset"))
			},
		},
		{
			name: "reconnect start",
			clear: func(s *ScooterMQTTClient, client mqtt.Client) {
				s.handleMQTTReconnectStart(client)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, server := newTestClient(t)
			client := &fakeMQTTClient{}
			s.setActiveMQTTClient(client)
			if err := s.subscribeCommands(client); err != nil {
				t.Fatalf("subscribeCommands returned error: %v", err)
			}

			tt.clear(s, client)
			if s.isCommandSubscriptionReady() {
				t.Fatal("command subscription remained ready")
			}
			assertRadioGagaStatus(t, server, "disconnected")

			s.updateCloudStatus()
			assertRadioGagaStatus(t, server, "disconnected")
		})
	}
}

func TestCommandSubscriptionReadiness_IgnoresDelayedLifecycleCallback(t *testing.T) {
	t.Run("same client reopened", func(t *testing.T) {
		s, server := newTestClient(t)
		client := &fakeMQTTClient{connectionOpen: true}
		s.setActiveMQTTClient(client)
		if err := s.subscribeCommands(client); err != nil {
			t.Fatalf("subscribeCommands returned error: %v", err)
		}

		s.handleMQTTConnectionLost(client, errors.New("delayed loss"))
		s.handleMQTTReconnectStart(client)
		if !s.isCommandSubscriptionReady() {
			t.Fatal("delayed callbacks cleared readiness for reopened client")
		}
		assertRadioGagaStatus(t, server, "connected")
	})

	t.Run("replaced client", func(t *testing.T) {
		s, server := newTestClient(t)
		oldClient := &fakeMQTTClient{connectionOpen: true}
		newClient := &fakeMQTTClient{connectionOpen: true}
		s.setActiveMQTTClient(oldClient)
		if err := s.subscribeCommands(oldClient); err != nil {
			t.Fatalf("old subscribeCommands returned error: %v", err)
		}
		s.setActiveMQTTClient(newClient)
		if err := s.subscribeCommands(newClient); err != nil {
			t.Fatalf("new subscribeCommands returned error: %v", err)
		}

		oldSubscribeCalls := oldClient.subCalls
		s.handleMQTTConnected(oldClient)
		if oldClient.subCalls != oldSubscribeCalls {
			t.Fatalf("delayed old OnConnect subscribed %d additional times", oldClient.subCalls-oldSubscribeCalls)
		}
		if !s.isCommandSubscriptionReady() {
			t.Fatal("old client's OnConnect callback cleared new client readiness")
		}
		assertRadioGagaStatus(t, server, "connected")

		s.handleMQTTConnectionLost(oldClient, errors.New("delayed old loss"))
		if !s.isCommandSubscriptionReady() {
			t.Fatal("old client's loss callback cleared new client readiness")
		}
		assertRadioGagaStatus(t, server, "connected")
	})
}

func TestHandleCommandUsesCallbackClient(t *testing.T) {
	message := &fakeMessage{
		topic:   "scooters/test-scooter/commands",
		payload: []byte(`{"command":"ping","request_id":"request-1"}`),
	}

	t.Run("before active client assignment", func(t *testing.T) {
		s, _ := newTestClient(t)
		callbackClient := &fakeMQTTClient{}

		s.handleCommand(callbackClient, message)
		if callbackClient.publishCalls != 1 {
			t.Fatalf("callback client publishes = %d, want 1", callbackClient.publishCalls)
		}
		if s.activeMQTTClient() != nil {
			t.Fatal("test unexpectedly had an active MQTT client")
		}
	})

	t.Run("while active field still references old client", func(t *testing.T) {
		s, _ := newTestClient(t)
		oldClient := &fakeMQTTClient{}
		callbackClient := &fakeMQTTClient{}
		s.setActiveMQTTClient(oldClient)

		s.handleCommand(callbackClient, message)
		if callbackClient.publishCalls != 1 {
			t.Fatalf("callback client publishes = %d, want 1", callbackClient.publishCalls)
		}
		if oldClient.publishCalls != 0 {
			t.Fatalf("old active client publishes = %d, want 0", oldClient.publishCalls)
		}
	})
}

type fakeMessage struct {
	topic    string
	payload  []byte
	retained bool
}

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 1 }
func (m *fakeMessage) Retained() bool    { return m.retained }
func (m *fakeMessage) Topic() string     { return m.topic }
func (m *fakeMessage) MessageID() uint16 { return 1 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              {}

func assertRadioGagaStatus(t *testing.T, server *miniredis.Miniredis, want string) {
	t.Helper()
	if got := server.HGet("remote-access", "radio-gaga"); got != want {
		t.Fatalf("remote-access.radio-gaga = %q, want %q", got, want)
	}
	if got := server.HGet("remote-access", "status"); got != want {
		t.Fatalf("remote-access.status = %q, want %q", got, want)
	}
	if got := server.HGet("internet", "unu-cloud"); got != want {
		t.Fatalf("internet.unu-cloud = %q, want %q", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
