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
	client := &mqttFake{connected: true}
	a.OnConnected(client)
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

func TestReconnectRotatesNonceAndDropsPriorGrant(t *testing.T) {
	r := newTestRedis(t)
	a := &Adapter{config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client}
	client := &mqttFake{connected: true}
	a.OnConnected(client)
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
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream}
	a.grantAt = time.Now()
	a.grantTTL = time.Second
	a.grantActive = true
	if err := a.refreshBlocked(context.Background()); err != nil {
		t.Fatal(err)
	}
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

func TestLocalStopPreventsReenableUntilServerClearsMembership(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, challenges: map[string]time.Time{testChallenge: now}, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	a.grant = &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 2, TransportEpoch: 3}
	a.grantAt, a.grantTTL, a.grantActive = now, 10*time.Second, true
	stop := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: false, LocalStop: true, Challenge: "00000000-0000-4000-8000-000000000009"}
	payload, _ := json.Marshal(stop)
	a.handleActivity(payload)
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
	if a.localStopPending {
		t.Fatal("server clear did not complete pending local withdrawal")
	}
	a.handleActivity(reenablePayload)
	staleGrant := Control{Version: 1, Type: "ride_group_control", ConnectionNonce: testConnection, Action: "grant", SessionID: &id, MemberID: &member, Revision: 2, TransportEpoch: 3, StreamID: &stream, Challenge: reenable.Challenge, ServerTimeMS: 1781731200000, LeaseTTLMS: 10000, CadenceMS: durationPointer(2000), Peers: []PeerDescriptor{}}
	stalePayload, _ := json.Marshal(staleGrant)
	a.handleControl(stalePayload)
	if a.grant != nil {
		t.Fatal("pre-revocation grant reactivated the withdrawn session")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.published) != sentAfter+1 {
		t.Fatal("fresh activity did not resume after server clear")
	}
}

func TestInactiveActivityPublishesWhileMapIsInactive(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, challenges: map[string]time.Time{}, peerStreams: map[string]peerReplayState{}}
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: testBoot, Sequence: 1, Active: false, LocalStop: false, Challenge: testChallenge}
	payload, _ := json.Marshal(activity)
	a.handleActivity(payload)
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

func TestPositionCadenceIsServerClampedAndNewFixesOnly(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, grant: &Control{SessionID: &id, MemberID: &member, StreamID: &stream, Revision: 1, TransportEpoch: 1}, grantAt: now, grantTTL: 10 * time.Second, grantActive: true, cadence: 2 * time.Second, peerStreams: map[string]peerReplayState{}, clock: func() time.Time { return now }}
	for _, offset := range []time.Duration{0, time.Second, 2 * time.Second} {
		now = time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC).Add(offset)
		fix := testGPSFix("52.000000", "13.000000", now, 4.2)
		payload, _ := json.Marshal(fix)
		a.handleGPS(payload)
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

func TestExpiredChallengeCannotGrant(t *testing.T) {
	r := newTestRedis(t)
	client := &mqttFake{connected: true}
	id, member, stream := testSession, testMember, testStream
	now := time.Now()
	a := &Adapter{ctx: context.Background(), config: Config{Enabled: true, Identifier: "vehicle-1"}, redis: r.client, mqtt: client, connection: testConnection, settingsReady: true, activity: &Activity{Active: true, SessionID: &id, MemberID: &member}, activityAt: now, challenges: map[string]time.Time{testChallenge: now.Add(-11 * time.Second)}, peerStreams: map[string]peerReplayState{}}
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
	timestamp := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	fix := `{"latitude":"0.000000","longitude":"0.000000","timestamp":"` + timestamp + `","active":true,"connected":true,"state":"fix-established","fix":"3d","eph":4.2,"speed":"31.0","course":"88.0","updated":"` + timestamp + `"}`
	a.handleGPS([]byte(fix))
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

func durationPointer(milliseconds int64) *int64 { return &milliseconds }
