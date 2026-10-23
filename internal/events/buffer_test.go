package events

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type bufferTestSender struct {
	mu      sync.Mutex
	events  []Event
	started chan struct{}
	release chan struct{}
	start   sync.Once
	unblock sync.Once
	err     error
}

func newBufferTestSender(t *testing.T, err error) *bufferTestSender {
	t.Helper()
	s := &bufferTestSender{started: make(chan struct{}), release: make(chan struct{}), err: err}
	t.Cleanup(s.resume)
	return s
}

func (s *bufferTestSender) send(event Event) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	s.start.Do(func() { close(s.started) })
	<-s.release
	return s.err
}

func (s *bufferTestSender) resume() { s.unblock.Do(func() { close(s.release) }) }

func (s *bufferTestSender) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func bufferTestEvent(status string) Event {
	return Event{EventType: EventTypeAlarm, Status: status, Timestamp: "2006-09-07T15:47:18Z", Data: map[string]interface{}{"original": "preserved"}}
}

func waitForBuffer(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("buffer operation did not complete")
	}
}

func startBufferFlush(b *Buffer, sender func(Event) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Flush(sender)
	}()
	return done
}

func bufferedSnapshot(b *Buffer) []BufferedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BufferedEvent(nil), b.events...)
}

func TestBufferConcurrentFlushesSendEachEventOnce(t *testing.T) {
	t.Parallel()
	b := NewBuffer(filepath.Join(t.TempDir(), "events.jsonl"), 3)
	first, second := bufferTestEvent("first"), bufferTestEvent("second")
	b.Add(first)
	b.Add(second)
	sender := newBufferTestSender(t, nil)
	owner := startBufferFlush(b, sender.send)
	waitForBuffer(t, sender.started)

	var callers sync.WaitGroup
	for range 32 {
		callers.Add(1)
		go func() { defer callers.Done(); b.Flush(sender.send) }()
	}
	coalesced := make(chan struct{})
	go func() { callers.Wait(); close(coalesced) }()
	waitForBuffer(t, coalesced)
	if got := len(sender.snapshot()); got != 1 {
		t.Fatalf("concurrent flushes started %d sends before the owner finished", got)
	}
	sender.resume()
	waitForBuffer(t, owner)
	if got := sender.snapshot(); !reflect.DeepEqual(got, []Event{first, second}) {
		t.Fatalf("sent events = %#v", got)
	}
	if got := b.Count(); got != 0 {
		t.Fatalf("buffer contains %d events after successful flush", got)
	}
}

func TestBufferAddDuringFlushSurvivesAndReloads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	b := NewBuffer(path, 3)
	first, second := bufferTestEvent("first"), bufferTestEvent("second")
	b.Add(first)
	sender := newBufferTestSender(t, nil)
	owner := startBufferFlush(b, sender.send)
	waitForBuffer(t, sender.started)
	b.Add(second)
	if got := NewBuffer(path, 3).Count(); got != 2 {
		t.Fatalf("in-flight snapshot and appended event were not persisted: %d", got)
	}
	sender.resume()
	waitForBuffer(t, owner)
	want := []BufferedEvent{{Event: second, Retries: 0}}
	for _, got := range [][]BufferedEvent{bufferedSnapshot(b), bufferedSnapshot(NewBuffer(path, 3))} {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("appended event was changed or lost: %#v", got)
		}
	}
	b.Flush(sender.send)
	if got := sender.snapshot(); !reflect.DeepEqual(got, []Event{first, second}) {
		t.Fatalf("sent events = %#v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("drained buffer file still exists: %v", err)
	}
}

func TestBufferCoalescedFlushDrainsAppendedEvents(t *testing.T) {
	t.Parallel()
	b := NewBuffer(filepath.Join(t.TempDir(), "events.jsonl"), 3)
	first, second := bufferTestEvent("first"), bufferTestEvent("second")
	b.Add(first)
	sender := newBufferTestSender(t, nil)
	owner := startBufferFlush(b, sender.send)
	waitForBuffer(t, sender.started)
	b.Add(second)
	waitForBuffer(t, startBufferFlush(b, sender.send))
	sender.resume()
	waitForBuffer(t, owner)
	if got := sender.snapshot(); !reflect.DeepEqual(got, []Event{first, second}) {
		t.Fatalf("coalesced flush did not drain appended event exactly once: %#v", got)
	}
	if got := b.Count(); got != 0 {
		t.Fatalf("coalesced request left %d events buffered", got)
	}
}

func TestBufferFailedFlushPreservesNewEventsAndRetryMetadata(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	b := NewBuffer(path, 3)
	first, second := bufferTestEvent("first"), bufferTestEvent("second")
	b.Add(first)
	sender := newBufferTestSender(t, errors.New("offline"))
	owner := startBufferFlush(b, sender.send)
	waitForBuffer(t, sender.started)
	b.Add(second)
	sender.resume()
	waitForBuffer(t, owner)
	want := []BufferedEvent{{Event: first, Retries: 1}, {Event: second, Retries: 0}}
	for _, got := range [][]BufferedEvent{bufferedSnapshot(b), bufferedSnapshot(NewBuffer(path, 3))} {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("failed/new events or retry metadata changed: %#v", got)
		}
	}
	recovered := newBufferTestSender(t, nil)
	recovered.resume()
	b.Flush(recovered.send)
	if got := recovered.snapshot(); !reflect.DeepEqual(got, []Event{first, second}) {
		t.Fatalf("retry sent events = %#v", got)
	}
	if got := b.Count(); got != 0 {
		t.Fatalf("successful retry left %d events buffered", got)
	}
}

func TestBufferConcurrentRetryRequestsCoalesceAndRespectBudget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	b := NewBuffer(path, 2)
	event := bufferTestEvent("first")
	b.Add(event)
	sender := newBufferTestSender(t, errors.New("offline"))
	owner := startBufferFlush(b, sender.send)
	waitForBuffer(t, sender.started)
	for range 20 {
		waitForBuffer(t, startBufferFlush(b, sender.send))
	}
	sender.resume()
	waitForBuffer(t, owner)
	if got := len(sender.snapshot()); got != 2 {
		t.Fatalf("coalesced callers caused %d attempts, want 2", got)
	}
	want := []BufferedEvent{{Event: event, Retries: 2}}
	if got := bufferedSnapshot(NewBuffer(path, 2)); !reflect.DeepEqual(got, want) {
		t.Fatalf("retry budget was not persisted: %#v", got)
	}
	b.Flush(func(Event) error { t.Error("retry-exhausted event was sent"); return nil })
	if got := b.Count(); got != 0 {
		t.Fatalf("retry-exhausted event was not discarded: %d", got)
	}
}
