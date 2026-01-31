package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"radio-gaga/internal/events"
	"radio-gaga/internal/models"
)

func testConfig() *models.TelegramConfig {
	return &models.TelegramConfig{
		Enabled:   true,
		BotToken:  "test-token",
		ChatID:    "12345",
		RateLimit: "10ms",
		QueueSize: 10,
		Events: map[string]bool{
			"alarm":                 true,
			"unauthorized_movement": true,
			"battery_warning":       true,
			"temperature_warning":   true,
			"state_change":          false,
		},
	}
}

func testScooter() *models.ScooterConfig {
	return &models.ScooterConfig{
		Identifier: "TEST-VIN",
		Name:       "Test Scooter",
	}
}

func TestShouldNotify(t *testing.T) {
	n, err := NewNotifier(testConfig(), testScooter())
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"alarm", true},
		{"unauthorized_movement", true},
		{"battery_warning", true},
		{"temperature_warning", true},
		{"state_change", false},
		{"unknown_event", false},
	}

	for _, tt := range tests {
		if got := n.ShouldNotify(tt.eventType); got != tt.expected {
			t.Errorf("ShouldNotify(%q) = %v, want %v", tt.eventType, got, tt.expected)
		}
	}
}

func TestFormatAlarmArmed(t *testing.T) {
	n, _ := NewNotifier(testConfig(), &models.ScooterConfig{
		Identifier: "WUNU2S3B7MZ000147",
		Name:       "Deep Blue",
	})

	msg := n.FormatMessage(events.NewEvent(events.EventTypeAlarm, "armed", nil))
	assertEq(t, msg, "🔒 Deep Blue alarm armed")
}

func TestFormatAlarmDisarmed(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	msg := n.FormatMessage(events.NewEvent(events.EventTypeAlarm, "disarmed", nil))
	assertEq(t, msg, "🔓 Test Scooter alarm disarmed")
}

func TestFormatAlarmTriggered(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	msg := n.FormatMessage(events.NewEvent(events.EventTypeAlarm, "triggered", nil))
	assertEq(t, msg, "🚨 Test Scooter alarm triggered!")
}

func TestFormatBatteryWarning(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	event := events.NewEvent(events.EventTypeBatteryWarning, "triggered", map[string]interface{}{
		"battery": "battery:0",
		"charge":  5,
	})
	msg := n.FormatMessage(event)
	assertEq(t, msg, "🔋 Test Scooter Battery 1 at 5%")
}

func TestFormatTemperatureWarning(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	event := events.NewEvent(events.EventTypeTemperatureWarning, "triggered", map[string]interface{}{
		"component":   "engine-ecu",
		"temperature": 85,
	})
	msg := n.FormatMessage(event)
	assertEq(t, msg, "🌡️ Test Scooter Engine at 85°C")
}

func TestFormatStateChange(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	event := events.NewEvent(events.EventTypeStateChange, "", map[string]interface{}{
		"from": "standby",
		"to":   "driving",
	})
	msg := n.FormatMessage(event)
	assertEq(t, msg, "🔄 Test Scooter: standby → driving")
}

func TestFormatConnectivity(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())

	lost := n.FormatMessage(events.NewEvent(events.EventTypeConnectivity, events.StatusLost, nil))
	assertEq(t, lost, "📡 Test Scooter lost internet")

	restored := n.FormatMessage(events.NewEvent(events.EventTypeConnectivity, events.StatusRegained, nil))
	assertEq(t, restored, "📡 Test Scooter back online")
}

func TestFormatUnauthorizedMovement(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	event := events.NewEvent(events.EventTypeUnauthorizedMovement, "triggered", map[string]interface{}{
		"gps_speed":     12,
		"vehicle_state": "standby",
	})
	msg := n.FormatMessage(event)
	assertEq(t, msg, "⚠️ Test Scooter is moving while parked (12 km/h)")
}

func TestFormatFaultNrfReset(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())
	event := events.NewEvent(events.EventTypeFault, "triggered", map[string]interface{}{
		"type":   "nrf_reset",
		"reason": "0x4",
		"count":  3,
	})
	msg := n.FormatMessage(event)
	assertEq(t, msg, "⚙️ Test Scooter NRF reset (reason: 0x4, count: 3)")
}

func TestFormatWithoutName(t *testing.T) {
	n, _ := NewNotifier(testConfig(), &models.ScooterConfig{
		Identifier: "WUNU2S3B7MZ000147",
	})
	msg := n.FormatMessage(events.NewEvent(events.EventTypeAlarm, "armed", nil))
	assertEq(t, msg, "🔒 WUNU2S3B7MZ000147 alarm armed")
}

func TestNotifyQueueFull(t *testing.T) {
	cfg := testConfig()
	cfg.QueueSize = 1
	n, err := NewNotifier(cfg, testScooter())
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	event := events.NewEvent(events.EventTypeAlarm, "triggered", nil)
	n.Notify(event)
	n.Notify(event)

	if len(n.queue) != 1 {
		t.Errorf("queue should have 1 event, got %d", len(n.queue))
	}
}

func TestSendToTelegram(t *testing.T) {
	var receivedCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&receivedCount, 1)
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.FormValue("chat_id") != "12345" {
			t.Errorf("unexpected chat_id: %s", r.FormValue("chat_id"))
		}
		if r.FormValue("parse_mode") != "HTML" {
			t.Errorf("unexpected parse_mode: %s", r.FormValue("parse_mode"))
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	n, _ := NewNotifier(testConfig(), testScooter())
	n.client = server.Client()

	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	n.Stop()
}

func TestStartStop(t *testing.T) {
	n, _ := NewNotifier(testConfig(), testScooter())

	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	n.Notify(events.NewEvent(events.EventTypeAlarm, "triggered", nil))
	time.Sleep(50 * time.Millisecond)
	cancel()
	n.Stop()
}

func TestInvalidRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit = "not-a-duration"

	_, err := NewNotifier(cfg, testScooter())
	if err == nil {
		t.Error("expected error for invalid rate_limit")
	}
}

func assertEq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:  %q\nwant: %q", got, want)
	}
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
