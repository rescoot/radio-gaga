package ridegroup

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func newV2AdapterTest(t *testing.T) (*Adapter, *mqttFake) {
	t.Helper()
	r := newTestRedis(t)
	a, err := NewAdapter(context.Background(), Config{Enabled: true, Identifier: "vehicle-1", ProtocolVersion: 2}, r.client)
	if err != nil {
		t.Fatal(err)
	}
	cleanupAdapter(t, a)
	client := &mqttFake{connected: true}
	now := time.Now()
	a.mqtt, a.connection, a.settingsReady, a.journalIPCEpochV2 = client, testConnection, true, 1
	a.journalReady = true
	a.journalStateV2 = &LocalStopV2{Version: 2, Type: v2LocalStopType, Phase: "idle", JournalRevision: 1, Challenge: testChallenge}
	a.activityV2 = &ActivityV2{Version: 2, Type: v2ActivityType, BootNonce: testBoot, Sequence: 1, Active: true, SessionID: strPtr(testSession), MemberID: strPtr(testMember), Challenge: testChallenge}
	a.activityAt = now
	a.challenges[testChallenge] = now
	return a, client
}

func v2Grant(t *testing.T) ControlV2 {
	t.Helper()
	v := ControlV2{Version: 2, Type: v2ControlType, ConnectionNonce: testConnection, Action: "grant", SessionID: strPtr(testSession), MemberID: strPtr(testMember), Revision: 1, TransportEpoch: 1, StreamID: strPtr(testStream), Challenge: testChallenge, ServerTimeMS: time.Now().UnixMilli(), LeaseTTLMS: 10000, CadenceMS: int64Pointer(2000), Peers: []PeerDescriptor{{PeerID: "00000000-0000-4000-8000-000000000009", Label: "Rider", PaletteIndex: 1}}, ConsentGeneration: 7}
	if _, err := EncodeControlV2(v); err != nil {
		t.Fatal(err)
	}
	return v
}
func strPtr(v string) *string { return &v }
func feedV2Grant(t *testing.T, a *Adapter, v ControlV2) {
	t.Helper()
	data, err := EncodeControlV2(v)
	if err != nil {
		t.Fatal(err)
	}
	a.handleControlV2(data)
}
func issueV2Probe(t *testing.T, a *Adapter) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.issueLocalProbeV2Locked(probeReconcileV2, nil) {
		t.Fatal("probe not admitted")
	}
	for challenge := range a.journalProbesV2 {
		return challenge
	}
	t.Fatal("probe missing")
	return ""
}
func sentTypeCount(m *mqttFake, kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.published {
		var h struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(r.payload, &h)
		if h.Type == kind {
			n++
		}
	}
	return n
}

func TestV2PreparedRecoveryQueriesThenFreshProbeAppliesSameFence(t *testing.T) {
	a, client := newV2AdapterTest(t)
	operation := "00000000-0000-4000-8000-000000000020"
	pq := issueV2Probe(t, a)
	state := LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &operation, JournalRevision: 2, Phase: "prepared", Blocked: true, ExpectedGeneration: int64Pointer(7), Challenge: pq}
	a.handleLocalStopSnapshotV2(state, nil)
	waitFor(t, func() bool { return sentTypeCount(client, v2StopType) == 1 })
	a.mu.Lock()
	attempt := a.localAttemptsV2[pq]
	a.mu.Unlock()
	if attempt == nil || attempt.mode != "query" {
		t.Fatal("prepared reconciliation did not query")
	}
	data, err := os.ReadFile("testdata/v2/control-stop-not_found.json")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := ParseControlV2(data)
	if err != nil {
		t.Fatal(err)
	}
	reply.ConnectionNonce, reply.Challenge, reply.StopOperationID = testConnection, pq, &operation
	encoded, err := EncodeControlV2(reply)
	if err != nil {
		t.Fatal(err)
	}
	a.handleControlV2(encoded)
	a.mu.Lock()
	if a.pendingQueryV2 == nil {
		a.mu.Unlock()
		t.Fatal("query prerequisite lost before confirmation")
	}
	var pa string
	for id, p := range a.journalProbesV2 {
		if p.purpose == probeApplyConfirmationV2 {
			pa = id
		}
	}
	a.mu.Unlock()
	if pa == "" || pa == pq {
		t.Fatal("missing fresh confirmation probe")
	}
	a.handleLocalStopSnapshotV2(state, nil)
	if sentTypeCount(client, v2StopType) != 1 {
		t.Fatal("query snapshot replay applied")
	}
	state.Challenge = pa
	a.handleLocalStopSnapshotV2(state, nil)
	waitFor(t, func() bool { return sentTypeCount(client, v2StopType) == 2 })
	client.mu.Lock()
	defer client.mu.Unlock()
	var apply StopV2
	for _, r := range client.published {
		if v, err := ParseStopV2(r.payload); err == nil && v.Mode == "apply" {
			apply = v
		}
	}
	if apply.OperationID != operation || apply.ExpectedConsentGeneration == nil || *apply.ExpectedConsentGeneration != 7 || apply.Challenge != pa {
		t.Fatal("apply changed saved tuple")
	}
}

func TestV2UncertainRecoveryNeverOffersOrApplies(t *testing.T) {
	a, client := newV2AdapterTest(t)
	op := "00000000-0000-4000-8000-000000000021"
	challenge := issueV2Probe(t, a)
	state := LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &op, JournalRevision: 3, Phase: "uncertain", Blocked: true, Challenge: challenge}
	a.handleLocalStopSnapshotV2(state, nil)
	waitFor(t, func() bool { return sentTypeCount(client, v2StopType) == 1 })
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.offerPermitV2 != nil || a.pendingQueryV2 != nil || a.localAttemptsV2[challenge].mode != "query" {
		t.Fatal("uncertain recovery acquired preparation authority")
	}
}

func TestV2UnsolicitedPreparingOnlyVetoesAndProbes(t *testing.T) {
	a, client := newV2AdapterTest(t)
	op := "00000000-0000-4000-8000-000000000021"
	state := LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &op, JournalRevision: 2, Phase: "preparing", Blocked: true, Challenge: testChallenge}
	a.handleLocalStopSnapshotV2(state, nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.offerPermitV2 != nil || a.journalReady || len(a.journalProbesV2) != 1 || sentTypeCount(client, v2StopType) != 0 {
		t.Fatal("unsolicited preparing was authorizing")
	}
}

func TestV2ProbeCorrelatedFreshPreparingPermitsOnlyOffer(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	op := "00000000-0000-4000-8000-000000000021"
	ch := issueV2Probe(t, a)
	a.handleLocalStopSnapshotV2(LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &op, JournalRevision: 2, Phase: "preparing", Blocked: true, Challenge: ch}, nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.offerPermitV2 == nil || a.offerPermitV2.challenge != ch || len(a.localAttemptsV2) != 0 || a.journalReady {
		t.Fatal("preparing did not remain nonauthorizing")
	}
}

func TestV2ApplyConfirmationRejectsMissingIdentityWithoutPanic(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingQueryV2 = &preparedQueryV2{operationID: testMember, revision: 2, fence: 7, connection: testConnection, ipcEpoch: 1, deadline: time.Now().Add(time.Second)}
	a.finalizeApplyConfirmationV2Locked(LocalStopV2{Phase: "idle"}, &localProbeV2{purpose: probeApplyConfirmationV2})
	if a.pendingQueryV2 != nil {
		t.Fatal("invalid confirmation retained")
	}
}

func TestV2RenewalAcceptsNewEpochAndPreservesPeerTombstones(t *testing.T) {
	for _, statusName := range []string{"inactive", "cleared", "no_fix"} {
		t.Run(statusName, func(t *testing.T) {
			a, _ := newV2AdapterTest(t)
			grant := v2Grant(t)
			feedV2Grant(t, a, grant)
			peerID := grant.Peers[0].PeerID
			ps := PeerStatusV2{Version: 2, Type: v2PeerStatusType, SessionID: testSession, Revision: 1, TransportEpoch: 1, StreamID: testStream, PeerID: peerID, PeerStreamID: testBoot, PeerTransportEpoch: 4, Status: statusName}
			data, err := MarshalCanonicalV2(ps)
			if err != nil {
				t.Fatal(err)
			}
			a.handlePeerStatusV2(data)
			a.mu.Lock()
			if !a.peerStreams[peerID].closed {
				a.mu.Unlock()
				t.Fatal("source epoch not terminal")
			}
			a.mu.Unlock()
			grant.TransportEpoch = 2
			grant.StreamID = strPtr("00000000-0000-4000-8000-000000000022")
			feedV2Grant(t, a, grant)
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.grantV2 == nil || a.grantV2.TransportEpoch != 2 {
				t.Fatal("server stream rotation rejected")
			}
			if !a.peerStreams[peerID].closed {
				t.Fatal("renewal erased peer terminal watermark")
			}
		})
	}
}

func TestV2ImmediateInactiveActivityClosesPublication(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	feedV2Grant(t, a, v2Grant(t))
	inactive := *a.activityV2
	inactive.Sequence++
	inactive.Active = false
	inactive.Challenge = "00000000-0000-4000-8000-000000000025"
	a.acceptActivityV2(inactive)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grantV2 != nil || a.grantActive {
		t.Fatal("inactive was throttled or left a grant")
	}
}

func TestV2ProducerFutureDoesNotPoisonRecoveryAndNoFixNeedsNewerSample(t *testing.T) {
	a, client := newV2AdapterTest(t)
	now := time.Now().UTC()
	a.clock = func() time.Time { return now }
	feedV2Grant(t, a, v2Grant(t))
	first, _ := json.Marshal(testGPSFix("52.0", "13.0", now, 3))
	a.handleGPSV2(first)
	waitFor(t, func() bool { return sentTypeCount(client, v2PositionType) == 1 })
	future, _ := json.Marshal(testGPSFix("52.1", "13.1", now.Add(24*time.Hour), 3))
	a.handleGPSV2(future)
	a.mu.Lock()
	if !a.gpsObservedAt.Equal(now) || !a.noFixClosedV2 {
		a.mu.Unlock()
		t.Fatal("future sample poisoned watermark")
	}
	a.mu.Unlock()
	setAdapterTestClock(a, &now, now.Add(time.Second))
	fix, _ := json.Marshal(testGPSFix("52.0", "13.0", now, 3))
	a.handleGPSV2(fix)
	a.mu.Lock()
	if !a.fixAvailablePendingV2 {
		a.mu.Unlock()
		t.Fatal("fresh recovery sample not advertised")
	}
	a.mu.Unlock()
	grant := v2Grant(t)
	grant.TransportEpoch = 2
	grant.StreamID = strPtr("00000000-0000-4000-8000-000000000022")
	feedV2Grant(t, a, grant)
	a.handleGPSV2(fix)
	a.mu.Lock()
	if a.sourceSequenceV2 != 0 {
		a.mu.Unlock()
		t.Fatal("recovery sample republished as a new measurement")
	}
	a.mu.Unlock()
	setAdapterTestClock(a, &now, now.Add(time.Second))
	fresh, _ := json.Marshal(testGPSFix("52.0", "13.0", now, 3))
	a.handleGPSV2(fresh)
	waitFor(t, func() bool { return sentTypeCount(client, v2PositionType) == 2 })
}
