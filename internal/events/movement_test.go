package events

import (
	"path/filepath"
	"testing"
)

type movementTestPublisher struct{ connected bool }

func (p movementTestPublisher) PublishEvent(Event) error { return nil }
func (p movementTestPublisher) IsConnected() bool        { return p.connected }

type movementTestListener struct{ received []Event }

func (l *movementTestListener) Notify(event Event)       { l.received = append(l.received, event) }
func (l *movementTestListener) ShouldNotify(string) bool { return true }
func (l *movementTestListener) ChannelName() string      { return "test" }

func TestMovementVerdictUsesDetectorEventPipeline(t *testing.T) {
	listener := &movementTestListener{}
	detector := &Detector{
		publisher: movementTestPublisher{},
		buffer:    NewBuffer(filepath.Join(t.TempDir(), "events.jsonl"), 3),
		listeners: []EventListener{listener},
	}
	monitor := NewMovementMonitor(nil)
	monitor.SetEventSink(detector.sendEvent)

	monitor.publishVerdict(StatusConfirmed, "", gpsSample{lat: 52, lng: 13},
		[]gpsSample{{lat: 52.001, lng: 13.001}}, "stand-by", "")

	if len(listener.received) != 1 || listener.received[0].EventType != EventTypeUnauthorizedMovement ||
		listener.received[0].Status != StatusConfirmed {
		t.Fatalf("listener received %#v", listener.received)
	}
	detector.buffer.mu.Lock()
	defer detector.buffer.mu.Unlock()
	if len(detector.buffer.events) != 1 || detector.buffer.events[0].Event.EventType != EventTypeUnauthorizedMovement ||
		detector.buffer.events[0].Event.Status != StatusConfirmed {
		t.Fatalf("buffered events = %#v", detector.buffer.events)
	}
}
