package ridegroup

import (
	"fmt"
	"time"
)

func (a *Adapter) handleGPSV2(payload []byte) {
	capturedAt, hasTimestamp := parseGPSTimestamp(payload)
	fix, fixErr := ParseGPSFix(payload)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.currentTime()
	if hasTimestamp && capturedAt.After(now) {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		a.closeSourceFixV2Locked()
		return
	}
	if !hasTimestamp {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		a.closeSourceFixV2Locked()
		return
	}
	if !capturedAt.After(a.gpsObservedAt) {
		if fixErr != nil {
			a.invalidatePositionPublicationsLocked()
			a.estimator.Reset()
			a.closeSourceFixV2Locked()
		}
		return
	}
	a.gpsObservedAt = capturedAt
	if fixErr != nil || now.Sub(capturedAt) > 5*time.Second {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		a.closeSourceFixV2Locked()
		return
	}
	if a.noFixClosedV2 {
		if a.fixAvailablePendingV2 && a.lastSourceStatusV2 != nil && a.lastSourceStatusV2.CapturedAtMS != nil && now.Sub(time.UnixMilli(*a.lastSourceStatusV2.CapturedAtMS)) <= 5*time.Second {
			a.retryNoFixStatusV2Locked(now)
			return
		}
		if a.publishSourceStatusV2Locked("fix_available", capturedAt) {
			a.fixAvailablePendingV2 = true
			a.recoveryFixAfterV2 = capturedAt
		}
		return
	}
	sample, valid := a.estimator.Observe(fix, now)
	if !valid {
		a.invalidatePositionPublicationsLocked()
		a.estimator.Reset()
		return
	}
	if !a.grantCurrentV2Locked(now) || !a.activityMatchesGrantV2Locked() || sample.CapturedAt.After(now) || now.Sub(sample.CapturedAt) > 5*time.Second || (!a.lastPositionAt.IsZero() && now.Sub(a.lastPositionAt) < a.cadence) {
		return
	}
	position := PositionV2{Version: 2, Type: v2PositionType, SessionID: *a.grantV2.SessionID, Revision: a.grantV2.Revision, TransportEpoch: a.grantV2.TransportEpoch, StreamID: *a.grantV2.StreamID, Sequence: a.sourceSequenceV2 + 1, CapturedAtMS: sample.CapturedAt.UnixMilli(), TimestampSource: "producer_fix", Latitude: sample.Latitude, Longitude: sample.Longitude, AccuracyM: sample.AccuracyM, HeadingDeg: sample.HeadingDeg, SpeedMPS: sample.SpeedMPS, MotionSource: sample.MotionSource}
	encoded, err := EncodePositionV2(position)
	if err != nil {
		return
	}
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, expiresAt: time.Now().Add(5 * time.Second), priority: 1, kind: "v2_position", positionSessionID: *a.grantV2.SessionID, positionMemberID: *a.grantV2.MemberID, positionStreamID: *a.grantV2.StreamID, positionRevision: a.grantV2.Revision, positionEpoch: a.grantV2.TransportEpoch, positionGeneration: a.positionGeneration, positionCapturedAt: sample.CapturedAt, sourceSequence: position.Sequence})
	a.sourceSequenceV2 = position.Sequence
	a.lastPositionAt = now
}

func (a *Adapter) closeSourceFixV2Locked() {
	if a.noFixClosedV2 && (a.lastSourceStatusV2 == nil || a.lastSourceStatusV2.State != "fix_available") {
		a.retryNoFixStatusV2Locked(a.currentTime())
		return
	}
	a.noFixClosedV2 = true
	a.fixAvailablePendingV2 = false
	a.recoveryFixAfterV2 = time.Time{}
	a.invalidatePositionPublicationsLocked()
	a.estimator.Reset()
	a.publishSourceStatusV2Locked("no_fix", time.Time{})
}

func (a *Adapter) publishNoFixStatusV2Locked() {
	if !a.noFixClosedV2 || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.MemberID == nil || a.grantV2.StreamID == nil {
		return
	}
	status := SourceStatusV2{Version: 2, Type: v2SourceStatusType, ConnectionNonce: a.connection, SessionID: *a.grantV2.SessionID, MemberID: *a.grantV2.MemberID, Revision: a.grantV2.Revision, TransportEpoch: a.grantV2.TransportEpoch, StreamID: *a.grantV2.StreamID, Sequence: a.sourceSequenceV2 + 1, State: "no_fix"}
	if a.publishSourceStatusValueV2Locked(status) {
		a.sourceSequenceV2 = status.Sequence
	}
}

func (a *Adapter) publishSourceStatusV2Locked(state string, capturedAt time.Time) bool {
	if !a.grantCurrentV2Locked(a.currentTime()) || !a.activityMatchesGrantV2Locked() || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.MemberID == nil || a.grantV2.StreamID == nil {
		return false
	}
	status := SourceStatusV2{Version: 2, Type: v2SourceStatusType, ConnectionNonce: a.connection, SessionID: *a.grantV2.SessionID, MemberID: *a.grantV2.MemberID, Revision: a.grantV2.Revision, TransportEpoch: a.grantV2.TransportEpoch, StreamID: *a.grantV2.StreamID, Sequence: a.sourceSequenceV2 + 1, State: state}
	if state == "fix_available" {
		if capturedAt.IsZero() || capturedAt.After(a.currentTime()) || a.currentTime().Sub(capturedAt) > 5*time.Second {
			return false
		}
		ms := capturedAt.UnixMilli()
		status.CapturedAtMS = &ms
	} else if state != "no_fix" {
		return false
	}
	if !a.publishSourceStatusValueV2Locked(status) {
		return false
	}
	a.sourceSequenceV2 = status.Sequence
	return true
}

func (a *Adapter) publishSourceStatusValueV2Locked(status SourceStatusV2) bool {
	encoded, err := EncodeSourceStatusV2(status)
	if err != nil {
		return false
	}
	copy := status
	a.lastSourceStatusV2 = &copy
	a.sourceStatusGeneration++
	a.sourceStatusRetryAt = a.currentTime().Add(3 * time.Second)
	a.dropPendingV2Source()
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	capturedAt := time.Time{}
	if status.CapturedAtMS != nil {
		capturedAt = time.UnixMilli(*status.CapturedAtMS)
	}
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, expiresAt: time.Now().Add(5 * time.Second), priority: 2, kind: "v2_source_status", sourceState: status.State, sourceSequence: status.Sequence, sourceCapturedAt: capturedAt, sourceGeneration: a.sourceStatusGeneration})
	return true
}

func (a *Adapter) retryNoFixStatusV2Locked(now time.Time) {
	status := a.lastSourceStatusV2
	if status == nil || (status.State != "no_fix" && status.State != "fix_available") || a.grantV2 == nil || !a.grantCurrentV2Locked(now) || !a.activityMatchesGrantV2Locked() || now.Before(a.sourceStatusRetryAt) {
		return
	}
	if status.State == "fix_available" && (status.CapturedAtMS == nil || now.Sub(time.UnixMilli(*status.CapturedAtMS)) < 0 || now.Sub(time.UnixMilli(*status.CapturedAtMS)) > 5*time.Second) {
		return
	}
	encoded, err := EncodeSourceStatusV2(*status)
	if err != nil {
		return
	}
	a.sourceStatusRetryAt = now.Add(3 * time.Second)
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	capturedAt := time.Time{}
	if status.CapturedAtMS != nil {
		capturedAt = time.UnixMilli(*status.CapturedAtMS)
	}
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, expiresAt: time.Now().Add(5 * time.Second), priority: 2, kind: "v2_source_status", sourceState: status.State, sourceSequence: status.Sequence, sourceCapturedAt: capturedAt, sourceGeneration: a.sourceStatusGeneration})
}

func (a *Adapter) activityMatchesGrantV2Locked() bool {
	return a.activityV2 != nil && a.activityV2.SessionID != nil && a.activityV2.MemberID != nil && a.grantV2 != nil && a.grantV2.SessionID != nil && a.grantV2.MemberID != nil && *a.activityV2.SessionID == *a.grantV2.SessionID && *a.activityV2.MemberID == *a.grantV2.MemberID
}
