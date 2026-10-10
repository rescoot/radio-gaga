package ridegroup

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestV2RedisReadinessAfterConnectionRestartsDiscovery(t *testing.T) {
	adapter, client := newV2AdapterTest(t)
	adapter.settingsReady = false
	adapter.journalReady = false
	adapter.journalStateV2 = nil
	adapter.OnConnected(client)
	if err := adapter.refreshBlocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sentTypeCount(client, v2HelloType) > 0 })
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.journalProbesV2) == 0 {
		t.Fatal("connection has no reconciliation probe")
	}
}

func TestV2BackendTransportConformance(t *testing.T) {
	path := os.Getenv("RIDE_GROUP_CONFORMANCE_INPUT")
	if path == "" {
		t.Skip("cross-repository trace not supplied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var flows [][]string
	if err = json.Unmarshal(data, &flows); err != nil {
		t.Fatal(err)
	}
	peers := 0
	for _, flow := range flows {
		for _, frame := range flow {
			var header struct {
				Type string `json:"type"`
			}
			if err = json.Unmarshal([]byte(frame), &header); err != nil {
				t.Fatal(err)
			}
			switch header.Type {
			case v2ControlType:
				_, err = ParseControlV2([]byte(frame))
			case v2PeerType:
				_, err = ParsePeerPositionV2([]byte(frame))
				peers++
			default:
				t.Fatalf("unexpected server envelope %s", header.Type)
			}
			if err != nil {
				t.Fatalf("backend envelope rejected: %v", err)
			}
		}
	}
	if peers != 30 {
		t.Fatalf("expected six riders/all five peers, got %d", peers)
	}
	recipients := 0
	var deliveredFlows [][]string
	for _, flow := range flows {
		var grant ControlV2
		var points [][]byte
		for _, frame := range flow {
			var h struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(frame), &h)
			if h.Type == v2ControlType {
				v, _ := ParseControlV2([]byte(frame))
				if v.Action == "grant" {
					grant = v
				}
			} else if h.Type == v2PeerType {
				points = append(points, []byte(frame))
			}
		}
		if len(points) == 0 {
			continue
		}
		if len(points) != 5 {
			t.Fatal("fanout missing peer")
		}
		adapter, _ := newV2AdapterTest(t)
		adapter.connection = grant.ConnectionNonce
		adapter.activityV2.SessionID = grant.SessionID
		adapter.activityV2.MemberID = grant.MemberID
		adapter.challenges[grant.Challenge] = time.Now()
		feedV2Grant(t, adapter, grant)
		adapter.mu.Lock()
		accepted := adapter.grantV2 != nil
		adapter.mu.Unlock()
		if !accepted {
			t.Fatal("backend grant denied by adapter")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		subscription := adapter.redis.Subscribe(ctx, peersChannel)
		if _, err := subscription.Receive(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		grantBytes, _ := EncodeControlV2(grant)
		delivered := []string{string(grantBytes)}
		for _, point := range points {
			adapter.handlePeerV2(point)
			message, err := subscription.ReceiveMessage(ctx)
			if err != nil {
				cancel()
				subscription.Close()
				t.Fatal("adapter did not forward backend peer", err)
			}
			original, _ := ParsePeerPositionV2(point)
			forwarded, err := ParsePeerPositionV2([]byte(message.Payload))
			if err != nil || forwarded.ValidForMS <= 0 || forwarded.ValidForMS > original.ValidForMS {
				t.Fatal("adapter extended or invalidated peer validity", err)
			}
			var before, after map[string]any
			_ = json.Unmarshal(point, &before)
			_ = json.Unmarshal([]byte(message.Payload), &after)
			for _, key := range []string{"valid_for_ms", "server_time_ms"} {
				delete(before, key)
				delete(after, key)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("adapter changed peer authority or producer data")
			}
			delivered = append(delivered, message.Payload)
		}
		subscription.Close()
		cancel()
		recipients++
		deliveredFlows = append(deliveredFlows, delivered)
	}
	if path := os.Getenv("RIDE_GROUP_ADAPTER_OUTPUT"); path != "" {
		data, err := json.Marshal(deliveredFlows)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if recipients != 6 {
		t.Fatal("missing recipient adapter")
	}
}
