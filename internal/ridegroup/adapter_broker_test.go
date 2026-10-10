package ridegroup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
)

func TestAdapterLocalBrokerHandshakeActivityAndPosition(t *testing.T) {
	brokerPath, err := exec.LookPath("mosquitto")
	if err != nil {
		t.Skip("mosquitto is unavailable")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	broker := exec.Command(brokerPath, "-p", strconv.Itoa(port))
	broker.Stdout, broker.Stderr = io.Discard, io.Discard
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Process.Kill(); _ = broker.Wait() })
	address := fmt.Sprintf("tcp://127.0.0.1:%d", port)

	observer := connectTestMQTT(t, address, "ride-group-observer")
	defer observer.Disconnect(100)
	messages := make(chan mqtt.Message, 32)
	if token := observer.Subscribe("scooters/vehicle-1/ride_group/up", 1, func(_ mqtt.Client, message mqtt.Message) {
		select {
		case messages <- message:
		default:
		}
	}); !token.WaitTimeout(2*time.Second) || token.Error() != nil {
		t.Fatal("observer subscribe failed")
	}
	device := connectTestMQTT(t, address, "ride-group-device")
	defer device.Disconnect(100)

	r := newTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter, err := NewAdapter(ctx, Config{Enabled: true, Identifier: "vehicle-1"}, r.client)
	if err != nil {
		t.Fatal(err)
	}
	controlSub := r.client.Subscribe(ctx, controlChannel)
	defer controlSub.Close()
	if _, err := controlSub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	adapter.Start()
	defer adapter.Stop()
	waitFor(t, func() bool { adapter.mu.Lock(); defer adapter.mu.Unlock(); return adapter.settingsReady })
	adapter.OnConnected(device)
	helloMessage := receiveBrokerMessage(t, messages)
	var hello map[string]any
	if err := json.Unmarshal(helloMessage.Payload(), &hello); err != nil {
		t.Fatal(err)
	}
	if hello["type"] != "ride_group_hello" || helloMessage.Topic() != "scooters/vehicle-1/ride_group/up" || helloMessage.Retained() {
		t.Fatalf("invalid hello: %v", hello)
	}
	connection := hello["connection_nonce"].(string)

	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: true, Challenge: testChallenge}
	publishJSON(t, r.client, activityChannel, activity)
	wireActivity := receiveBrokerMessage(t, messages)
	var wire map[string]any
	if err := json.Unmarshal(wireActivity.Payload(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire["type"] != "ride_group_activity" || wire["connection_nonce"] != connection || wire["boot_nonce"] != testBoot || wire["seq"] != float64(1) {
		t.Fatalf("activity was not forwarded exactly: %v", wire)
	}

	id, member, stream := testSession, testMember, testStream
	control := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: connection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 1, TransportEpoch: 1, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: time.Now().UnixMilli(), LeaseTTLMS: 10000, CadenceMS: durationPointer(1000), Peers: []PeerDescriptor{}}
	controlPayload, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	publishMQTTJSON(t, observer, "scooters/vehicle-1/ride_group/down", 1, control)
	ipcContext, ipcCancel := context.WithTimeout(ctx, 2*time.Second)
	defer ipcCancel()
	controlMessage, err := controlSub.ReceiveMessage(ipcContext)
	if err != nil {
		t.Fatal("control IPC was not emitted")
	}
	if controlMessage.Payload != string(controlPayload) {
		t.Fatal("control envelope was not forwarded byte-for-byte")
	}
	waitFor(t, func() bool { adapter.mu.Lock(); defer adapter.mu.Unlock(); return adapter.grant != nil })
	time.Sleep(time.Second)
	secondActivity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 2, Active: true, SessionID: &id, MemberID: &member, Challenge: "00000000-0000-4000-8000-000000000007"}
	publishJSON(t, r.client, activityChannel, secondActivity)
	secondWireActivity := receiveBrokerMessage(t, messages)
	if secondWireActivity.Topic() != "scooters/vehicle-1/ride_group/up" {
		t.Fatalf("second heartbeat topic = %s", secondWireActivity.Topic())
	}

	gpsTime := time.Now().UTC().Truncate(time.Second)
	gps := map[string]any{"latitude": "0.000000", "longitude": "0.000000", "timestamp": gpsTime.Format(time.RFC3339), "active": true, "connected": true, "state": "fix-established", "fix": "3d", "eph": 3.5, "speed": "35.0", "course": "180.0", "updated": gpsTime.Add(time.Second).Format(time.RFC3339)}
	publishJSON(t, r.client, gpsChannel, gps)
	positionMessage := receiveBrokerMessage(t, messages)
	if positionMessage.Topic() != "scooters/vehicle-1/ride_group/up" || positionMessage.Qos() != 0 || positionMessage.Retained() {
		t.Fatalf("position transport qos/retention invalid: topic=%s qos=%d retained=%v", positionMessage.Topic(), positionMessage.Qos(), positionMessage.Retained())
	}
	position, err := ParsePosition(positionMessage.Payload())
	if err != nil {
		t.Fatalf("position invalid: %v", err)
	}
	if position.TimestampSource != "producer_fix" || position.Latitude != 0 || position.Longitude != 0 || position.MotionSource != "unknown" {
		t.Fatalf("position fields incorrect: %+v", position)
	}
	if err := r.client.HSet(ctx, "settings", "ride-group.blocked", "true").Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.Publish(ctx, settingsChannel, "ride-group.blocked").Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.blocked && adapter.grant == nil
	})
	stopMessage := receiveBrokerMessage(t, messages)
	var stop Activity
	if err := json.Unmarshal(stopMessage.Payload(), &stop); err != nil {
		t.Fatal(err)
	}
	if stop.Type != "ride_group_activity" || stop.Active || !stop.LocalStop || stop.SessionID != nil || stop.MemberID != nil {
		t.Fatal("persisted local block did not publish an inactive local stop")
	}
}

func connectTestMQTT(t *testing.T, address, clientID string) mqtt.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		options := mqtt.NewClientOptions().AddBroker(address).SetClientID(clientID).SetAutoReconnect(false).SetConnectRetry(false).SetConnectTimeout(500 * time.Millisecond)
		client := mqtt.NewClient(options)
		token := client.Connect()
		if token.WaitTimeout(750*time.Millisecond) && token.Error() == nil {
			return client
		}
		lastErr = token.Error()
		client.Disconnect(0)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("local broker connect failed: %v", lastErr)
	return nil
}

func receiveBrokerMessage(t *testing.T, messages <-chan mqtt.Message) mqtt.Message {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for local broker message")
		return nil
	}
}

func publishMQTTJSON(t *testing.T, client mqtt.Client, topic string, qos byte, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	token := client.Publish(topic, qos, false, payload)
	if !token.WaitTimeout(2*time.Second) || token.Error() != nil {
		t.Fatal("local broker publish failed")
	}
}

func publishJSON(t *testing.T, client *redis.Client, channel string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), channel, payload).Err(); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for local ride-group state")
}
