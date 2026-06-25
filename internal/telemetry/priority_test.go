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
		{"gps", "speed", Quick},
		{"gps", "state", Quick},
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
		{"battery:0", "unknown_field", Quick}, // battery:0 hash is Quick
		{"battery:1", "temperature", Quick},   // battery:1 hash is Quick
		{"gps", "unknown_field", Medium},      // gps no longer has a hash priority
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
	// Noisy fields should return -1 (filtered from change detection)
	noisy := []struct{ hash, field string }{
		{"gps", "timestamp"},
		{"internet", "signal-quality"},
		{"gps", "latitude"},
		{"gps", "longitude"},
		{"gps", "altitude"},
		{"gps", "course"},
	}
	for _, tt := range noisy {
		t.Run(tt.hash+"["+tt.field+"]", func(t *testing.T) {
			if got := GetFieldPriority(tt.hash, tt.field); got != -1 {
				t.Errorf("GetFieldPriority(%s, %s) = %v, want -1 (noisy)", tt.hash, tt.field, got)
			}
		})
	}
}

func TestQuantizeForChangeDetection(t *testing.T) {
	tests := []struct {
		name               string
		hash, field, value string
		want               string
	}{
		{"voltage rounds to 50mV", "battery:0", "voltage", "54213", "54200"},
		{"voltage rounds up", "battery:1", "voltage", "54226", "54250"},
		{"current rounds to 100mA", "battery:0", "current", "1249", "1200"},
		{"negative current", "battery:0", "current", "-1280", "-1300"},
		{"motor voltage", "engine-ecu", "motor:voltage", "52140", "52150"},
		{"parked gps speed noise rounds to 0", "gps", "speed", "0.34", "0"},
		{"real gps speed", "gps", "speed", "23.7", "24"},
		{"unmapped field unchanged", "battery:0", "charge", "87", "87"},
		{"non-numeric unchanged", "battery:0", "voltage", "n/a", "n/a"},
		{"empty unchanged", "battery:0", "voltage", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuantizeForChangeDetection(tt.hash, tt.field, tt.value); got != tt.want {
				t.Errorf("QuantizeForChangeDetection(%s, %s, %q) = %q, want %q",
					tt.hash, tt.field, tt.value, got, tt.want)
			}
		})
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
