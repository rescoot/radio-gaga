package ridegroup

import (
	"testing"
	"time"
)

func TestV2ProvisionalVetoInvalidatesQueuedStopApply(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	op := testMember
	a.journalStateV2 = &LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &op, JournalRevision: 2, Phase: "prepared", Blocked: true, ExpectedGeneration: int64Pointer(7), Challenge: testChallenge}
	a.journalReady = false
	a.mu.Lock()
	if !a.sendStopV2Locked(op, 2, int64Pointer(7), testChallenge, "apply", time.Now().Add(time.Second)) {
		a.mu.Unlock()
		t.Fatal("apply not queued")
	}
	a.inhibitJournalV2Locked()
	authorized := a.mqttPublicationAuthorizedLocked(mqttPublication{kind: "v2_stop", stopChallenge: testChallenge})
	a.mu.Unlock()
	if authorized {
		t.Fatal("provisional veto retained stop apply authority")
	}
}

func TestV2ApplyConfirmationCannotExtendQueryDeadline(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	q := &preparedQueryV2{operationID: testMember, revision: 2, fence: 7, connection: testConnection, ipcEpoch: a.journalIPCEpochV2, deadline: time.Now().Add(time.Millisecond)}
	if !a.issueLocalProbeV2Locked(probeApplyConfirmationV2, q) {
		t.Fatal("confirmation probe not issued")
	}
	for _, p := range a.journalProbesV2 {
		if p.deadline.After(q.deadline) {
			t.Fatal("confirmation extended prerequisite")
		}
	}
}

func TestV2ExpiredGrantCannotRestoreOlderTransportEpoch(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	c := v2Grant(t)
	c.TransportEpoch = 3
	feedV2Grant(t, a, c)
	a.mu.Lock()
	a.clearGrantV2Locked()
	a.mu.Unlock()
	c.TransportEpoch = 2
	feedV2Grant(t, a, c)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grantV2 != nil {
		t.Fatal("old transport epoch restored after lease loss")
	}
}

func TestV2ActualRedisBoundaryDropsRetiredGrantAndPeer(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	c := v2Grant(t)
	feedV2Grant(t, a, c)
	data, err := EncodeControlV2(c)
	if err != nil {
		t.Fatal(err)
	}
	p := redisPublication{channel: controlChannel, payload: data, queuedAt: time.Now()}
	a.mu.Lock()
	_, valid := a.finalRedisPayloadV2Locked(p)
	if !valid {
		a.mu.Unlock()
		t.Fatal("current grant denied")
	}
	a.inhibitJournalV2Locked()
	_, valid = a.finalRedisPayloadV2Locked(p)
	a.mu.Unlock()
	if valid {
		t.Fatal("retired positive grant crossed Redis boundary")
	}
}

func TestV2CommittedProofCannotReplaceSavedFence(t *testing.T) {
	a, _ := newV2AdapterTest(t)
	op := testMember
	a.journalStateV2 = &LocalStopV2{Version: 2, Type: v2LocalStopType, OperationID: &op, JournalRevision: 2, Phase: "prepared", Blocked: true, ExpectedGeneration: int64Pointer(7), Challenge: testChallenge}
	a.journalReady = false
	a.mu.Lock()
	a.sendStopV2Locked(op, 2, int64Pointer(7), testChallenge, "query", time.Now().Add(time.Second))
	a.mu.Unlock()
	outcome := "committed"
	c := ControlV2{Version: 2, Type: v2ControlType, ConnectionNonce: testConnection, Action: "status", Challenge: testChallenge, ServerTimeMS: time.Now().UnixMilli(), Peers: []PeerDescriptor{}, ConsentGeneration: 9, StopOperationID: &op, StopResult: &outcome, StopExpectedGeneration: int64Pointer(8)}
	data, err := EncodeControlV2(c)
	if err != nil {
		t.Fatal(err)
	}
	a.handleControlV2(data)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.localAttemptsV2) != 0 || !a.journalBlockedV2 {
		t.Fatal("mismatched receipt was retained as proof")
	}
}
