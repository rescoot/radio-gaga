package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

type faultTestListener struct{ events chan Event }

func (l faultTestListener) Notify(event Event)     { l.events <- event }
func (faultTestListener) ShouldNotify(string) bool { return true }
func (faultTestListener) ChannelName() string      { return "test" }

func TestFaultWatcherWaitsForStreamCreatedAfterStartup(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	listener := faultTestListener{events: make(chan Event, 1)}
	detector := &Detector{
		redisClient: client,
		buffer:      NewBuffer(filepath.Join(t.TempDir(), "events.jsonl"), 3),
		listeners:   []EventListener{listener},
		stopCh:      make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go detector.watchFaultStream(ctx)

	time.Sleep(20 * time.Millisecond)
	if err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: "events:faults",
		Values: map[string]interface{}{"group": "engine", "code": "42"},
	}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	select {
	case event := <-listener.events:
		if event.EventType != EventTypeFault || event.Status != StatusTriggered {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fault event was not delivered after its stream was created")
	}
}
