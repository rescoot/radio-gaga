// Package redisbus is a single subscriber that fans Redis pub/sub
// notifications out to multiple in-process consumers, doing one HGETALL per
// (debounced) pub/sub event instead of one per consumer.
package redisbus

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// HashHandler receives the full hash contents after a change. With debounce
// enabled, multiple publishes within the window collapse into a single call.
type HashHandler func(ctx context.Context, fields map[string]string)

// PayloadHandler receives one publish payload (typically the changed field
// name). It fires once per publish, even when HashHandlers are coalesced.
type PayloadHandler func(ctx context.Context, payload string)

type Bus struct {
	rdb      *redis.Client
	debounce time.Duration

	mu              sync.RWMutex
	hashHandlers    map[string][]HashHandler
	payloadHandlers map[string][]PayloadHandler

	pendingMu sync.Mutex
	pending   map[string]*pendingHash
}

type pendingHash struct {
	timer    *time.Timer
	payloads []string
}

// New creates a Bus. debounce <= 0 disables coalescing.
func New(rdb *redis.Client, debounce time.Duration) *Bus {
	return &Bus{
		rdb:             rdb,
		debounce:        debounce,
		hashHandlers:    make(map[string][]HashHandler),
		payloadHandlers: make(map[string][]PayloadHandler),
		pending:         make(map[string]*pendingHash),
	}
}

// OnHash registers a handler that fires once per debounce window after the
// hash changes, with the current full hash contents.
func (b *Bus) OnHash(channel string, h HashHandler) {
	b.mu.Lock()
	b.hashHandlers[channel] = append(b.hashHandlers[channel], h)
	b.mu.Unlock()
}

// OnPayload registers a handler that fires once per publish on the given
// channel, with the publish payload. Not coalesced.
func (b *Bus) OnPayload(channel string, h PayloadHandler) {
	b.mu.Lock()
	b.payloadHandlers[channel] = append(b.payloadHandlers[channel], h)
	b.mu.Unlock()
}

// Channels returns the union of channels with at least one handler registered.
func (b *Bus) Channels() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := make(map[string]struct{}, len(b.hashHandlers)+len(b.payloadHandlers))
	for ch := range b.hashHandlers {
		seen[ch] = struct{}{}
	}
	for ch := range b.payloadHandlers {
		seen[ch] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for ch := range seen {
		out = append(out, ch)
	}
	return out
}

// Start runs the subscriber loop until ctx is cancelled. Register handlers
// before calling Start; channels added afterwards are not picked up.
func (b *Bus) Start(ctx context.Context) {
	channels := b.Channels()
	if len(channels) == 0 {
		log.Println("[redisbus] no channels registered, exiting")
		return
	}

	pubsub := b.rdb.Subscribe(ctx, channels...)
	defer pubsub.Close()

	log.Printf("[redisbus] subscribed to %d channels (debounce=%s)", len(channels), b.debounce)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			b.cancelPending()
			return
		case msg := <-ch:
			if msg == nil {
				continue
			}
			b.dispatch(ctx, msg.Channel, msg.Payload)
		}
	}
}

func (b *Bus) dispatch(ctx context.Context, hash, payload string) {
	if b.debounce <= 0 {
		b.fanOut(ctx, hash, []string{payload})
		return
	}

	b.pendingMu.Lock()
	p, ok := b.pending[hash]
	if !ok {
		p = &pendingHash{}
		b.pending[hash] = p
		h := hash
		p.timer = time.AfterFunc(b.debounce, func() {
			b.pendingMu.Lock()
			payloads := p.payloads
			delete(b.pending, h)
			b.pendingMu.Unlock()
			b.fanOut(ctx, h, payloads)
		})
	}
	p.payloads = append(p.payloads, payload)
	b.pendingMu.Unlock()
}

func (b *Bus) fanOut(ctx context.Context, hash string, payloads []string) {
	b.mu.RLock()
	hh := b.hashHandlers[hash]
	ph := b.payloadHandlers[hash]
	b.mu.RUnlock()

	if len(hh) > 0 {
		fields, err := b.rdb.HGetAll(ctx, hash).Result()
		if err != nil {
			log.Printf("[redisbus] HGetAll %s: %v", hash, err)
		} else {
			for _, h := range hh {
				h(ctx, fields)
			}
		}
	}
	for _, p := range payloads {
		for _, h := range ph {
			h(ctx, p)
		}
	}
}

func (b *Bus) cancelPending() {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	for h, p := range b.pending {
		p.timer.Stop()
		delete(b.pending, h)
	}
}
