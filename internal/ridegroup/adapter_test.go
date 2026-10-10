package ridegroup

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
)

type tokenResult struct {
	err  error
	done chan struct{}
}

func newTokenResult() *tokenResult {
	ch := make(chan struct{})
	close(ch)
	return &tokenResult{done: ch}
}
func (t *tokenResult) Wait() bool                     { return true }
func (t *tokenResult) WaitTimeout(time.Duration) bool { return true }
func (t *tokenResult) Done() <-chan struct{}          { return t.done }
func (t *tokenResult) Error() error                   { return t.err }

type publishRecord struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}
type mqttFake struct {
	mqtt.Client
	mu         sync.Mutex
	connected  bool
	subscribed []string
	published  []publishRecord
}

type blockedPublishMQTT struct {
	*mqttFake
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (m *blockedPublishMQTT) Publish(string, byte, bool, interface{}) mqtt.Token {
	m.calls++
	select {
	case m.entered <- struct{}{}:
	default:
	}
	<-m.release
	return newTokenResult()
}

type mqttMessageFake struct {
	mu       sync.Mutex
	payload  []byte
	qos      byte
	retained bool
	acks     int
}

type blockedAckMessage struct {
	*mqttMessageFake
	entered chan struct{}
	release chan struct{}
}

func (m *blockedAckMessage) Ack() {
	close(m.entered)
	<-m.release
	m.mqttMessageFake.Ack()
}

func (m *mqttMessageFake) Duplicate() bool   { return false }
func (m *mqttMessageFake) Qos() byte         { return m.qos }
func (m *mqttMessageFake) Retained() bool    { return m.retained }
func (m *mqttMessageFake) Topic() string     { return "scooters/vehicle-1/ride_group/down" }
func (m *mqttMessageFake) MessageID() uint16 { return 1 }
func (m *mqttMessageFake) Payload() []byte   { return m.payload }
func (m *mqttMessageFake) Ack() {
	m.mu.Lock()
	m.acks++
	m.mu.Unlock()
}
func (m *mqttMessageFake) ackCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acks
}

func (m *mqttFake) IsConnected() bool { return m.connected }
func (m *mqttFake) Subscribe(topic string, _ byte, _ mqtt.MessageHandler) mqtt.Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscribed = append(m.subscribed, topic)
	return newTokenResult()
}
func (m *mqttFake) Unsubscribe(...string) mqtt.Token { return newTokenResult() }
func (m *mqttFake) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, _ := payload.([]byte)
	m.published = append(m.published, publishRecord{topic: topic, qos: qos, retained: retained, payload: append([]byte(nil), body...)})
	return newTokenResult()
}

type testRedis struct {
	server *miniredis.Miniredis
	client *redis.Client
}

func newTestRedis(t *testing.T) testRedis {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return testRedis{server: server, client: client}
}

const (
	testConnection = "00000000-0000-4000-8000-000000000004"
	testBoot       = "00000000-0000-4000-8000-000000000005"
	testChallenge  = "00000000-0000-4000-8000-000000000006"
	testSession    = "00000000-0000-4000-8000-000000000002"
	testMember     = "00000000-0000-4000-8000-000000000003"
	testStream     = "00000000-0000-4000-8000-000000000001"
)

func TestOnConnectedUsesOnlyDeviceTopicsAndAdvertisesHello(t *testing.T) {
	r := newTestRedis(t)
	a := &Adapter{config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client}
	cleanupAdapter(t, a)
	client := &mqttFake{connected: true}
	a.OnConnected(client)
	waitForPublished(t, client, 1)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.subscribed) != 1 || client.subscribed[0] != "scooters/vehicle-1/ride_group/down" {
		t.Fatalf("subscriptions = %v", client.subscribed)
	}
	if len(client.published) != 1 {
		t.Fatalf("hello publishes = %d", len(client.published))
	}
	record := client.published[0]
	if record.topic != "scooters/vehicle-1/ride_group/up" || record.qos != 0 || record.retained {
		t.Fatalf("hello publish = %+v", record)
	}
	var hello struct {
		Version      int      `json:"v"`
		Type         string   `json:"type"`
		Nonce        string   `json:"connection_nonce"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(record.payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Version != 1 || hello.Type != "ride_group_hello" || !validUUID(hello.Nonce) || len(hello.Capabilities) != 1 || hello.Capabilities[0] != "ride_group_v1" {
		t.Fatalf("hello = %+v", hello)
	}
}

func TestActiveDiscoveryRefreshesExpiredCapabilityWithoutIdleWakeups(t *testing.T) {
	r := newTestRedis(t)
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, clock: func() time.Time { return now }, settingsReady: true}
	cleanupAdapter(t, a)
	client := &mqttFake{connected: true}
	a.OnConnected(client)
	waitForPublished(t, client, 1)
	now = now.Add(31 * time.Second)
	time.Sleep(20 * time.Millisecond)
	client.mu.Lock()
	idleCount := len(client.published)
	client.mu.Unlock()
	if idleCount != 1 {
		t.Fatal("idle elapsed time caused an unsolicited handshake")
	}
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: true, Challenge: testChallenge}
	payload, _ := json.Marshal(activity)
	a.handleActivity(payload)
	waitForPublished(t, client, 3)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != 3 || string(client.published[1].payload) == "" || string(client.published[2].payload) == "" {
		t.Fatalf("active handshake publications = %d", len(client.published))
	}
}

func TestDownlinkAcknowledgesQoSOneAndDropsRetained(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true}
	cleanupAdapter(t, a)
	retained := &mqttMessageFake{qos: 1, retained: true, payload: []byte(`{}`)}
	a.handleDownlink(client, retained)
	waitFor(t, func() bool { return retained.ackCount() == 1 })
	if a.grant != nil {
		t.Fatal("retained QoS1 message was not acknowledged and dropped")
	}
	invalid := &mqttMessageFake{qos: 1, payload: []byte(`{}`)}
	a.handleDownlink(client, invalid)
	waitFor(t, func() bool { return invalid.ackCount() == 1 })
	if invalid.ackCount() != 1 {
		t.Fatal("invalid QoS1 message was not acknowledged")
	}
	qosZero := &mqttMessageFake{qos: 0, payload: []byte(`{}`)}
	a.handleDownlink(client, qosZero)
	if qosZero.ackCount() != 0 {
		t.Fatal("QoS0 message was acknowledged")
	}
}

func TestBlockedQoS1AcknowledgmentDoesNotBlockCallbackOrShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &mqttFake{connected: true}
	a := &Adapter{ctx: ctx, cancel: cancel, config: Config{Enabled: true, Identifier: "vehicle-1"}, mqtt: client, connection: testConnection}
	cleanupAdapter(t, a)
	message := &blockedAckMessage{mqttMessageFake: &mqttMessageFake{qos: 1, retained: true}, entered: make(chan struct{}), release: make(chan struct{})}
	started := time.Now()
	a.handleDownlink(client, message)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("ordered MQTT callback waited for acknowledgment transport")
	}
	select {
	case <-message.entered:
	case <-time.After(time.Second):
		t.Fatal("manual acknowledgment worker did not start")
	}
	started = time.Now()
	a.Stop()
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("shutdown waited for blocked QoS1 acknowledgment")
	}
	close(message.release)
}

func TestClearingGrantDropsQueuedPositions(t *testing.T) {
	a := &Adapter{mqttPending: map[string]mqttPublication{"position": {kind: "position"}}}
	a.mu.Lock()
	a.clearGrantLocked()
	a.mu.Unlock()
	a.egressMu.Lock()
	_, queued := a.mqttPending["position"]
	a.egressMu.Unlock()
	if queued {
		t.Fatal("queued position survived grant clearing")
	}
}

func TestBlockedPublishDoesNotHoldStateOrSpawnWriters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &blockedPublishMQTT{mqttFake: &mqttFake{connected: true}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	now := time.Now()
	a := &Adapter{ctx: ctx, cancel: cancel, config: Config{Enabled: true, Identifier: "vehicle-1"}, mqtt: client, connection: testConnection, settingsReady: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	a.queueMQTTPublication(mqttPublication{client: client, connection: testConnection, topic: "scooters/vehicle-1/ride_group/up", payload: []byte("position"), kind: "position", priority: 1})
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("publisher did not enter blocked Publish")
	}
	for i := 0; i < 100; i++ {
		a.queueMQTTPublication(mqttPublication{client: client, connection: testConnection, topic: "scooters/vehicle-1/ride_group/up", payload: []byte("latest-position"), kind: "position", priority: 1})
	}
	a.egressMu.Lock()
	pendingCount := len(a.mqttPending)
	a.egressMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("bounded latest-only MQTT queue retained %d pending publications", pendingCount)
	}
	started := time.Now()
	a.mu.Lock()
	a.activity = &Activity{Active: true, BootNonce: testBoot, Sequence: 1}
	a.activityAt = now.Add(-11 * time.Second)
	a.grant = &Control{SessionID: stringPointer(testSession), MemberID: stringPointer(testMember), StreamID: stringPointer(testStream)}
	a.grantAt, a.grantTTL, a.grantActive = now.Add(-11*time.Second), 10*time.Second, true
	a.mu.Unlock()
	a.expireLease()
	stop := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 2, Active: false, LocalStop: true, Challenge: "00000000-0000-4000-8000-000000000009"}
	stopPayload, _ := json.Marshal(stop)
	a.handleActivity(stopPayload)
	a.OnDisconnected(client)
	if time.Since(started) > 200*time.Millisecond {
		t.Fatal("lease expiry, local stop, or disconnect waited for blocked Publish")
	}
	started = time.Now()
	a.Stop()
	if time.Since(started) > 200*time.Millisecond {
		t.Fatal("shutdown waited for blocked Publish")
	}
	close(client.release)
	a.egressMu.Lock()
	done := a.mqttWorkerDone
	a.egressMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded MQTT worker did not exit after the in-flight call returned")
	}
	if client.calls != 1 {
		t.Fatalf("concurrent or repeated Publish calls = %d", client.calls)
	}
}

func TestReconnectRotatesNonceAndDropsPriorGrant(t *testing.T) {
	r := newTestRedis(t)
	a := &Adapter{config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client}
	cleanupAdapter(t, a)
	client := &mqttFake{connected: true}
	a.OnConnected(client)
	waitForPublished(t, client, 1)
	firstNonce := a.connection
	id, member, stream := testSession, testMember, testStream
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1}
	a.grantAt = time.Now()
	a.grantTTL = time.Second
	a.grantActive = true
	a.activity = &Activity{Active: true}
	a.OnDisconnected(client)
	if a.grant != nil || a.activity != nil || a.connection != "" {
		t.Fatal("disconnect retained transport authority")
	}
	a.OnConnected(client)
	waitForPublished(t, client, 2)
	if a.connection == "" || a.connection == firstNonce || a.grant != nil {
		t.Fatal("reconnect did not establish a fresh empty connection")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.subscribed) != 2 || len(client.published) != 2 {
		t.Fatalf("reconnect subscriptions/hello count = %d/%d", len(client.subscribed), len(client.published))
	}
}

func TestBlockedSettingClearsGrantAndSendsLocalStop(t *testing.T) {
	r := newTestRedis(t)
	if err := r.client.HSet(context.Background(), "settings", "ride-group.blocked", "true").Err(); err != nil {
		t.Fatal(err)
	}
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, bootNonce: testBoot, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream}
	a.grantAt = time.Now()
	a.grantTTL = time.Second
	a.grantActive = true
	if err := a.refreshBlocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForPublished(t, client, 1)
	if !a.blocked || a.grant != nil || a.grantActive {
		t.Fatal("block did not clear local authority")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != 1 {
		t.Fatalf("local stop publishes = %d", len(client.published))
	}
	var stop struct {
		Type      string  `json:"type"`
		Active    bool    `json:"active"`
		LocalStop bool    `json:"local_stop"`
		SessionID *string `json:"session_id"`
		MemberID  *string `json:"member_id"`
	}
	if err := json.Unmarshal(client.published[0].payload, &stop); err != nil {
		t.Fatal(err)
	}
	if stop.Type != "ride_group_activity" || stop.Active || !stop.LocalStop || stop.SessionID != nil || stop.MemberID != nil {
		t.Fatalf("stop = %+v", stop)
	}
}

func TestDiscoveryGrantRequiresFreshMatchingHeartbeatAndChallengeIsOneTime(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: true, Challenge: testChallenge, SessionID: &id, MemberID: &member}, activityAt: now, challenges: map[string]time.Time{testChallenge: now}, peerStreams: map[string]peerReplayState{}, cadence: defaultCadence, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	control := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 2, TransportEpoch: 3, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(2000), Peers: []PeerDescriptor{}}
	encoded, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(1500 * time.Millisecond)
	a.handleControl(encoded)
	if !a.grantCurrentLocked(now) || a.grantTTL > 8500*time.Millisecond {
		t.Fatalf("grant TTL was not conservatively aged: current=%v ttl=%s", a.grantCurrentLocked(now), a.grantTTL)
	}
	grantAt := a.grantAt
	a.handleControl(encoded)
	if !a.grantAt.Equal(grantAt) {
		t.Fatal("replayed challenge renewed the grant")
	}
	now = now.Add(-time.Second)
	if a.grantCurrentLocked(now) {
		t.Fatal("regressive monotonic clock kept grant active")
	}
}

func TestLocalStopBarrierRemainsClosedUntilApprovedConfirmation(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, lastHelloAt: now, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, challenges: map[string]time.Time{testChallenge: now}, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 2, TransportEpoch: 3}
	a.grantAt, a.grantTTL, a.grantActive = now, 10*time.Second, true
	stop := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: false, LocalStop: true, Challenge: "00000000-0000-4000-8000-000000000009"}
	payload, _ := json.Marshal(stop)
	a.handleActivity(payload)
	waitForPublished(t, client, 1)
	if !a.localStopPending || a.grant != nil {
		t.Fatal("local stop did not immediately withdraw local authority")
	}
	client.mu.Lock()
	sentBefore := len(client.published)
	client.mu.Unlock()
	reenable := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 2, Active: true, SessionID: &id, MemberID: &member, Challenge: "00000000-0000-4000-8000-00000000000a"}
	reenablePayload, _ := json.Marshal(reenable)
	now = now.Add(time.Second)
	a.handleActivity(reenablePayload)
	client.mu.Lock()
	sentAfter := len(client.published)
	client.mu.Unlock()
	if sentAfter != sentBefore {
		t.Fatal("reenable activity was sent before server revocation")
	}
	clear := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "clear", SessionID: &id, MemberID: &member, Revision: 3, TransportEpoch: 3, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 0, Peers: []PeerDescriptor{}}
	clearPayload, _ := json.Marshal(clear)
	a.handleControl(clearPayload)
	if !a.localStopPending {
		t.Fatal("unmatched server clear released the local withdrawal barrier")
	}
	a.handleActivity(reenablePayload)
	time.Sleep(20 * time.Millisecond)
	staleGrant := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 2, TransportEpoch: 3, StreamID: &stream, Challenge: reenable.Challenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(2000), Peers: []PeerDescriptor{}}
	stalePayload, _ := json.Marshal(staleGrant)
	a.handleControl(stalePayload)
	if a.grant != nil {
		t.Fatal("pre-revocation grant reactivated the withdrawn session")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != sentAfter {
		t.Fatal("sharing resumed without the approved stop confirmation")
	}
}

func TestInactiveActivityPublishesWhileMapIsInactive(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: false, LocalStop: false, Challenge: testChallenge}
	payload, _ := json.Marshal(activity)
	a.handleActivity(payload)
	waitForPublished(t, client, 1)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != 1 {
		t.Fatalf("inactive activity publications = %d", len(client.published))
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(client.published[0].payload, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["active"]) != "false" || string(wire["local_stop"]) != "false" || string(wire["session_id"]) != "null" || string(wire["member_id"]) != "null" || string(wire["connection_nonce"]) != `"`+testConnection+`"` {
		t.Fatalf("wire activity mismatch: %s", client.published[0].payload)
	}
}

func TestPeerStatusHighWaterAndClosedStreamTombstones(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	peerID := "00000000-0000-4000-8000-000000000007"
	peerStream := "00000000-0000-4000-8000-000000000009"
	now := time.Now()
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, grant: &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1, Peers: []PeerDescriptor{{PeerID: peerID}}}, grantAt: now, grantTTL: 10 * time.Second, grantActive: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	status := PeerStatus{Version: 1, Type: "ride_group_peer_status", SessionID: id, Revision: 1, TransportEpoch: 1, StreamID: stream, PeerID: peerID, PeerStreamID: peerStream, PeerTransportEpoch: 2, Status: "no_fix"}
	statusPayload, _ := json.Marshal(status)
	a.handlePeerStatus(statusPayload)
	position := PeerPosition{Version: 1, Type: "ride_group_peer", SessionID: id, Revision: 1, TransportEpoch: 1, StreamID: stream, PeerStreamID: peerStream, PeerTransportEpoch: 1, Sequence: 1, PeerID: peerID, Label: "Peer", PaletteIndex: 1, CapturedAtMS: 1781731200000, TimestampSource: "producer_fix", MotionSource: "unknown", ReceivedAtMS: 1781731200100, ServerTimeMS: 1781731200200, ValidForMS: 9000}
	oldPayload, _ := json.Marshal(position)
	a.handlePeer(oldPayload)
	if len(a.peerStreams) != 1 || a.peerStreams[peerID].epoch != 2 {
		t.Fatal("peer-status-first epoch was not retained")
	}
	position.PeerTransportEpoch = 2
	position.Sequence = 2
	position.CapturedAtMS++
	freshPayload, _ := json.Marshal(position)
	a.handlePeer(freshPayload)
	if a.peerStreams[peerID].sequence != 2 {
		t.Fatal("newer source epoch point was rejected")
	}
	status.PeerTransportEpoch = 2
	status.Status = "inactive"
	inactivePayload, _ := json.Marshal(status)
	a.handlePeerStatus(inactivePayload)
	position.Sequence++
	position.CapturedAtMS++
	closedPayload, _ := json.Marshal(position)
	a.handlePeer(closedPayload)
	if !a.peerStreams[peerID].closed {
		t.Fatal("inactive status did not tombstone the source stream")
	}
	position.PeerTransportEpoch = 3
	position.PeerStreamID = "00000000-0000-4000-8000-00000000000a"
	position.Sequence = 1
	position.CapturedAtMS++
	nextStreamPayload, _ := json.Marshal(position)
	a.handlePeer(nextStreamPayload)
	if a.peerStreams[peerID].epoch != 3 || a.peerStreams[peerID].closed {
		t.Fatal("newer source stream did not replace closed stream")
	}
}

func TestSteadyHeartbeatAndPositionCadencePreservesSameStream(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	start := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	now := start
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, bootNonce: testBoot, settingsReady: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}, cadence: 2 * time.Second, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	sequence := int64(0)
	challenge := testChallenge
	for second := 0; second <= 42; second++ {
		now = start.Add(time.Duration(second) * time.Second)
		if second%3 == 0 {
			sequence++
			challenge, _ = newUUID()
			activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: sequence, Active: true, SessionID: &id, MemberID: &member, Challenge: challenge}
			payload, _ := json.Marshal(activity)
			a.handleActivity(payload)
			control := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "renew", SessionID: &id, MemberID: &member, Revision: 1, TransportEpoch: 1, StreamID: &stream, Challenge: challenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(2000), Peers: []PeerDescriptor{}}
			controlPayload, _ := json.Marshal(control)
			a.handleControl(controlPayload)
		}
		if second%2 == 0 {
			fix := testGPSFix("52.000000", "13.000000", now, 3)
			payload, _ := json.Marshal(fix)
			a.handleGPS(payload)
			if a.positionSeq < int64(second/2+1) {
				t.Fatalf("position %d was not queued: seq=%d grant=%v active=%v activityAge=%s grantAge=%s gps=%s cadence=%s", second/2+1, a.positionSeq, a.grant != nil, a.grantActive, now.Sub(a.activityAt), now.Sub(a.grantAt), a.gpsObservedAt, a.cadence)
			}
			waitForPositionCount(t, client, second/2+1)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	var highest int64
	positions := 0
	for _, message := range client.published {
		var envelope struct {
			Type     string `json:"type"`
			Sequence int64  `json:"seq"`
		}
		if json.Unmarshal(message.payload, &envelope) == nil && envelope.Type == "ride_group_position" {
			positions++
			if envelope.Sequence > highest {
				highest = envelope.Sequence
			}
		}
	}
	if positions < 20 || highest < 20 || a.grant == nil {
		t.Fatalf("steady session stalled: positions=%d highest_seq=%d grant=%v", positions, highest, a.grant != nil)
	}
}

func TestSameStreamSequenceContinuesAfterLocalLeaseExpiry(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, bootNonce: testBoot, settingsReady: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}, cadence: time.Second, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: true, SessionID: &id, MemberID: &member, Challenge: testChallenge}
	activityPayload, _ := json.Marshal(activity)
	a.handleActivity(activityPayload)
	grant := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 1, TransportEpoch: 1, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(1000), Peers: []PeerDescriptor{}}
	grantPayload, _ := json.Marshal(grant)
	a.handleControl(grantPayload)
	for i := 0; i < 2; i++ {
		now = now.Add(time.Second)
		payload, _ := json.Marshal(testGPSFix("52.000000", "13.000000", now, 3))
		a.handleGPS(payload)
	}
	if a.positionSeq != 2 {
		t.Fatalf("initial sequence = %d", a.positionSeq)
	}
	now = now.Add(11 * time.Second)
	a.expireLease()
	if a.grant != nil {
		t.Fatal("expired local grant remained active")
	}
	activity.Sequence = 2
	activity.Challenge = "00000000-0000-4000-8000-00000000000a"
	activityPayload, _ = json.Marshal(activity)
	a.handleActivity(activityPayload)
	grant.Challenge = activity.Challenge
	grant.Action = "grant"
	grantPayload, _ = json.Marshal(grant)
	a.handleControl(grantPayload)
	now = now.Add(time.Second)
	payload, _ := json.Marshal(testGPSFix("52.000000", "13.000000", now, 3))
	a.handleGPS(payload)
	if a.positionSeq != 3 {
		t.Fatalf("same-stream sequence reset after lease expiry: %d", a.positionSeq)
	}
}

func TestPositionCadenceIsServerClampedAndNewFixesOnly(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, grant: &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1}, grantAt: now, grantTTL: 10 * time.Second, grantActive: true, cadence: 2 * time.Second, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	for index, offset := range []time.Duration{0, time.Second, 2 * time.Second} {
		now = time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC).Add(offset)
		fix := testGPSFix("52.000000", "13.000000", now, 4.2)
		payload, _ := json.Marshal(fix)
		a.handleGPS(payload)
		if index == 0 || index == 2 {
			waitForPositionCount(t, client, index/2+1)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != 2 {
		t.Fatalf("position count at two-second cadence = %d, want 2", len(client.published))
	}
	for _, message := range client.published {
		if len(message.payload) > MaxEnvelopeBytes || message.qos != 0 || message.retained {
			t.Fatalf("position size/qos/retention invalid: size=%d qos=%d retained=%v", len(message.payload), message.qos, message.retained)
		}
	}
}

func TestGPSObservationHighWaterSurvivesNoFixEstimatorReset(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, grant: &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1}, grantAt: now, grantTTL: 10 * time.Second, grantActive: true, cadence: time.Second, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	cleanupAdapter(t, a)
	first := testGPSFix("52.000000", "13.000000", now, 3)
	firstPayload, _ := json.Marshal(first)
	a.handleGPS(firstPayload)
	now = now.Add(time.Second)
	noFix := `{"latitude":"52.000000","longitude":"13.000000","timestamp":"` + now.Format(time.RFC3339) + `","active":true,"connected":true,"state":"searching","fix":"none","eph":null}`
	a.handleGPS([]byte(noFix))
	replayed := testGPSFix("52.000100", "13.000000", now, 3)
	replayedPayload, _ := json.Marshal(replayed)
	a.handleGPS(replayedPayload)
	if a.positionSeq != 1 {
		t.Fatalf("replayed source timestamp advanced position sequence after no-fix reset: %d", a.positionSeq)
	}
	time.Sleep(25 * time.Millisecond)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) > 1 {
		t.Fatalf("replayed source timestamp was published after no-fix reset: %d", len(client.published))
	}
}

func TestExpiredChallengeCannotGrant(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Now()
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, challenges: map[string]time.Time{testChallenge: now.Add(-11 * time.Second)}, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	control := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 1, TransportEpoch: 1, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(2000), Peers: []PeerDescriptor{}}
	payload, _ := json.Marshal(control)
	a.handleControl(payload)
	if a.grant != nil {
		t.Fatal("expired challenge granted a lease")
	}
}

func TestClearRemainsEffectiveWithoutActiveHeartbeatAndIgnoresOtherSession(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: false}, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 2, TransportEpoch: 3}
	clear := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "clear", SessionID: &id, MemberID: &member, Revision: 2, TransportEpoch: 3, StreamID: &stream, Challenge: testChallenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 0, Peers: []PeerDescriptor{}}
	payload, _ := json.Marshal(clear)
	a.handleControl(payload)
	if a.grant != nil {
		t.Fatal("clear did not clear current grant while inactive")
	}
	otherID := "00000000-0000-4000-8000-000000000008"
	a.grant = &Control{SessionID: &otherID, MemberID: &member, StreamID: &stream, Revision: 3, TransportEpoch: 4}
	oldPayload, _ := json.Marshal(clear)
	a.handleControl(oldPayload)
	if a.grant == nil || *a.grant.SessionID != otherID {
		t.Fatal("stale clear revoked an unrelated new session")
	}
}

func TestGPSPublishesOnlyFreshSourceFixAndIgnoresRetainedMotion(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: time.Now(), grant: &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1}, grantAt: time.Now(), grantTTL: 10 * time.Second, grantActive: true, cadence: time.Second, peerStreams: map[string]peerReplayState{}}
	cleanupAdapter(t, a)
	timestamp := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	fix := `{"latitude":"0.000000","longitude":"0.000000","timestamp":"` + timestamp + `","active":true,"connected":true,"state":"fix-established","fix":"3d","eph":4.2,"speed":"31.0","course":"88.0","updated":"` + timestamp + `"}`
	a.handleGPS([]byte(fix))
	waitForPositionCount(t, client, 1)
	a.handleGPS([]byte(fix))
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != 1 {
		t.Fatalf("GPS publications = %d, want one new fix", len(client.published))
	}
	got := client.published[0]
	if got.topic != "scooters/vehicle-1/ride_group/up" || got.qos != 0 || got.retained {
		t.Fatalf("position publish = %+v", got)
	}
	var position map[string]any
	if err := json.Unmarshal(got.payload, &position); err != nil {
		t.Fatal(err)
	}
	if position["type"] != "ride_group_position" || position["timestamp_source"] != "producer_fix" || position["lat"] != float64(0) || position["lon"] != float64(0) || position["speed_mps"] != nil || position["heading_deg"] != nil || position["motion_source"] != "unknown" {
		t.Fatalf("position payload incorrect: %v", position)
	}
	if strings.Contains(string(got.payload), "updated") || strings.Contains(string(got.payload), "31.0") {
		t.Fatal("producer metadata or retained speed leaked to wire")
	}
}

func cleanupAdapter(t *testing.T, adapter *Adapter) {
	t.Helper()
	if adapter.cancel == nil {
		adapter.ctx, adapter.cancel = context.WithCancel(context.Background())
	}
	t.Cleanup(adapter.Stop)
}

func waitForPublished(t *testing.T, client *mqttFake, count int) {
	t.Helper()
	waitFor(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return len(client.published) >= count
	})
}

func waitForPositionCount(t *testing.T, client *mqttFake, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		positions := 0
		for _, publication := range client.published {
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(publication.payload, &envelope) == nil && envelope.Type == "ride_group_position" {
				positions++
			}
		}
		client.mu.Unlock()
		if positions >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d position publications", count)
}

func durationPointer(milliseconds int64) *int64 { return &milliseconds }
