package ridegroup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

const v2AttemptLifetime = 10 * time.Second

type probePurposeV2 uint8

const (
	probeReconcileV2 probePurposeV2 = iota
	probeApplyConfirmationV2
	probeOfferV2
)

type localProbeV2 struct {
	challenge   string
	connection  string
	issuedAt    time.Time
	deadline    time.Time
	purpose     probePurposeV2
	operationID string
	revision    int64
	fence       *int64
	ipcEpoch    uint64
	consumed    bool
}

type localStopAttemptV2 struct {
	LocalStopAttemptV2
	mode     string
	fence    *int64
	deadline time.Time
	ipcEpoch uint64
	consumed bool
}

type preparedQueryV2 struct {
	operationID    string
	revision       int64
	fence          int64
	connection     string
	ipcEpoch       uint64
	deadline       time.Time
	queryChallenge string
}

func (a *Adapter) invalidateLocalAttemptsV2Locked() {
	if a.journalProbesV2 == nil {
		a.journalProbesV2 = make(map[string]*localProbeV2)
	}
	if a.localAttemptsV2 == nil {
		a.localAttemptsV2 = make(map[string]*localStopAttemptV2)
	}
	if a.usedLocalChallenges == nil {
		a.usedLocalChallenges = make(map[string][]byte)
	}
	a.journalProbesV2 = make(map[string]*localProbeV2)
	a.localAttemptsV2 = make(map[string]*localStopAttemptV2)
	a.usedLocalChallenges = make(map[string][]byte)
	a.pendingQueryV2 = nil
}

func (a *Adapter) clearGrantV2Locked() {
	a.positionGeneration++
	a.dropPendingPosition()
	a.dropPendingV2Source()
	a.grantV2 = nil
	a.grantActive = false
	a.grantAt = time.Time{}
	a.grantTTL = 0
	a.lastPositionAt = time.Time{}
	a.estimator.Reset()
}

func (a *Adapter) grantCurrentV2Locked(now time.Time) bool {
	return a.protocolVersion == v2ProtocolVersion && a.settingsReady && a.journalReady && !a.blocked && !a.journalBlockedV2 && a.journalStateV2 != nil && a.journalStateV2.Phase == "idle" && a.mqtt != nil && a.mqtt.IsConnected() && a.grantV2 != nil && a.grantActive && !a.grantAt.IsZero() && !now.Before(a.grantAt) && now.Sub(a.grantAt) < a.grantTTL && a.activityV2 != nil && a.activityV2.Active && now.Sub(a.activityAt) >= 0 && now.Sub(a.activityAt) < leaseDuration
}

func (a *Adapter) publishHelloV2Locked(challenge string) {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	if challenge == "" {
		var err error
		challenge, err = newUUID()
		if err != nil {
			return
		}
	}
	value := HelloV2{Version: 2, Type: v2HelloType, ConnectionNonce: a.connection, Capabilities: []string{"ride_group_v2"}, Challenge: challenge}
	payload, err := EncodeHelloV2(value)
	if err != nil {
		return
	}
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: payload, priority: 3, kind: "v2_hello", expiresAt: time.Now().Add(v2AttemptLifetime)})
	a.lastHelloAt = a.currentTime()
}

func (a *Adapter) publishActivityV2Locked(activity ActivityV2) {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return
	}
	value := WireActivityV2{Version: 2, Type: v2ActivityType, ConnectionNonce: a.connection, BootNonce: activity.BootNonce, Sequence: activity.Sequence, Active: activity.Active, LocalStop: false, SessionID: activity.SessionID, MemberID: activity.MemberID, Challenge: activity.Challenge}
	payload, err := EncodeWireActivityV2(value)
	if err != nil {
		return
	}
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: payload, expiresAt: time.Now().Add(5 * time.Second), priority: 2, kind: "v2_activity", activityBootNonce: activity.BootNonce, activitySequence: activity.Sequence, activityActive: activity.Active})
}

func (a *Adapter) issueLocalProbeV2Locked(purpose probePurposeV2, query *preparedQueryV2) bool {
	if a.protocolVersion != 2 || a.redis == nil || !a.settingsReady || a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" {
		return false
	}
	if a.journalProbesV2 == nil {
		a.journalProbesV2 = make(map[string]*localProbeV2)
	}
	now := a.currentTime()
	for challenge, pending := range a.journalProbesV2 {
		if now.After(pending.deadline) || pending.connection != a.connection || pending.ipcEpoch != a.journalIPCEpochV2 {
			delete(a.journalProbesV2, challenge)
		}
	}
	if len(a.journalProbesV2) >= 1 {
		return false
	}
	cutoff := now.Add(-v2AttemptLifetime)
	kept := a.probeTimesV2[:0]
	for _, issued := range a.probeTimesV2 {
		if !issued.Before(cutoff) {
			kept = append(kept, issued)
		}
	}
	a.probeTimesV2 = kept
	if len(a.probeTimesV2) >= 10 {
		return false
	}
	challenge, err := newUUID()
	if err != nil {
		return false
	}
	probe := LocalStopProbeV2{Version: 2, Type: v2LocalStopProbeType, Adapter: "mqtt", Challenge: challenge}
	payload, err := EncodeLocalStopProbeV2(probe)
	if err != nil {
		return false
	}
	attempt := &localProbeV2{challenge: challenge, connection: a.connection, issuedAt: now, deadline: now.Add(v2AttemptLifetime), purpose: purpose, ipcEpoch: a.journalIPCEpochV2}
	if query != nil {
		attempt.operationID, attempt.revision, attempt.fence = query.operationID, query.revision, int64Pointer(query.fence)
		if attempt.deadline.After(query.deadline) {
			attempt.deadline = query.deadline
		}
	}
	a.journalProbesV2[challenge] = attempt
	a.probeTimesV2 = append(a.probeTimesV2, now)
	a.queueRedisPublication(controlChannel, payload, v2AttemptLifetime)
	return true
}

func (a *Adapter) handleActivityV2(payload []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case v2LocalStopType:
		state, err := ParseLocalStopV2(payload)
		if err == nil {
			a.handleLocalStopSnapshotV2(state, payload)
		}
	case v2ActivityType:
		activity, err := ParseActivityV2(payload)
		if err == nil {
			a.acceptActivityV2(activity)
		}
	}
}

func (a *Adapter) handleLocalStopSnapshotV2(state LocalStopV2, _ []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	canonical, err := MarshalCanonicalV2(state)
	if err != nil {
		return
	}
	probe := a.journalProbesV2[state.Challenge]
	now := a.currentTime()
	if prior, exists := a.usedLocalChallenges[state.Challenge]; exists {
		if !bytes.Equal(prior, canonical) {
			a.inhibitJournalV2Locked()
			a.maybeIssueReconciliationProbeV2Locked()
		}
		return
	}
	if probe == nil || probe.consumed || now.Before(probe.issuedAt) || now.After(probe.deadline) || probe.connection != a.connection || probe.ipcEpoch != a.journalIPCEpochV2 || a.mqtt == nil || !a.mqtt.IsConnected() {
		if state.Blocked || state.Phase != "idle" {
			a.inhibitJournalV2Locked()
		}
		a.maybeIssueReconciliationProbeV2Locked()
		return
	}
	if !a.acceptJournalRevisionV2Locked(state) {
		a.inhibitJournalV2Locked()
		a.consumeProbeV2Locked(probe, canonical)
		a.maybeIssueReconciliationProbeV2Locked()
		return
	}
	a.consumeProbeV2Locked(probe, canonical)
	copy := state
	a.journalStateV2 = &copy
	a.journalBlockedV2 = state.Blocked
	a.journalReady = state.Phase == "idle" && !state.Blocked
	if state.Blocked || state.Phase != "idle" {
		a.clearGrantV2Locked()
	}
	if probe.purpose == probeApplyConfirmationV2 {
		a.finalizeApplyConfirmationV2Locked(state, probe)
		return
	}
	switch state.Phase {
	case "idle":
		if !state.Blocked {
			a.publishQueuedV2ActivityLocked()
		}
	case "preparing":
		if state.OperationID != nil && state.ExpectedGeneration == nil && state.CommittedGeneration == nil {
			permit := &localProbeV2{challenge: state.Challenge, connection: probe.connection, issuedAt: probe.issuedAt, deadline: probe.deadline, ipcEpoch: probe.ipcEpoch, operationID: *state.OperationID, revision: state.JournalRevision, purpose: probeOfferV2}
			a.offerPermitV2 = permit
			a.publishHelloV2Locked(state.Challenge)
		}
	case "prepared":
		if state.OperationID != nil && state.ExpectedGeneration != nil && state.CommittedGeneration == nil {
			q := &preparedQueryV2{operationID: *state.OperationID, revision: state.JournalRevision, fence: *state.ExpectedGeneration, connection: a.connection, ipcEpoch: a.journalIPCEpochV2, deadline: probe.deadline, queryChallenge: state.Challenge}
			a.pendingQueryV2 = q
			a.sendStopV2Locked(*state.OperationID, state.JournalRevision, state.ExpectedGeneration, state.Challenge, "query", q.deadline)
		}
	case "uncertain":
		if state.OperationID != nil {
			a.sendStopV2Locked(*state.OperationID, state.JournalRevision, state.ExpectedGeneration, state.Challenge, "query", probe.deadline)
		}
	}
}

func (a *Adapter) acceptJournalRevisionV2Locked(state LocalStopV2) bool {
	previous := a.journalStateV2
	if previous == nil {
		return true
	}
	if state.JournalRevision < previous.JournalRevision {
		return false
	}
	if state.JournalRevision == previous.JournalRevision {
		return state.Phase == previous.Phase && sameStringPtr(state.OperationID, previous.OperationID) && sameIntPtr(state.ExpectedGeneration, previous.ExpectedGeneration) && sameIntPtr(state.CommittedGeneration, previous.CommittedGeneration) && state.Blocked == previous.Blocked
	}
	return true
}

func (a *Adapter) consumeProbeV2Locked(probe *localProbeV2, payload []byte) {
	probe.consumed = true
	delete(a.journalProbesV2, probe.challenge)
	a.usedLocalChallenges[probe.challenge] = append([]byte(nil), payload...)
	for len(a.usedLocalChallenges) > 10 {
		for challenge := range a.usedLocalChallenges {
			delete(a.usedLocalChallenges, challenge)
			break
		}
	}
}

func (a *Adapter) inhibitJournalV2Locked() {
	a.journalReady = false
	a.journalBlockedV2 = true
	a.activityV2 = nil
	a.activity = nil
	a.activityAt = time.Time{}
	a.challenges = make(map[string]time.Time)
	a.offerPermitV2 = nil
	a.pendingQueryV2 = nil
	a.invalidateLocalAttemptsV2Locked()
	a.journalIPCEpochV2++
	a.clearGrantV2Locked()
}

func (a *Adapter) maybeIssueReconciliationProbeV2Locked() {
	if a.protocolVersion == 2 && a.settingsReady && a.mqtt != nil && a.mqtt.IsConnected() {
		a.issueLocalProbeV2Locked(probeReconcileV2, nil)
	}
}

func (a *Adapter) acceptActivityV2(activity ActivityV2) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.settingsReady || !a.journalReady || a.blocked || a.journalBlockedV2 || a.journalStateV2 == nil || a.journalStateV2.Phase != "idle" || a.mqtt == nil || !a.mqtt.IsConnected() {
		return
	}
	if activity.BootNonce == a.activityBootNonce && activity.Sequence <= a.activitySequence {
		return
	}
	if _, exists := a.challenges[activity.Challenge]; exists {
		return
	}
	if activity.Active && a.activityV2 != nil && a.activityV2.SessionID != nil && activity.SessionID != nil && (*a.activityV2.SessionID != *activity.SessionID || *a.activityV2.MemberID != *activity.MemberID) {
		return
	}
	if activity.Active && a.activityV2 != nil && a.activityV2.SessionID != nil && activity.SessionID == nil {
		return
	}
	now := a.currentTime()
	if activity.Active && !a.activityAt.IsZero() && now.Sub(a.activityAt) < time.Second {
		return
	}
	copy := activity
	a.activityV2, a.activity = &copy, nil
	a.activityBootNonce, a.activitySequence = activity.BootNonce, activity.Sequence
	a.activityAt = now
	if !activity.Active {
		a.clearGrantV2Locked()
	}
	if a.grantV2 != nil && a.grantV2.SessionID != nil && activity.SessionID != nil && *a.grantV2.SessionID == *activity.SessionID && *a.grantV2.MemberID == *activity.MemberID {
		a.grantActive = true
	}
	a.challenges[activity.Challenge] = now
	a.pruneChallengesLocked(now)
	a.publishHelloV2Locked(activity.Challenge)
	a.publishActivityV2Locked(activity)
}

func (a *Adapter) publishQueuedV2ActivityLocked() {
	if a.activityV2 != nil && a.journalReady && !a.blocked && !a.journalBlockedV2 {
		a.publishActivityV2Locked(*a.activityV2)
	}
}

func (a *Adapter) handleControlV2(payload []byte) {
	control, err := ParseControlV2(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mqtt == nil || !a.mqtt.IsConnected() || control.ConnectionNonce != a.connection {
		return
	}
	if control.Action == "status" && control.StopResult != nil {
		a.handleStopStatusV2Locked(control)
		return
	}
	if control.Action == "status" {
		a.maybeSendOfferV2Locked(control)
		a.queueRedisPublication(controlChannel, payload, v2AttemptLifetime)
		return
	}
	if control.Action == "clear" {
		if a.grantV2 != nil {
			if !sameStringPtr(a.grantV2.SessionID, control.SessionID) || !sameStringPtr(a.grantV2.MemberID, control.MemberID) || control.TransportEpoch < a.grantV2.TransportEpoch || control.Revision < a.grantV2.Revision || control.ConsentGeneration < a.grantV2.ConsentGeneration || (control.TransportEpoch == a.grantV2.TransportEpoch && !sameStringPtr(a.grantV2.StreamID, control.StreamID)) {
				return
			}
			copy := control
			a.lastClearedV2 = &copy
			a.clearGrantV2Locked()
			a.activityV2 = nil
			a.activityAt = time.Time{}
		} else if a.journalBlockedV2 {
			return
		}
		a.challenges = make(map[string]time.Time)
		a.queueRedisPublication(controlChannel, payload, 0)
		return
	}
	if control.Action != "grant" && control.Action != "renew" {
		return
	}
	if !a.grantChallengeCurrentV2Locked(control.Challenge) || !a.grantableV2Locked() {
		return
	}
	if control.SessionID == nil || control.MemberID == nil || (a.activityV2.SessionID != nil && (!sameStringPtr(a.activityV2.SessionID, control.SessionID) || !sameStringPtr(a.activityV2.MemberID, control.MemberID))) {
		return
	}
	if a.lastClearedV2 != nil && sameStringPtr(a.lastClearedV2.SessionID, control.SessionID) && (control.TransportEpoch <= a.lastClearedV2.TransportEpoch || control.Revision < a.lastClearedV2.Revision) {
		return
	}
	if prior := a.lastAuthorityV2; prior != nil && sameStringPtr(prior.SessionID, control.SessionID) && (control.TransportEpoch < prior.TransportEpoch || control.Revision < prior.Revision || control.ConsentGeneration < prior.ConsentGeneration || (control.TransportEpoch == prior.TransportEpoch && !sameStringPtr(prior.StreamID, control.StreamID))) {
		return
	}
	if a.grantV2 != nil {
		if !sameStringPtr(a.grantV2.SessionID, control.SessionID) || !sameStringPtr(a.grantV2.MemberID, control.MemberID) || control.TransportEpoch < a.grantV2.TransportEpoch || control.Revision < a.grantV2.Revision || control.ConsentGeneration < a.grantV2.ConsentGeneration || (control.TransportEpoch == a.grantV2.TransportEpoch && *control.StreamID != *a.grantV2.StreamID) {
			return
		}
	}
	sentAt := a.challenges[control.Challenge]
	remaining := time.Duration(control.LeaseTTLMS)*time.Millisecond - a.elapsedSince(sentAt)
	if remaining <= 0 {
		return
	}
	newStream := a.grantV2 == nil || control.StreamID == nil || a.grantV2.StreamID == nil || *control.StreamID != *a.grantV2.StreamID || control.TransportEpoch != a.grantV2.TransportEpoch
	if newStream {
		a.positionSeq = 0
		a.sourceSequenceV2 = 0
		a.positionGeneration++
		a.dropPendingPosition()
		a.dropPendingV2Source()
		a.positionStreamID, a.positionSessionID, a.positionEpoch = *control.StreamID, *control.SessionID, control.TransportEpoch
		if a.fixAvailablePendingV2 {
			a.noFixClosedV2 = false
			a.fixAvailablePendingV2 = false
		}
	}
	copy := control
	a.grantV2 = &copy
	a.lastAuthorityV2 = &copy
	a.lastClearedV2 = nil
	a.grantAt, a.grantTTL = a.currentTime(), remaining
	a.grantActive = a.activityV2.SessionID != nil && sameStringPtr(a.activityV2.SessionID, control.SessionID) && sameStringPtr(a.activityV2.MemberID, control.MemberID)
	a.cadence = time.Duration(*control.CadenceMS) * time.Millisecond
	if a.cadence < time.Second {
		a.cadence = time.Second
	}
	a.setPeerReplayScopeLocked(Control{SessionID: control.SessionID, Peers: control.Peers})
	if a.noFixClosedV2 {
		a.publishNoFixStatusV2Locked()
	}
	a.challenges[control.Challenge] = sentAt
	a.queueRedisPublication(controlChannel, payload, leaseDuration-a.elapsedSince(sentAt))
}

func (a *Adapter) grantableV2Locked() bool {
	now := a.currentTime()
	return a.protocolVersion == 2 && a.settingsReady && !a.blocked && a.journalReady && !a.journalBlockedV2 && a.journalStateV2 != nil && a.journalStateV2.Phase == "idle" && a.activityV2 != nil && a.activityV2.Active && now.Sub(a.activityAt) >= 0 && now.Sub(a.activityAt) < leaseDuration
}

func (a *Adapter) grantChallengeCurrentV2Locked(challenge string) bool {
	sentAt, ok := a.challenges[challenge]
	return ok && a.elapsedSince(sentAt) >= 0 && a.elapsedSince(sentAt) < leaseDuration && a.activityV2 != nil && a.activityV2.Active
}

func (a *Adapter) maybeSendOfferV2Locked(control ControlV2) {
	permit := a.offerPermitV2
	if permit == nil || permit.consumed || permit.connection != a.connection || permit.ipcEpoch != a.journalIPCEpochV2 || a.currentTime().After(permit.deadline) || control.Challenge != permit.challenge || control.StopResult != nil || a.journalStateV2 == nil || a.journalStateV2.Phase != "preparing" || a.journalStateV2.OperationID == nil || *a.journalStateV2.OperationID != permit.operationID || a.journalStateV2.JournalRevision != permit.revision {
		return
	}
	result := LocalStopResultV2{Version: 2, Type: v2LocalStopResultType, Adapter: "mqtt", OperationID: permit.operationID, JournalRevision: permit.revision, Challenge: permit.challenge, ConnectionNonce: a.connection, Result: "offer", ExpectedGeneration: int64Pointer(control.ConsentGeneration)}
	encoded, err := EncodeLocalStopResultV2(result)
	if err != nil {
		return
	}
	permit.consumed = true
	a.offerPermitV2 = nil
	a.queueRedisPublication(controlChannel, encoded, time.Until(permit.deadline))
}

func (a *Adapter) sendStopV2Locked(operationID string, revision int64, expected *int64, challenge, mode string, deadline time.Time) bool {
	if a.mqtt == nil || !a.mqtt.IsConnected() || a.connection == "" || a.journalStateV2 == nil || a.journalStateV2.OperationID == nil || *a.journalStateV2.OperationID != operationID || a.journalStateV2.JournalRevision != revision || a.journalIPCEpochV2 == 0 {
		return false
	}
	if mode == "apply" && (a.journalStateV2.Phase != "prepared" || !sameIntPtr(a.journalStateV2.ExpectedGeneration, expected) || a.journalStateV2.CommittedGeneration != nil) {
		return false
	}
	request := StopV2{Version: 2, Type: v2StopType, ConnectionNonce: a.connection, OperationID: operationID, ExpectedConsentGeneration: expected, Challenge: challenge, Mode: mode}
	encoded, err := EncodeStopV2(request)
	if err != nil {
		return false
	}
	now := a.currentTime()
	if deadline.IsZero() || deadline.After(now.Add(v2AttemptLifetime)) {
		deadline = now.Add(v2AttemptLifetime)
	}
	attempt := &localStopAttemptV2{LocalStopAttemptV2: LocalStopAttemptV2{Adapter: "mqtt", OperationID: operationID, JournalRevision: revision, Challenge: challenge, ConnectionNonce: a.connection, StartedAt: now}, mode: mode, fence: cloneInt64(expected), deadline: deadline, ipcEpoch: a.journalIPCEpochV2}
	if a.localAttemptsV2 == nil {
		a.localAttemptsV2 = make(map[string]*localStopAttemptV2)
	}
	for _, pending := range a.localAttemptsV2 {
		if !pending.consumed && now.Before(pending.deadline) {
			return false
		}
	}
	if len(a.localAttemptsV2) >= 10 {
		return false
	}
	a.localAttemptsV2[challenge] = attempt
	topic := fmt.Sprintf("scooters/%s/ride_group/up", a.config.Identifier)
	a.queueMQTTPublication(mqttPublication{client: a.mqtt, connection: a.connection, topic: topic, payload: encoded, expiresAt: deadline, priority: 3, kind: "v2_stop", stopChallenge: challenge})
	return true
}

func (a *Adapter) handleStopStatusV2Locked(control ControlV2) {
	attempt := a.localAttemptsV2[control.Challenge]
	if attempt == nil || attempt.consumed || attempt.ConnectionNonce != a.connection || attempt.ipcEpoch != a.journalIPCEpochV2 || a.currentTime().After(attempt.deadline) || control.StopOperationID == nil || *control.StopOperationID != attempt.OperationID || control.ConnectionNonce != attempt.ConnectionNonce {
		return
	}
	if a.journalStateV2 == nil || a.journalStateV2.OperationID == nil || *a.journalStateV2.OperationID != attempt.OperationID || a.journalStateV2.JournalRevision != attempt.JournalRevision {
		a.inhibitJournalV2Locked()
		return
	}
	if attempt.mode == "apply" && (a.journalStateV2.Phase != "prepared" || !sameIntPtr(a.journalStateV2.ExpectedGeneration, attempt.fence)) {
		a.inhibitJournalV2Locked()
		return
	}
	if *control.StopResult == "committed" && attempt.fence != nil && !sameIntPtr(control.StopExpectedGeneration, attempt.fence) {
		a.inhibitJournalV2Locked()
		return
	}
	attempt.consumed = true
	result := LocalStopResultV2{Version: 2, Type: v2LocalStopResultType, Adapter: "mqtt", OperationID: attempt.OperationID, JournalRevision: attempt.JournalRevision, Challenge: attempt.Challenge, ConnectionNonce: attempt.ConnectionNonce, Result: *control.StopResult, ExpectedGeneration: cloneInt64(control.StopExpectedGeneration)}
	switch *control.StopResult {
	case "committed", "superseded", "unconfirmed":
		result.CommittedGeneration = int64Pointer(control.ConsentGeneration)
	}
	encoded, err := EncodeLocalStopResultV2(result)
	if err == nil {
		a.queueRedisPublication(controlChannel, encoded, attempt.deadline.Sub(a.currentTime()))
	}
	if attempt.mode == "query" && *control.StopResult == "not_found" && a.pendingQueryV2 != nil && a.pendingQueryMatchesV2(attempt) {
		query := *a.pendingQueryV2
		if a.issueLocalProbeV2Locked(probeApplyConfirmationV2, &query) {
			a.pendingQueryV2 = &query
			return
		}
		a.pendingQueryV2 = nil
	}
	if attempt.mode == "apply" {
		a.pendingQueryV2 = nil
	}
}

func (a *Adapter) pendingQueryMatchesV2(attempt *localStopAttemptV2) bool {
	q := a.pendingQueryV2
	return q != nil && q.operationID == attempt.OperationID && q.revision == attempt.JournalRevision && sameIntPtr(attempt.fence, int64Pointer(q.fence)) && q.connection == a.connection && q.ipcEpoch == a.journalIPCEpochV2 && q.queryChallenge == attempt.Challenge && q.deadline.Equal(attempt.deadline) && a.currentTime().Before(q.deadline) && a.journalStateV2 != nil && a.journalStateV2.Phase == "prepared" && a.journalStateV2.OperationID != nil && *a.journalStateV2.OperationID == q.operationID && a.journalStateV2.JournalRevision == q.revision && sameIntPtr(a.journalStateV2.ExpectedGeneration, int64Pointer(q.fence))
}

func (a *Adapter) finalizeApplyConfirmationV2Locked(state LocalStopV2, probe *localProbeV2) {
	query := a.pendingQueryV2
	if state.OperationID == nil || query == nil || probe.purpose != probeApplyConfirmationV2 || probe.connection != a.connection || probe.ipcEpoch != a.journalIPCEpochV2 || !a.currentTime().Before(probe.deadline) || !a.currentTime().Before(query.deadline) || query.operationID != *state.OperationID || query.revision != state.JournalRevision || state.Phase != "prepared" || state.ExpectedGeneration == nil || *state.ExpectedGeneration != query.fence || state.CommittedGeneration != nil || query.connection != a.connection || query.ipcEpoch != a.journalIPCEpochV2 {
		a.pendingQueryV2 = nil
		return
	}
	if a.sendStopV2Locked(query.operationID, query.revision, int64Pointer(query.fence), probe.challenge, "apply", query.deadline) {
		a.pendingQueryV2 = nil
	}
}

func (a *Adapter) handlePeerV2(payload []byte) {
	peer, err := ParsePeerPositionV2(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.grantCurrentV2Locked(a.currentTime()) || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.StreamID == nil || peer.SessionID != *a.grantV2.SessionID || peer.Revision != a.grantV2.Revision || peer.TransportEpoch != a.grantV2.TransportEpoch || peer.StreamID != *a.grantV2.StreamID || !a.peerInGrantLocked(peer.PeerID) {
		return
	}
	previous, exists := a.peerStreams[peer.PeerID]
	if exists && peer.PeerTransportEpoch < previous.epoch {
		return
	}
	if exists && peer.PeerTransportEpoch == previous.epoch && (peer.PeerStreamID != previous.streamID || previous.closed || peer.Sequence <= previous.sequence || peer.CapturedAtMS <= previous.captured) {
		return
	}
	a.peerStreams[peer.PeerID] = peerReplayState{streamID: peer.PeerStreamID, epoch: peer.PeerTransportEpoch, sequence: peer.Sequence, captured: peer.CapturedAtMS}
	a.queueRedisPublication(peersChannel, payload, time.Second)
}

func (a *Adapter) handlePeerStatusV2(payload []byte) {
	status, err := ParsePeerStatusV2(payload)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.grantCurrentV2Locked(a.currentTime()) || a.grantV2 == nil || a.grantV2.SessionID == nil || a.grantV2.StreamID == nil || status.SessionID != *a.grantV2.SessionID || status.Revision != a.grantV2.Revision || status.TransportEpoch != a.grantV2.TransportEpoch || status.StreamID != *a.grantV2.StreamID || !a.peerInGrantLocked(status.PeerID) {
		return
	}
	previous, exists := a.peerStreams[status.PeerID]
	if exists && (status.PeerTransportEpoch < previous.epoch || (status.PeerTransportEpoch == previous.epoch && status.PeerStreamID != previous.streamID)) {
		return
	}
	if !exists || status.PeerTransportEpoch > previous.epoch {
		previous = peerReplayState{streamID: status.PeerStreamID, epoch: status.PeerTransportEpoch}
	}
	previous.status = status.Status
	if status.Status == "inactive" || status.Status == "cleared" || status.Status == "no_fix" {
		previous.closed = true
	}
	a.peerStreams[status.PeerID] = previous
	a.queueRedisPublication(peersChannel, payload, time.Second)
}

func (a *Adapter) currentStopAttemptV2(challenge string) *localStopAttemptV2 {
	return a.localAttemptsV2[challenge]
}

func (a *Adapter) expireV2LeaseLocked(now time.Time) {
	if a.activityV2 != nil && (now.Sub(a.activityAt) >= leaseDuration || now.Before(a.activityAt)) {
		a.activityV2 = nil
		a.activity = nil
		a.activityAt = time.Time{}
		a.challenges = make(map[string]time.Time)
		a.clearGrantV2Locked()
	}
	if a.grantV2 != nil && (now.Before(a.grantAt) || now.Sub(a.grantAt) >= a.grantTTL) {
		a.clearGrantV2Locked()
	}
	for challenge, probe := range a.journalProbesV2 {
		if now.After(probe.deadline) || probe.connection != a.connection || probe.ipcEpoch != a.journalIPCEpochV2 {
			delete(a.journalProbesV2, challenge)
		}
	}
	for challenge, attempt := range a.localAttemptsV2 {
		if now.After(attempt.deadline) || attempt.ConnectionNonce != a.connection || attempt.ipcEpoch != a.journalIPCEpochV2 {
			delete(a.localAttemptsV2, challenge)
		}
	}
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func sameIntPtr(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func sameStringPtr(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func int64Pointer(value int64) *int64 { return &value }
func sameControlBindingV2(a, b *ControlV2) bool {
	return a != nil && b != nil && sameStringPtr(a.SessionID, b.SessionID) && sameStringPtr(a.MemberID, b.MemberID) && sameStringPtr(a.StreamID, b.StreamID)
}
func sameControlIdentityV2(a, b *ControlV2) bool {
	return sameControlBindingV2(a, b) && a.Revision == b.Revision
}
func controlV2JSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }
func equalJSON(a, b any) bool        { return bytes.Equal(controlV2JSON(a), controlV2JSON(b)) }
