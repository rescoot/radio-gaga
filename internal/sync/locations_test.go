package sync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })

	return mr, rc
}

func TestReadLocalLocations(t *testing.T) {
	mr, rc := newTestRedis(t)

	mr.HSet("settings",
		"dashboard.saved-locations.0", "52.520008,13.404954,Home,2025-01-01T00:00:00Z,2025-06-15T10:00:00Z",
		"dashboard.saved-locations.1", "48.856613,2.352222,Paris,2025-02-01T00:00:00Z,",
		"dashboard.saved-locations.2", "40.712776,-74.005974",
		"unrelated-key", "should-be-ignored",
	)

	pusher := NewLocationPusher(rc, "http://unused", "42", "tok", 5*time.Second)
	locs, err := pusher.ReadLocalLocations(context.Background())
	if err != nil {
		t.Fatalf("ReadLocalLocations: %v", err)
	}

	if len(locs) != 3 {
		t.Fatalf("expected 3 locations, got %d", len(locs))
	}

	bySlot := map[int]Location{}
	for _, l := range locs {
		bySlot[l.Slot] = l
	}

	if loc := bySlot[0]; loc.Label != "Home" || loc.Latitude != 52.520008 || loc.Longitude != 13.404954 {
		t.Errorf("slot 0 mismatch: %+v", loc)
	}
	if loc := bySlot[1]; loc.Label != "Paris" || loc.CreatedAt != "2025-02-01T00:00:00Z" {
		t.Errorf("slot 1 mismatch: %+v", loc)
	}
	if loc := bySlot[2]; loc.Latitude != 40.712776 || loc.Longitude != -74.005974 || loc.Label != "" {
		t.Errorf("slot 2 mismatch: %+v", loc)
	}
}

func TestPushLocations(t *testing.T) {
	mr, rc := newTestRedis(t)

	mr.HSet("settings",
		"dashboard.saved-locations.0", "52.520008,13.404954,Home,2025-01-01T00:00:00Z,2025-06-15T10:00:00Z",
	)

	var gotMethod, gotPath, gotAuth string
	var gotBody PushRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pusher := NewLocationPusher(rc, srv.URL, "42", "secret-token", 5*time.Second)
	if err := pusher.Push(context.Background()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if gotPath != "/api/v1/scooters/42/locations" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("unexpected auth header: %s", gotAuth)
	}
	if len(gotBody.Locations) != 1 {
		t.Fatalf("expected 1 location in body, got %d", len(gotBody.Locations))
	}
	if gotBody.Locations[0].Label != "Home" {
		t.Errorf("unexpected label: %s", gotBody.Locations[0].Label)
	}
}

func TestPushSuppression(t *testing.T) {
	_, rc := newTestRedis(t)

	pusher := NewLocationPusher(rc, "http://unused", "42", "tok", 5*time.Second)

	if !pusher.ShouldPush() {
		t.Error("ShouldPush should be true initially")
	}

	pusher.markPushed()

	if pusher.ShouldPush() {
		t.Error("ShouldPush should be false right after markPushed")
	}

	// Force the suppression window to expire
	pusher.mu.Lock()
	pusher.suppressUntil = time.Now().Add(-1 * time.Second)
	pusher.mu.Unlock()

	if !pusher.ShouldPush() {
		t.Error("ShouldPush should be true after suppression window expires")
	}
}

func TestPushHTTPError(t *testing.T) {
	mr, rc := newTestRedis(t)

	mr.HSet("settings",
		"dashboard.saved-locations.0", "52.520008,13.404954,Home,,",
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	pusher := NewLocationPusher(rc, srv.URL, "42", "bad-token", 5*time.Second)
	err := pusher.Push(context.Background())
	if err == nil {
		t.Fatal("expected error on 401 response")
	}
}

func TestSuppress(t *testing.T) {
	_, rc := newTestRedis(t)

	pusher := NewLocationPusher(rc, "http://unused", "42", "tok", 5*time.Second)

	pusher.Suppress()

	if pusher.ShouldPush() {
		t.Error("ShouldPush should be false after Suppress()")
	}
}
