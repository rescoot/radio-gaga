package handlers

import (
	"context"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestLocationsMergePublishesPersistableRecordNotifications(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sub := client.Subscribe(ctx, "settings")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	params := map[string]interface{}{
		"locations": []interface{}{
			map[string]interface{}{
				"latitude": 52.52, "longitude": 13.405, "label": "Home",
				"created_at": "2026-09-29T10:00:00Z", "last_used_at": "2026-09-29T11:00:00Z",
			},
			map[string]interface{}{
				"latitude": 52.51, "longitude": 13.39, "label": "Work",
			},
		},
	}
	if err := handleLocationsMergeCommand(client, ctx, params); err != nil {
		t.Fatalf("handleLocationsMergeCommand: %v", err)
	}

	messages := sub.Channel()
	for i, want := range []string{"dashboard.saved-locations.0", "dashboard.saved-locations.1"} {
		select {
		case msg := <-messages:
			if msg.Payload != want {
				t.Errorf("notification %d = %q, want %q", i, msg.Payload, want)
			}
		case <-ctx.Done():
			t.Fatalf("notification %d: %v", i, ctx.Err())
		}
	}

	if got := server.HGet("settings", "dashboard.saved-locations.0.label"); got != "Home" {
		t.Errorf("slot 0 label = %q, want Home", got)
	}
	if got := server.HGet("settings", "dashboard.saved-locations.1.label"); got != "Work" {
		t.Errorf("slot 1 label = %q, want Work", got)
	}
}
