package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/handlers"
	"radio-gaga/internal/models"
)

type updateLaneMQTT struct {
	mqtt.Client
	responses chan models.CommandResponse
	publishes atomic.Int32
	token     mqtt.Token
	published chan struct{}
}

func (c *updateLaneMQTT) IsConnectionOpen() bool { return true }
func (c *updateLaneMQTT) Publish(topic string, _ byte, _ bool, payload interface{}) mqtt.Token {
	c.publishes.Add(1)
	if c.published != nil {
		c.published <- struct{}{}
	}
	if strings.HasSuffix(topic, "/acks") {
		var response models.CommandResponse
		if err := json.Unmarshal(payload.([]byte), &response); err != nil {
			panic(err)
		}
		c.responses <- response
	}
	if c.token != nil {
		return c.token
	}
	return &fakeToken{}
}

func laneCommand(command, requestID string, params map[string]interface{}) *acknowledgedCommandMessage {
	payload, _ := json.Marshal(models.CommandMessage{Command: command, RequestID: requestID, Params: params})
	return &acknowledgedCommandMessage{fakeMessage: fakeMessage{topic: "scooters/test-scooter/commands", payload: payload}, acknowledged: make(chan struct{})}
}

func waitLaneAck(t *testing.T, message *acknowledgedCommandMessage) {
	t.Helper()
	select {
	case <-message.acknowledged:
	case <-time.After(time.Second):
		t.Fatal("command delivery not acknowledged")
	}
}

func waitLaneResponse(t *testing.T, client *updateLaneMQTT, id, status string) models.CommandResponse {
	t.Helper()
	select {
	case response := <-client.responses:
		if response.RequestID != id || response.Status != status {
			t.Fatalf("response=%+v want=%s/%s", response, id, status)
		}
		return response
	case <-time.After(time.Second):
		t.Fatalf("no application response for %s", id)
	}
	return models.CommandResponse{}
}

func newLaneClient(t *testing.T) (*ScooterMQTTClient, *updateLaneMQTT) {
	t.Helper()
	s, _ := newTestClient(t)
	s.ctx, s.cancel = context.WithCancel(s.ctx)
	s.commandQueue = make(chan queuedCommand, commandQueueCapacity)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	s.config.StateDir = t.TempDir()
	client := &updateLaneMQTT{responses: make(chan models.CommandResponse, 32)}
	s.setActiveMQTTClient(client)
	t.Cleanup(func() {
		s.cancel()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("self-update worker survived shutdown")
		}
	})
	return s, client
}

func TestSelfUpdateLaneKeepsCommandsResponsiveDuringStalledDownload(t *testing.T) {
	s, client := newLaneClient(t)
	started, release := make(chan struct{}), make(chan struct{})
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Header().Set("Content-Length", "9")
		_, _ = w.Write([]byte("candi"))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
			_, _ = w.Write([]byte("date"))
		case <-r.Context().Done():
		}
	}))
	defer func() { s.cancel(); server.Close() }()
	s.startCommandWorker()
	params := map[string]interface{}{"url": server.URL, "checksum": "sha256:" + strings.Repeat("0", 64)}
	update := laneCommand("self_update", "update", params)
	s.handleCommand(client, update)
	<-started
	duplicate := laneCommand("self_update", "update", params)
	s.handleCommand(client, duplicate)
	waitLaneAck(t, duplicate)
	select {
	case result := <-client.responses:
		t.Fatalf("duplicate got premature application response: %+v", result)
	default:
	}
	busy := laneCommand("self_update", "other-update", params)
	s.handleCommand(client, busy)
	waitLaneResponse(t, client, "other-update", "error")
	waitLaneAck(t, busy)
	for _, command := range []string{"txn:replace", "config:set", "config:del", "config:save", "update_ca_cert", "migrate_broker"} {
		message := laneCommand(command, command, nil)
		s.handleCommand(client, message)
		response := waitLaneResponse(t, client, command, "error")
		if !strings.Contains(response.Error, "self-update in progress") {
			t.Fatalf("maintenance gate: %+v", response)
		}
		waitLaneAck(t, message)
	}
	for _, id := range []string{"ping-1", "ping-2"} {
		ping := laneCommand("ping", id, nil)
		s.handleCommand(client, ping)
		waitLaneResponse(t, client, id, "success")
		waitLaneAck(t, ping)
	}
	select {
	case <-update.acknowledged:
		t.Fatal("original delivery ACKed before update completed")
	default:
	}
	close(release)
	waitLaneResponse(t, client, "update", "error")
	waitLaneAck(t, update)
	redelivery := laneCommand("self_update", "update", params)
	s.handleCommand(client, redelivery)
	waitLaneResponse(t, client, "update", "error")
	waitLaneAck(t, redelivery)
	if downloads.Load() != 1 {
		t.Fatalf("duplicate transfers: %d", downloads.Load())
	}
}

func TestSelfUpdateLaneReconnectDoesNotLaunchSecondJobOrPublishThroughOldClient(t *testing.T) {
	s, old := newLaneClient(t)
	started, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	s.startCommandWorkerWithSelfUpdate(func(client handlers.CommandHandlerClient, _ mqtt.Message) {
		runs.Add(1)
		close(started)
		<-release
		client.SendCommandResponse("update", "success", "")
	})
	params := map[string]interface{}{"url": "https://example.test/binary", "checksum": "sha256:" + strings.Repeat("0", 64)}
	original := laneCommand("self_update", "update", params)
	s.handleCommand(old, original)
	<-started
	current := &updateLaneMQTT{responses: make(chan models.CommandResponse, 8)}
	s.setActiveMQTTClient(current)
	duplicate := laneCommand("self_update", "update", params)
	s.handleCommand(current, duplicate)
	waitLaneAck(t, duplicate)
	ping := laneCommand("ping", "live", nil)
	s.handleCommand(current, ping)
	waitLaneResponse(t, current, "live", "success")
	waitLaneAck(t, ping)
	if runs.Load() != 1 {
		t.Fatalf("reconnect spawned %d jobs", runs.Load())
	}
	select {
	case response := <-current.responses:
		t.Fatalf("premature result: %+v", response)
	default:
	}
	close(release)
	waitLaneResponse(t, current, "update", "success")
	s.cancel()
	s.wg.Wait()
	if old.publishes.Load() != 0 {
		t.Fatal("update published through stale MQTT connection")
	}
	select {
	case <-original.acknowledged:
		t.Fatal("original connection delivery acknowledged after reconnect")
	default:
	}
}

func TestSelfUpdateLaneShutdownCancelsDownloadAndJoinsWorker(t *testing.T) {
	s, client := newLaneClient(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer func() { s.cancel(); server.Close() }()
	s.startCommandWorker()
	message := laneCommand("self_update", "update", map[string]interface{}{"url": server.URL, "checksum": "sha256:" + strings.Repeat("0", 64)})
	s.handleCommand(client, message)
	<-started
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("download did not stop on shutdown")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
	if client.publishes.Load() != 0 {
		t.Fatal("shutdown updater published a response")
	}
	select {
	case <-message.acknowledged:
		t.Fatal("canceled update acknowledged")
	default:
	}
}

func TestSelfUpdateResponseWaitIsCanceledByShutdown(t *testing.T) {
	s, client := newLaneClient(t)
	pending := &pendingUpdateToken{done: make(chan struct{})}
	client.token = pending
	client.published = make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- s.publishSelfUpdate("test", false, []byte("response")) }()
	// Publish enqueues without blocking the connection-generation lock.
	<-client.published
	s.setActiveMQTTClient(&updateLaneMQTT{responses: make(chan models.CommandResponse, 1)})
	s.cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("publish error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("publish wait survived cancellation")
	}
}

type pendingUpdateToken struct{ done chan struct{} }

func (t *pendingUpdateToken) Wait() bool                   { <-t.done; return true }
func (*pendingUpdateToken) WaitTimeout(time.Duration) bool { return false }
func (t *pendingUpdateToken) Done() <-chan struct{}        { return t.done }
func (*pendingUpdateToken) Error() error                   { return nil }

func TestSelfUpdateDeliveryAckIsGenerationScoped(t *testing.T) {
	s, old := newLaneClient(t)
	message := laneCommand("self_update", "old", nil)
	s.setActiveMQTTClient(&updateLaneMQTT{responses: make(chan models.CommandResponse, 1)})
	s.ackSelfUpdateDelivery(queuedCommand{client: old, message: message})
	select {
	case <-message.acknowledged:
		t.Fatal("stale generation ACKed")
	default:
	}
	current := s.activeMQTTClient()
	s.ackSelfUpdateDelivery(queuedCommand{client: current, message: message})
	waitLaneAck(t, message)
	s.cancel()
	canceled := laneCommand("self_update", "canceled", nil)
	s.ackSelfUpdateDelivery(queuedCommand{client: current, message: canceled})
	select {
	case <-canceled.acknowledged:
		t.Fatal("shutdown delivery ACKed")
	default:
	}
}

func TestSelfUpdateLaneDoesNotGateVehicleCommands(t *testing.T) {
	for _, command := range []string{"lock", "unlock", "blinkers", "honk", "alarm", "open_seatbox", "ping", "get_state", "restart"} {
		if selfUpdateConflictsWithCommand(command) {
			t.Fatal(fmt.Sprintf("vehicle command %s gated", command))
		}
	}
}
