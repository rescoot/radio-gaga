package ridegroup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"
)

const (
	activityChannel = "ride-group:activity"
	controlChannel  = "ride-group:control"
	peersChannel    = "ride-group:peers"
	gpsChannel      = "gps:tpv"
	settingsChannel = "settings"
	defaultCadence  = 2 * time.Second
	leaseDuration   = 10 * time.Second
)

type Config struct {
	Enabled    bool
	Identifier string
}

type peerReplayState struct {
	streamID string
	epoch    int64
	sequence int64
	captured int64
	status   string
	closed   bool
}

type peerReplayScope struct {
	sessionID string
}

type Adapter struct {
	config          Config
	redis           *redis.Client
	clock           func() time.Time
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	egressMu        sync.Mutex
	mqttWorkerOnce  sync.Once
	redisWorkerOnce sync.Once
	ackWorkerOnce   sync.Once
	mqttWake        chan struct{}
	mqttWorkerDone  chan struct{}
	mqttPending     map[string]mqttPublication
	mqttOrder       uint64
	mqttClosing     bool
	redisQueue      chan redisPublication
	ackQueue        chan mqtt.Message

	mu                 sync.Mutex
	mqtt               mqtt.Client
	connecting         bool
	connectAttempt     uint64
	connection         string
	lastHelloAt        time.Time
	bootNonce          string
	stopSequence       int64
	activity           *Activity
	activityAt         time.Time
	activityBootNonce  string
	activitySequence   int64
	challenges         map[string]time.Time
	grant              *Control
	grantAt            time.Time
	grantTTL           time.Duration
	grantActive        bool
	lastCleared        *Control
	cadence            time.Duration
	positionSeq        int64
	positionStreamID   string
	positionSessionID  string
	positionEpoch      int64
	positionGeneration uint64
	lastPositionAt     time.Time
	gpsObservedAt      time.Time
	blocked            bool
	localStopPending   bool
	settingsReady      bool
	peerStreams        map[string]peerReplayState
	peerScope          peerReplayScope
	estimator          MotionEstimator
}

func NewAdapter(parent context.Context, cfg Config, client *redis.Client) (*Adapter, error) {
	ctx, cancel := context.WithCancel(parent)
	bootNonce, err := newUUID()
	if err != nil {
		cancel()
		return nil, err
	}
	return &Adapter{config: cfg, redis: client, ctx: ctx, cancel: cancel, bootNonce: bootNonce, challenges: make(map[string]time.Time), peerStreams: make(map[string]peerReplayState), cadence: defaultCadence}, nil
}

func (a *Adapter) Start() {
	if !a.config.Enabled || a.redis == nil {
		return
	}
	a.startWorkers()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.run()
	}()
}

func (a *Adapter) Stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	client := a.mqtt
	topic := ""
	if a.config.Enabled && client != nil && client.IsConnected() {
		topic = fmt.Sprintf("scooters/%s/ride_group/down", a.config.Identifier)
	}
	a.connectAttempt++
	a.connecting = false
	a.clearGrantLocked()
	a.mqtt = nil
	a.mu.Unlock()
	a.cancel()
	a.stopMQTTWorker(client, topic)
	a.wg.Wait()
}

func (a *Adapter) OnConnected(client mqtt.Client) {
	if a == nil || !a.config.Enabled || client == nil || !client.IsConnected() || (a.ctx != nil && a.ctx.Err() != nil) {
		return
	}
	a.mu.Lock()
	if (a.mqtt == client && a.connection != "") || a.connecting {
		a.mu.Unlock()
		return
	}
	a.connecting = true
	a.connectAttempt++
	attempt := a.connectAttempt
	a.mu.Unlock()
	connection, err := newUUID()
	if err != nil {
		a.mu.Lock()
		if a.connectAttempt == attempt {
			a.connecting = false
		}
		a.mu.Unlock()
		return
	}
	topic := fmt.Sprintf("scooters/%s/ride_group/down", a.config.Identifier)
	token := client.Subscribe(topic, 1, a.handleDownlink)
	if !token.WaitTimeout(3*time.Second) || token.Error() != nil || !client.IsConnected() || (a.ctx != nil && a.ctx.Err() != nil) {
		a.mu.Lock()
		if a.connectAttempt == attempt {
			a.connecting = false
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connectAttempt != attempt {
		return
	}
	a.connecting = false
	a.resetConnectionLocked(client, connection)
	a.lastCleared = nil
	a.publishHelloLocked()
	if a.blocked || a.localStopPending {
		a.publishLocalStopLocked()
	}
}

func (a *Adapter) OnDisconnected(client mqtt.Client) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.mqtt == client || a.connecting {
		a.connectAttempt++
		a.connecting = false
		if a.mqtt == client {
			a.resetConnectionLocked(nil, "")
		}
	}
	a.mu.Unlock()
}

func (a *Adapter) resetConnectionLocked(client mqtt.Client, connection string) {
	a.mqtt = client
	a.connection = connection
	a.lastHelloAt = time.Time{}
	a.positionSeq = 0
	a.positionStreamID = ""
	a.positionSessionID = ""
	a.positionEpoch = 0
	a.activity = nil
	a.activityAt = time.Time{}
	a.challenges = make(map[string]time.Time)
	a.clearGrantLocked()
}

func (a *Adapter) invalidatePositionPublicationsLocked() {
	a.positionGeneration++
	a.dropPendingPosition()
}

func (a *Adapter) clearGrantLocked() {
	a.invalidatePositionPublicationsLocked()
	a.grant = nil
	a.grantAt = time.Time{}
	a.grantTTL = 0
	a.grantActive = false
	a.lastPositionAt = time.Time{}
	a.estimator.Reset()
}

func (a *Adapter) run() {
	channels := []string{activityChannel, gpsChannel, settingsChannel}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for a.ctx.Err() == nil {
		pubsub := a.redis.Subscribe(a.ctx, channels...)
		events := pubsub.ChannelWithSubscriptions(a.ctx, 32)
		subscribed := make(map[string]bool, len(channels))
		ready := false
		restart := false
		for !restart {
			select {
			case <-a.ctx.Done():
				_ = pubsub.Close()
				return
			case event, ok := <-events:
				if !ok {
					a.failClosed()
					restart = true
					continue
				}
				switch value := event.(type) {
				case *redis.Subscription:
					if value.Kind != "subscribe" || !containsChannel(channels, value.Channel) {
						continue
					}
					if ready {
						a.failClosed()
						restart = true
						continue
					}
					subscribed[value.Channel] = true
					if len(subscribed) == len(channels) {
						if err := a.completeRedisRecovery(events); err != nil {
							if a.ctx.Err() == nil {
								log.Printf("Ride-group Redis recovery unavailable; sharing remains fail-closed")
							}
							a.failClosed()
							restart = true
							continue
						}
						ready = true
					}
				case *redis.Message:
					if !ready {
						continue
					}
					if a.handleRedisMessage(value) {
						if err := a.refreshBlockedWithTimeout(); err != nil {
							a.failClosed()
							restart = true
						}
					}
				}
			case <-ticker.C:
				a.expireLease()
			}
		}
		_ = pubsub.Close()
		if a.ctx.Err() == nil {
			select {
			case <-a.ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func containsChannel(channels []string, name string) bool {
	for _, channel := range channels {
		if channel == name {
			return true
		}
	}
	return false
}

func (a *Adapter) completeRedisRecovery(events <-chan interface{}) error {
	a.mu.Lock()
	a.settingsReady = false
	a.mu.Unlock()
	for {
		if _, err := a.drainRedisEvents(events); err != nil {
			return err
		}
		if err := a.refreshBlockedWithTimeout(); err != nil {
			return err
		}
		settingsChanged, err := a.drainRedisEvents(events)
		if err != nil {
			return err
		}
		if !settingsChanged {
			return nil
		}
	}
}

func (a *Adapter) drainRedisEvents(events <-chan interface{}) (bool, error) {
	settingsChanged := false
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return false, fmt.Errorf("Redis subscription ended during recovery")
			}
			if _, ok := event.(*redis.Subscription); ok {
				return false, fmt.Errorf("Redis subscription changed during recovery")
			}
			if message, ok := event.(*redis.Message); ok && message.Channel == settingsChannel && strings.Contains(message.Payload, "ride-group.blocked") {
				settingsChanged = true
			}
		default:
			return settingsChanged, nil
		}
	}
}

func (a *Adapter) refreshBlockedWithTimeout() error {
	ctx, cancel := context.WithTimeout(a.ctx, time.Second)
	defer cancel()
	return a.refreshBlocked(ctx)
}

func (a *Adapter) handleRedisMessage(message *redis.Message) bool {
	switch message.Channel {
	case activityChannel:
		a.handleActivity([]byte(message.Payload))
	case gpsChannel:
		a.handleGPS([]byte(message.Payload))
	case settingsChannel:
		return strings.Contains(message.Payload, "ride-group.blocked")
	}
	return false
}

func (a *Adapter) refreshBlocked(ctx context.Context) error {
	value, err := a.redis.HGet(ctx, "settings", "ride-group.blocked").Result()
	if err != nil && err != redis.Nil {
		return err
	}
	blocked := value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
	a.mu.Lock()
	defer a.mu.Unlock()
	wasBlocked := a.blocked
	a.blocked = blocked
	a.settingsReady = true
	if blocked {
		a.localStopPending = true
		a.activity = nil
		a.activityAt = time.Time{}
		a.challenges = make(map[string]time.Time)
		a.clearGrantLocked()
		if a.mqtt != nil && a.mqtt.IsConnected() {
			a.publishLocalStopLocked()
		}
	} else if wasBlocked {
		// Opting back in never restores a prior activity or grant.
		a.activity = nil
		a.activityAt = time.Time{}
	}
	return nil
}

func (a *Adapter) handleActivity(payload []byte) {
	activity, err := ParseActivity(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.settingsReady {
		return
	}
	if activity.BootNonce == a.activityBootNonce && activity.Sequence <= a.activitySequence {
		return
	}
	if a.blocked || a.localStopPending {
		if activity.LocalStop && a.blocked {
			a.activityBootNonce = activity.BootNonce
			a.activitySequence = activity.Sequence
			a.publishLocalStopLocked()
		}
		return
	}
	if _, exists := a.challenges[activity.Challenge]; exists {
		return
	}
	if !activity.Active || activity.LocalStop {
		a.activity = &activity
		a.activityAt = a.currentTime()
		a.activityBootNonce = activity.BootNonce
		a.activitySequence = activity.Sequence
		a.challenges = make(map[string]time.Time)
		a.clearGrantLocked()
		if activity.LocalStop {
			a.localStopPending = true
		}
		if a.mqtt != nil && a.mqtt.IsConnected() {
			if activity.LocalStop {
				a.publishActivityQoSLocked(activity, 1)
			} else {
				a.publishActivityLocked(activity)
			}
		}
		return
	}
	if a.mqtt == nil || !a.mqtt.IsConnected() {
		return
	}
	if !a.activityAt.IsZero() && a.elapsedSince(a.activityAt) < time.Second {
		return
	}
	if (activity.SessionID == nil) != (activity.MemberID == nil) {
		return
	}
	if a.grant != nil && a.grant.SessionID != nil && activity.SessionID != nil && (*activity.SessionID != *a.grant.SessionID || *activity.MemberID != *a.grant.MemberID) {
		return
	}
	if a.grant != nil && a.grant.SessionID != nil && activity.SessionID == nil {
		return
	}
	a.activity = &activity
	a.activityBootNonce = activity.BootNonce
	a.activitySequence = activity.Sequence
	if a.grant != nil && a.grant.SessionID != nil && activity.SessionID != nil && *activity.SessionID == *a.grant.SessionID && *activity.MemberID == *a.grant.MemberID {
		a.grantActive = true
	}
	a.activityAt = a.currentTime()
	if (a.grant == nil || !a.grantCurrentLocked(a.activityAt)) && (a.lastHelloAt.IsZero() || a.activityAt.Sub(a.lastHelloAt) >= 30*time.Second) {
		a.publishHelloLocked()
	}
	a.challenges[activity.Challenge] = a.activityAt
	a.pruneChallengesLocked(a.activityAt)
	a.publishActivityLocked(activity)
}

func (a *Adapter) handleDownlink(_ mqtt.Client, message mqtt.Message) {
	if message == nil {
		return
	}
	defer a.queueAck(message)
	if message.Retained() {
		return
	}
	payload := append([]byte(nil), message.Payload()...)
	if len(payload) > MaxEnvelopeBytes {
		return
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "ride_group_control":
		a.handleControl(payload)
	case "ride_group_peer":
		a.handlePeer(payload)
	case "ride_group_peer_status":
		a.handlePeerStatus(payload)
	}
}

func (a *Adapter) handleControl(payload []byte) {
	control, err := ParseControl(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mqtt == nil || !a.mqtt.IsConnected() || control.ConnectionNonce != a.connection {
		return
	}
	if control.Action == "clear" {
		if a.grant != nil {
			if a.grant.SessionID == nil || a.grant.MemberID == nil || a.grant.StreamID == nil || control.SessionID == nil || control.MemberID == nil || control.StreamID == nil || *control.SessionID != *a.grant.SessionID || *control.MemberID != *a.grant.MemberID || control.TransportEpoch < a.grant.TransportEpoch || control.Revision < a.grant.Revision || (control.TransportEpoch == a.grant.TransportEpoch && control.Revision == a.grant.Revision && *control.StreamID != *a.grant.StreamID) {
				return
			}
			copy := control
			a.lastCleared = &copy
			a.clearGrantLocked()
		} else if a.localStopPending {
			return
		} else if a.lastCleared == nil || control.SessionID == nil || control.MemberID == nil || control.StreamID == nil || *control.SessionID != *a.lastCleared.SessionID || *control.MemberID != *a.lastCleared.MemberID || *control.StreamID != *a.lastCleared.StreamID || control.Revision != a.lastCleared.Revision || control.TransportEpoch != a.lastCleared.TransportEpoch {
			return
		}
		a.challenges = make(map[string]time.Time)
		a.queueRedisPublication(controlChannel, payload, 0)
		return
	}
	if a.blocked || a.localStopPending {
		return
	}
	sentAt, ok := a.challenges[control.Challenge]
	if !ok || a.elapsedSince(sentAt) < 0 || a.elapsedSince(sentAt) >= leaseDuration || a.activity == nil || !a.activity.Active || a.activity.LocalStop || a.elapsedSince(a.activityAt) < 0 || a.elapsedSince(a.activityAt) >= leaseDuration {
		return
	}
	switch control.Action {
	case "grant", "renew":
		if a.lastCleared != nil && control.SessionID != nil && control.MemberID != nil && *control.SessionID == *a.lastCleared.SessionID && *control.MemberID == *a.lastCleared.MemberID && (control.Revision < a.lastCleared.Revision || control.TransportEpoch < a.lastCleared.TransportEpoch || (control.Revision == a.lastCleared.Revision && control.TransportEpoch == a.lastCleared.TransportEpoch)) {
			return
		}
		if a.grant != nil {
			if a.grant.SessionID == nil || a.grant.MemberID == nil || a.grant.StreamID == nil || *control.SessionID != *a.grant.SessionID || *control.MemberID != *a.grant.MemberID || control.TransportEpoch < a.grant.TransportEpoch || control.Revision < a.grant.Revision {
				return
			}
			if control.TransportEpoch == a.grant.TransportEpoch && *control.StreamID != *a.grant.StreamID {
				return
			}
		}
		elapsed := a.elapsedSince(sentAt)
		if a.activity.SessionID != nil && (*a.activity.SessionID != *control.SessionID || *a.activity.MemberID != *control.MemberID) {
			return
		}
		remaining := time.Duration(control.LeaseTTLMS)*time.Millisecond - elapsed
		if remaining <= 0 {
			return
		}
		if a.positionStreamID != *control.StreamID || a.positionSessionID != *control.SessionID || a.positionEpoch != control.TransportEpoch {
			a.positionSeq = 0
			a.positionStreamID = *control.StreamID
			a.positionSessionID = *control.SessionID
			a.positionEpoch = control.TransportEpoch
		}
		if a.grant == nil || *a.grant.SessionID != *control.SessionID || *a.grant.MemberID != *control.MemberID || *a.grant.StreamID != *control.StreamID || a.grant.Revision != control.Revision || a.grant.TransportEpoch != control.TransportEpoch {
			a.positionGeneration++
			a.dropPendingPosition()
		}
		a.setPeerReplayScopeLocked(control)
		a.grant = &control
		a.lastCleared = nil
		a.grantAt = a.currentTime()
		a.grantTTL = remaining
		a.grantActive = a.activity.SessionID != nil && a.activity.MemberID != nil && *a.activity.SessionID == *control.SessionID && *a.activity.MemberID == *control.MemberID
		a.cadence = time.Duration(*control.CadenceMS) * time.Millisecond
	case "status":
		// A no-session status is informational and cannot clear an existing grant.
	}
	delete(a.challenges, control.Challenge)
	a.queueRedisPublication(controlChannel, payload, leaseDuration-a.elapsedSince(sentAt))
}

func (a *Adapter) handlePeer(payload []byte) {
	peer, err := ParsePeerPosition(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.grantCurrentLocked(a.currentTime()) || a.grant.SessionID == nil || a.grant.MemberID == nil || a.grant.StreamID == nil || peer.SessionID != *a.grant.SessionID || peer.Revision != a.grant.Revision || peer.TransportEpoch != a.grant.TransportEpoch || peer.StreamID != *a.grant.StreamID || !a.peerInGrantLocked(peer.PeerID) {
		return
	}
	a.setPeerReplayScopeLocked(*a.grant)
	previous, exists := a.peerStreams[peer.PeerID]
	if exists && peer.PeerTransportEpoch < previous.epoch {
		return
	}
	if exists && peer.PeerTransportEpoch == previous.epoch {
		if peer.PeerStreamID != previous.streamID || previous.closed || peer.Sequence <= previous.sequence || peer.CapturedAtMS <= previous.captured {
			return
		}
	}
	a.peerStreams[peer.PeerID] = peerReplayState{streamID: peer.PeerStreamID, epoch: peer.PeerTransportEpoch, sequence: peer.Sequence, captured: peer.CapturedAtMS}
	a.queueRedisPublication(peersChannel, payload, time.Second)
}

func (a *Adapter) handlePeerStatus(payload []byte) {
	status, err := ParsePeerStatus(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.grantCurrentLocked(a.currentTime()) || a.grant.SessionID == nil || a.grant.MemberID == nil || a.grant.StreamID == nil || status.SessionID != *a.grant.SessionID || status.Revision != a.grant.Revision || status.TransportEpoch != a.grant.TransportEpoch || status.StreamID != *a.grant.StreamID || !a.peerInGrantLocked(status.PeerID) {
		return
	}
	a.setPeerReplayScopeLocked(*a.grant)
	previous, exists := a.peerStreams[status.PeerID]
	if exists && (status.PeerTransportEpoch < previous.epoch || (status.PeerTransportEpoch == previous.epoch && status.PeerStreamID != previous.streamID)) {
		return
	}
	if !exists || status.PeerTransportEpoch > previous.epoch {
		previous = peerReplayState{streamID: status.PeerStreamID, epoch: status.PeerTransportEpoch}
	}
	previous.status = status.Status
	if status.Status == "inactive" || status.Status == "cleared" {
		previous.closed = true
	}
	a.peerStreams[status.PeerID] = previous
	a.queueRedisPublication(peersChannel, payload, time.Second)
}

func (a *Adapter) handleGPS(payload []byte) {
	capturedAt, hasTimestamp := parseGPSTimestamp(payload)
	fix, err := ParseGPSFix(payload)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.currentTime()
	if hasTimestamp && capturedAt.After(now) {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		return
	}
	if !hasTimestamp {
		a.invalidatePositionPublicationsLocked()
		if err != nil {
			a.estimator.Reset()
		}
		return
	}
	if !capturedAt.After(a.gpsObservedAt) {
		if err != nil {
			a.invalidatePositionPublicationsLocked()
			a.estimator.Reset()
		}
		return
	}
	a.gpsObservedAt = capturedAt
	if err != nil {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		return
	}
	sample, valid := a.estimator.Observe(fix, now)
	if !valid {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		return
	}
	if !a.grantCurrentLocked(now) || a.activity == nil || a.activity.SessionID == nil || a.activity.MemberID == nil || a.grant.SessionID == nil || *a.activity.SessionID != *a.grant.SessionID || *a.activity.MemberID != *a.grant.MemberID || now.Sub(sample.CapturedAt) > 5*time.Second || (!a.lastPositionAt.IsZero() && now.Sub(a.lastPositionAt) < a.cadence) {
		return
	}
	position := Position{Version: 1, Type: "ride_group_position", SessionID: *a.grant.SessionID, Revision: a.grant.Revision, TransportEpoch: a.grant.TransportEpoch, StreamID: *a.grant.StreamID, Sequence: a.positionSeq + 1, CapturedAtMS: sample.CapturedAt.UnixMilli(), TimestampSource: "producer_fix", Latitude: sample.Latitude, Longitude: sample.Longitude, AccuracyM: sample.AccuracyM, HeadingDeg: sample.HeadingDeg, SpeedMPS: sample.SpeedMPS, MotionSource: sample.MotionSource}
	encoded, err := json.Marshal(position)
	if err != nil || len(encoded) > MaxEnvelopeBytes {
		return
	}
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, expiresAt: time.Now().Add(5 * time.Second), priority: 1, kind: "position", positionSessionID: *a.grant.SessionID, positionMemberID: *a.grant.MemberID, positionStreamID: *a.grant.StreamID, positionRevision: a.grant.Revision, positionEpoch: a.grant.TransportEpoch, positionGeneration: a.positionGeneration, positionCapturedAt: sample.CapturedAt})
	a.positionSeq++
	a.lastPositionAt = now
}

func (a *Adapter) expireLease() {
	now := a.currentTime()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activity != nil && (now.Sub(a.activityAt) >= leaseDuration || now.Before(a.activityAt)) {
		a.activity = nil
		a.activityAt = time.Time{}
		a.challenges = make(map[string]time.Time)
		a.clearGrantLocked()
	}
	if a.grant != nil && (now.Sub(a.grantAt) >= a.grantTTL || now.Before(a.grantAt)) {
		a.clearGrantLocked()
	}
}

func (a *Adapter) currentTime() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

func (a *Adapter) elapsedSince(at time.Time) time.Duration {
	return a.currentTime().Sub(at)
}

func (a *Adapter) grantCurrentLocked(now time.Time) bool {
	return a.settingsReady && a.mqtt != nil && a.mqtt.IsConnected() && a.grant != nil && a.grantActive && !a.grantAt.IsZero() && now.Sub(a.grantAt) >= 0 && now.Sub(a.grantAt) < a.grantTTL && a.activity != nil && a.activity.Active && !a.activity.LocalStop && now.Sub(a.activityAt) >= 0 && now.Sub(a.activityAt) < leaseDuration && !a.blocked
}

func (a *Adapter) publishHelloLocked() {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	body := struct {
		Version         int      `json:"v"`
		Type            string   `json:"type"`
		ConnectionNonce string   `json:"connection_nonce"`
		Capabilities    []string `json:"capabilities"`
	}{1, "ride_group_hello", a.connection, []string{"ride_group_v1"}}
	encoded, _ := json.Marshal(body)
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, priority: 3, kind: "hello"})
	a.lastHelloAt = a.currentTime()
}

func (a *Adapter) publishActivityLocked(activity Activity) {
	a.publishActivityQoSLocked(activity, 0)
}

func (a *Adapter) publishActivityQoSLocked(activity Activity, qos byte) {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	body := struct {
		Version         int     `json:"v"`
		Type            string  `json:"type"`
		ConnectionNonce string  `json:"connection_nonce"`
		BootNonce       string  `json:"boot_nonce"`
		Sequence        int64   `json:"seq"`
		Active          bool    `json:"active"`
		LocalStop       bool    `json:"local_stop"`
		SessionID       *string `json:"session_id"`
		MemberID        *string `json:"member_id"`
		Challenge       string  `json:"challenge"`
	}{1, "ride_group_activity", a.connection, activity.BootNonce, activity.Sequence, activity.Active, activity.LocalStop, activity.SessionID, activity.MemberID, activity.Challenge}
	encoded, err := json.Marshal(body)
	if err == nil && len(encoded) <= MaxEnvelopeBytes {
		topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
		priority := 2
		if activity.LocalStop {
			priority = 4
		}
		a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, qos: qos, payload: encoded, expiresAt: time.Now().Add(leaseDuration), priority: priority, kind: "activity", activityBootNonce: activity.BootNonce, activitySequence: activity.Sequence, activityActive: activity.Active, activityLocalStop: activity.LocalStop})
	}
}

func (a *Adapter) publishInactiveLocked() {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	challenge, err := newUUID()
	if err != nil {
		return
	}
	a.stopSequence++
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: a.bootNonce, Sequence: a.stopSequence, Active: false, LocalStop: false, Challenge: challenge}
	a.publishActivityLocked(activity)
}

func (a *Adapter) publishLocalStopLocked() {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	challenge, err := newUUID()
	if err != nil {
		return
	}
	a.stopSequence++
	a.localStopPending = true
	activity := Activity{Version: 1, Type: "ride_group_activity", BootNonce: a.bootNonce, Sequence: a.stopSequence, Active: false, LocalStop: true, Challenge: challenge}
	a.publishActivityQoSLocked(activity, 1)
}

func (a *Adapter) pruneChallengesLocked(now time.Time) {
	for challenge, sent := range a.challenges {
		if now.Sub(sent) >= leaseDuration {
			delete(a.challenges, challenge)
		}
	}
	for len(a.challenges) >= 10 {
		var oldest string
		var oldestAt time.Time
		for challenge, sent := range a.challenges {
			if oldest == "" || sent.Before(oldestAt) {
				oldest, oldestAt = challenge, sent
			}
		}
		delete(a.challenges, oldest)
	}
}

func (a *Adapter) setPeerReplayScopeLocked(control Control) {
	scope := peerReplayScope{sessionID: *control.SessionID}
	if a.peerScope != scope {
		a.peerStreams = make(map[string]peerReplayState)
		a.peerScope = scope
	}
	if a.peerStreams == nil {
		a.peerStreams = make(map[string]peerReplayState)
	}
	authorized := make(map[string]struct{}, len(control.Peers))
	for _, peer := range control.Peers {
		authorized[peer.PeerID] = struct{}{}
	}
	for peerID := range a.peerStreams {
		if _, exists := authorized[peerID]; !exists {
			delete(a.peerStreams, peerID)
		}
	}
}

func (a *Adapter) peerInGrantLocked(peerID string) bool {
	if a.grant == nil {
		return false
	}
	for _, peer := range a.grant.Peers {
		if peer.PeerID == peerID {
			return true
		}
	}
	return false
}

func (a *Adapter) failClosed() {
	a.mu.Lock()
	if a.activity != nil && a.activity.Active {
		a.publishInactiveLocked()
	}
	a.settingsReady = false
	a.activity = nil
	a.activityAt = time.Time{}
	a.challenges = make(map[string]time.Time)
	a.clearGrantLocked()
	a.mu.Unlock()
}

func newUUID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], id[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], id[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], id[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], id[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], id[10:16])
	return string(encoded), nil
}
