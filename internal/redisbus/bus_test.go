package redisbus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func newTestBus(t *testing.T, debounce time.Duration) (*Bus, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return New(rdb, debounce), mr, rdb
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestBusFanOutNoDebounce(t *testing.T) {
	bus, mr, rdb := newTestBus(t, 0)
	defer rdb.Close()

	mr.HSet("cb-battery", "charge", "42", "current", "100")

	var hashCalls, payloadCalls int32
	var mu sync.Mutex
	var seenFields map[string]string

	bus.OnHash("cb-battery", func(_ context.Context, fields map[string]string) {
		atomic.AddInt32(&hashCalls, 1)
		mu.Lock()
		seenFields = fields
		mu.Unlock()
	})
	bus.OnPayload("cb-battery", func(_ context.Context, _ string) {
		atomic.AddInt32(&payloadCalls, 1)
	})

	go bus.Start(t.Context())

	waitFor(t, func() bool { return mr.PubSubNumSub("cb-battery")["cb-battery"] == 1 }, time.Second, "subscription")

	mr.Publish("cb-battery", "charge")
	mr.Publish("cb-battery", "current")

	waitFor(t, func() bool { return atomic.LoadInt32(&hashCalls) == 2 }, time.Second, "two hash calls")

	if got := atomic.LoadInt32(&payloadCalls); got != 2 {
		t.Errorf("payload calls = %d, want 2", got)
	}
	mu.Lock()
	if seenFields["charge"] != "42" || seenFields["current"] != "100" {
		t.Errorf("seenFields = %v", seenFields)
	}
	mu.Unlock()
}

func TestBusCoalesces(t *testing.T) {
	bus, mr, rdb := newTestBus(t, 25*time.Millisecond)
	defer rdb.Close()

	mr.HSet("cb-battery", "charge", "42")

	var hashCalls, payloadCalls int32
	bus.OnHash("cb-battery", func(_ context.Context, _ map[string]string) {
		atomic.AddInt32(&hashCalls, 1)
	})
	bus.OnPayload("cb-battery", func(_ context.Context, _ string) {
		atomic.AddInt32(&payloadCalls, 1)
	})

	go bus.Start(t.Context())

	waitFor(t, func() bool { return mr.PubSubNumSub("cb-battery")["cb-battery"] == 1 }, time.Second, "subscription")

	for range 5 {
		mr.Publish("cb-battery", "field")
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&payloadCalls) == 5 }, time.Second, "five payload calls")
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&hashCalls); got != 1 {
		t.Errorf("hash calls = %d, want 1 (coalesced)", got)
	}
}
