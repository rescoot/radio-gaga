package telemetry

import (
	"testing"

	"radio-gaga/internal/models"
)

func TestGetFieldPriority_ExactMatch(t *testing.T) {
	tests := []struct {
		hash     string
		field    string
		expected Priority
	}{
		{"vehicle", "state", Immediate},
		{"vehicle", "seatbox:lock", Immediate},
		{"vehicle", "handlebar:lock-sensor", Immediate},
		{"vehicle", "blinker:state", Immediate},
		{"power-manager", "state", Immediate},
		{"gps", "latitude", Quick},
		{"gps", "longitude", Quick},
		{"battery:0", "charge", Quick},
		{"battery:0", "state", Quick},
		{"aux-battery", "charge", Slow},
		{"cb-battery", "charge", Slow},
		{"ble", "mac-address", Slow},
	}

	for _, tt := range tests {
		t.Run(tt.hash+"["+tt.field+"]", func(t *testing.T) {
			got := GetFieldPriority(tt.hash, tt.field)
			if got != tt.expected {
				t.Errorf("GetFieldPriority(%s, %s) = %v, want %v", tt.hash, tt.field, got, tt.expected)
			}
		})
	}
}

func TestGetFieldPriority_HashFallback(t *testing.T) {
	// Fields not explicitly mapped should fall back to hash priority
	tests := []struct {
		hash     string
		field    string
		expected Priority
	}{
		{"gps", "unknown_field", Quick},       // gps hash is Quick
		{"battery:0", "unknown_field", Quick}, // battery:0 hash is Quick
		{"battery:1", "temperature", Quick},   // battery:1 hash is Quick
	}

	for _, tt := range tests {
		t.Run(tt.hash+"["+tt.field+"]", func(t *testing.T) {
			got := GetFieldPriority(tt.hash, tt.field)
			if got != tt.expected {
				t.Errorf("GetFieldPriority(%s, %s) = %v, want %v", tt.hash, tt.field, got, tt.expected)
			}
		})
	}
}

func TestGetFieldPriority_DefaultMedium(t *testing.T) {
	// Unknown hashes should default to Medium priority
	tests := []struct {
		hash  string
		field string
	}{
		{"engine-ecu", "speed"},
		{"internet", "status"},
		{"modem", "power-state"},
		{"dashboard", "mode"},
		{"system", "mdb-version"},
	}

	for _, tt := range tests {
		t.Run(tt.hash+"["+tt.field+"]", func(t *testing.T) {
			got := GetFieldPriority(tt.hash, tt.field)
			if got != Medium {
				t.Errorf("GetFieldPriority(%s, %s) = %v, want Medium", tt.hash, tt.field, got)
			}
		})
	}
}

func TestGetFieldPriority_NoisyField(t *testing.T) {
	// Noisy fields should return -1
	got := GetFieldPriority("gps", "timestamp")
	if got != -1 {
		t.Errorf("GetFieldPriority(gps, timestamp) = %v, want -1 (noisy)", got)
	}
}

func TestGetPriorityDeadlines(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	deadlines := GetPriorityDeadlines(config)

	if deadlines[Immediate].Seconds() != 1 {
		t.Errorf("Immediate deadline = %v, want 1s", deadlines[Immediate])
	}
	if deadlines[Quick].Seconds() != 5 {
		t.Errorf("Quick deadline = %v, want 5s", deadlines[Quick])
	}
	if deadlines[Medium].Seconds() != 60 {
		t.Errorf("Medium deadline = %v, want 1m", deadlines[Medium])
	}
	if deadlines[Slow].Minutes() != 15 {
		t.Errorf("Slow deadline = %v, want 15m", deadlines[Slow])
	}
}

func TestPriorityNames(t *testing.T) {
	if PriorityNames[Immediate] != "Immediate" {
		t.Errorf("PriorityNames[Immediate] = %s, want Immediate", PriorityNames[Immediate])
	}
	if PriorityNames[Quick] != "Quick" {
		t.Errorf("PriorityNames[Quick] = %s, want Quick", PriorityNames[Quick])
	}
	if PriorityNames[Medium] != "Medium" {
		t.Errorf("PriorityNames[Medium] = %s, want Medium", PriorityNames[Medium])
	}
	if PriorityNames[Slow] != "Slow" {
		t.Errorf("PriorityNames[Slow] = %s, want Slow", PriorityNames[Slow])
	}
}
