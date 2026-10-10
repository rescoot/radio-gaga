package ridegroup

import (
	"context"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	redisEgressQueueSize = 8
	redisPublishTimeout  = time.Second
	mqttPublishTimeout   = 1500 * time.Millisecond
)

type mqttPublication struct {
	client             mqtt.Client
	connection         string
	topic              string
	qos                byte
	retained           bool
	payload            []byte
	expiresAt          time.Time
	priority           int
	kind               string
	order              uint64
	unsubscribe        string
	activityBootNonce  string
	activitySequence   int64
	activityActive     bool
	activityLocalStop  bool
	positionSessionID  string
	positionMemberID   string
	positionStreamID   string
	positionRevision   int64
	positionEpoch      int64
	positionGeneration uint64
	positionCapturedAt time.Time
	stopChallenge      string
	sourceState        string
	sourceSequence     int64
	sourceCapturedAt   time.Time
	sourceGeneration   uint64
}

type redisPublication struct {
	channel   string
	payload   []byte
	expiresAt time.Time
	queuedAt  time.Time
}

func (a *Adapter) startWorkers() {
	a.startMQTTWorker()
	a.startRedisWorker()
	a.startAckWorker()
}

func (a *Adapter) startMQTTWorker() {
	a.mqttWorkerOnce.Do(func() {
		a.egressMu.Lock()
		if a.mqttWake == nil {
			a.mqttWake = make(chan struct{}, 1)
		}
		wake := a.mqttWake
		if a.mqttWorkerDone == nil {
			a.mqttWorkerDone = make(chan struct{})
		}
		done := a.mqttWorkerDone
		a.egressMu.Unlock()
		go func() {
			defer close(done)
			a.runMQTTWorker(wake)
		}()
	})
}

func (a *Adapter) queueMQTTPublication(publication mqttPublication) {
	a.startMQTTWorker()
	a.egressMu.Lock()
	if a.mqttClosing {
		a.egressMu.Unlock()
		return
	}
	if a.mqttPending == nil {
		a.mqttPending = make(map[string]mqttPublication)
	}
	kind := publication.kind
	if publication.unsubscribe != "" {
		kind = "unsubscribe"
		publication.kind = kind
	}
	previous, exists := a.mqttPending[kind]
	replaceClearedStop := kind == "activity" && !publication.activityLocalStop && previous.activityLocalStop
	if !exists || publication.priority >= previous.priority || replaceClearedStop || publication.unsubscribe != "" {
		a.mqttOrder++
		publication.order = a.mqttOrder
		publication.payload = append([]byte(nil), publication.payload...)
		a.mqttPending[kind] = publication
	}
	wake := a.mqttWake
	a.egressMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (a *Adapter) dropPendingPosition() {
	a.egressMu.Lock()
	delete(a.mqttPending, "position")
	delete(a.mqttPending, "v2_position")
	a.egressMu.Unlock()
}

func (a *Adapter) dropPendingV2Source() {
	a.egressMu.Lock()
	delete(a.mqttPending, "v2_source_status")
	a.egressMu.Unlock()
}

func (a *Adapter) mqttPublicationAuthorizedLocked(publication mqttPublication) bool {
	now := a.currentTime()
	switch publication.kind {
	case "v2_hello":
		return a.settingsReady && a.protocolVersion == v2ProtocolVersion && a.mqtt != nil && a.mqtt.IsConnected()
	case "v2_activity":
		if !a.settingsReady || !a.journalReady || a.blocked || a.journalBlockedV2 || a.journalStateV2 == nil || a.journalStateV2.Phase != "idle" || a.activityV2 == nil || a.mqtt == nil || !a.mqtt.IsConnected() {
			return false
		}
		if a.activityV2.BootNonce != publication.activityBootNonce || a.activityV2.Sequence != publication.activitySequence || a.activityV2.Active != publication.activityActive {
			return false
		}
		return now.Sub(a.activityAt) >= 0 && now.Sub(a.activityAt) < leaseDuration
	case "v2_position":
		if !a.grantCurrentV2Locked(now) || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.MemberID == nil || a.grantV2.StreamID == nil || a.activityV2 == nil || a.activityV2.SessionID == nil || a.activityV2.MemberID == nil || a.noFixClosedV2 {
			return false
		}
		age := now.Sub(publication.positionCapturedAt)
		return publication.positionGeneration == a.positionGeneration && *a.activityV2.SessionID == *a.grantV2.SessionID && *a.activityV2.MemberID == *a.grantV2.MemberID && *a.grantV2.SessionID == publication.positionSessionID && *a.grantV2.MemberID == publication.positionMemberID && *a.grantV2.StreamID == publication.positionStreamID && a.grantV2.Revision == publication.positionRevision && a.grantV2.TransportEpoch == publication.positionEpoch && age >= 0 && age <= 5*time.Second
	case "v2_source_status":
		if !a.grantCurrentV2Locked(now) || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.MemberID == nil || a.grantV2.StreamID == nil || a.lastSourceStatusV2 == nil || a.activityV2 == nil || a.activityV2.SessionID == nil || a.activityV2.MemberID == nil {
			return false
		}
		status := a.lastSourceStatusV2
		if publication.sourceGeneration != a.sourceStatusGeneration || publication.sourceSequence != status.Sequence || publication.sourceState != status.State || status.SessionID != *a.grantV2.SessionID || status.MemberID != *a.grantV2.MemberID || status.StreamID != *a.grantV2.StreamID || status.Revision != a.grantV2.Revision || status.TransportEpoch != a.grantV2.TransportEpoch || status.ConnectionNonce != a.connection || *a.activityV2.SessionID != status.SessionID || *a.activityV2.MemberID != status.MemberID {
			return false
		}
		if status.State == "no_fix" {
			return a.noFixClosedV2
		}
		age := now.Sub(publication.sourceCapturedAt)
		return status.CapturedAtMS != nil && age >= 0 && age <= 5*time.Second
	case "v2_stop":
		attempt := a.localAttemptsV2[publication.stopChallenge]
		if attempt == nil || attempt.consumed || attempt.ConnectionNonce != a.connection || attempt.ipcEpoch != a.journalIPCEpochV2 || now.Before(attempt.StartedAt) || now.After(attempt.deadline) || a.journalStateV2 == nil || a.journalStateV2.OperationID == nil || *a.journalStateV2.OperationID != attempt.OperationID || a.journalStateV2.JournalRevision != attempt.JournalRevision {
			return false
		}
		if attempt.mode == "apply" {
			return a.journalStateV2.Phase == "prepared" && sameIntPtr(a.journalStateV2.ExpectedGeneration, attempt.fence) && a.journalStateV2.CommittedGeneration == nil
		}
		return (a.journalStateV2.Phase == "prepared" || a.journalStateV2.Phase == "uncertain") && sameIntPtr(a.journalStateV2.ExpectedGeneration, attempt.fence)
	case "position":
		if !a.settingsReady || a.blocked || a.localStopPending || a.mqtt == nil || a.grant == nil || !a.grantActive || a.grantAt.IsZero() || now.Sub(a.grantAt) < 0 || now.Sub(a.grantAt) >= a.grantTTL || a.activity == nil || !a.activity.Active || a.activity.LocalStop || now.Sub(a.activityAt) < 0 || now.Sub(a.activityAt) >= leaseDuration || a.grant.SessionID == nil || a.grant.MemberID == nil || a.grant.StreamID == nil || a.activity.SessionID == nil || a.activity.MemberID == nil {
			return false
		}
		age := now.Sub(publication.positionCapturedAt)
		return publication.positionGeneration == a.positionGeneration && *a.activity.SessionID == *a.grant.SessionID && *a.activity.MemberID == *a.grant.MemberID && *a.grant.SessionID == publication.positionSessionID && *a.grant.MemberID == publication.positionMemberID && *a.grant.StreamID == publication.positionStreamID && a.grant.Revision == publication.positionRevision && a.grant.TransportEpoch == publication.positionEpoch && age >= 0 && age <= 5*time.Second
	case "activity":
		if publication.activityLocalStop {
			return a.localStopPending
		}
		if publication.activityActive {
			return a.settingsReady && !a.blocked && !a.localStopPending && a.activity != nil && a.activity.Active && !a.activity.LocalStop && a.activityBootNonce == publication.activityBootNonce && a.activitySequence == publication.activitySequence && now.Sub(a.activityAt) >= 0 && now.Sub(a.activityAt) < leaseDuration
		}
		if a.blocked || a.localStopPending {
			return false
		}
		if a.activity == nil {
			return true
		}
		return !a.activity.Active && !a.activity.LocalStop && a.activityBootNonce == publication.activityBootNonce && a.activitySequence == publication.activitySequence
	default:
		return true
	}
}

func (a *Adapter) nextMQTTPublicationLocked() (mqttPublication, bool) {
	var next mqttPublication
	var nextKind string
	found := false
	for kind, publication := range a.mqttPending {
		if !found || publication.order < next.order {
			next = publication
			nextKind = kind
			found = true
		}
	}
	if found {
		delete(a.mqttPending, nextKind)
	}
	return next, found
}

func (a *Adapter) runMQTTWorker(wake <-chan struct{}) {
	for range wake {
		for {
			a.egressMu.Lock()
			publication, exists := a.nextMQTTPublicationLocked()
			closing := a.mqttClosing
			a.egressMu.Unlock()
			if !exists {
				if closing {
					return
				}
				break
			}
			if publication.unsubscribe != "" {
				if publication.client != nil && publication.client.IsConnected() {
					token := publication.client.Unsubscribe(publication.unsubscribe)
					token.WaitTimeout(mqttPublishTimeout)
				}
				continue
			}
			if publication.client == nil || (!publication.expiresAt.IsZero() && !time.Now().Before(publication.expiresAt)) || !publication.client.IsConnected() {
				continue
			}
			a.mu.Lock()
			current := a.mqtt == publication.client && a.connection == publication.connection && a.mqttPublicationAuthorizedLocked(publication)
			a.mu.Unlock()
			if !current {
				continue
			}
			// Paho's Publish may block for its configured WriteTimeout before returning a token.
			token := publication.client.Publish(publication.topic, publication.qos, publication.retained, publication.payload)
			if token != nil {
				token.WaitTimeout(mqttPublishTimeout)
			}
		}
	}
}

func (a *Adapter) stopMQTTWorker(client mqtt.Client, topic string) {
	a.startMQTTWorker()
	a.egressMu.Lock()
	a.mqttClosing = true
	a.mqttPending = make(map[string]mqttPublication)
	if client != nil && topic != "" {
		a.mqttOrder++
		a.mqttPending["unsubscribe"] = mqttPublication{client: client, unsubscribe: topic, priority: 100, kind: "unsubscribe", order: a.mqttOrder}
	}
	wake := a.mqttWake
	a.egressMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (a *Adapter) startRedisWorker() {
	a.redisWorkerOnce.Do(func() {
		if a.redisQueue == nil {
			a.redisQueue = make(chan redisPublication, redisEgressQueueSize)
		}
		a.wg.Add(1)
		go a.runRedisWorker()
	})
}

func (a *Adapter) queueRedisPublication(channel string, payload []byte, maxAge time.Duration) {
	a.startRedisWorker()
	now := time.Now()
	publication := redisPublication{channel: channel, payload: append([]byte(nil), payload...), queuedAt: a.currentTime()}
	if maxAge > 0 {
		publication.expiresAt = now.Add(maxAge)
	}
	select {
	case a.redisQueue <- publication:
	default:
		select {
		case <-a.redisQueue:
		default:
		}
		select {
		case a.redisQueue <- publication:
		default:
		}
	}
}

func (a *Adapter) runRedisWorker() {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case publication := <-a.redisQueue:
			if (!publication.expiresAt.IsZero() && !time.Now().Before(publication.expiresAt)) || a.redis == nil {
				continue
			}
			a.mu.Lock()
			payload, valid := a.finalRedisPayloadV2Locked(publication)
			a.mu.Unlock()
			if !valid {
				continue
			}
			ctx, cancel := context.WithTimeout(a.ctx, redisPublishTimeout)
			_, _ = a.redis.Publish(ctx, publication.channel, payload).Result()
			cancel()
		}
	}
}

func (a *Adapter) startAckWorker() {
	a.ackWorkerOnce.Do(func() {
		if a.ackQueue == nil {
			a.ackQueue = make(chan mqtt.Message, redisEgressQueueSize)
		}
		go func() {
			for {
				select {
				case <-a.ctx.Done():
					return
				case message := <-a.ackQueue:
					message.Ack()
				}
			}
		}()
	})
}

func (a *Adapter) queueAck(message mqtt.Message) {
	if message == nil || message.Qos() == 0 {
		return
	}
	a.startAckWorker()
	select {
	case a.ackQueue <- message:
	default:
		// A full bounded queue leaves QoS1 delivery unacknowledged for broker redelivery.
	}
}
