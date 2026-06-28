package telemetry

import (
	"math"
	"strconv"
	"strings"
	"time"

	"radio-gaga/internal/models"
)

// Priority represents a telemetry field priority level
type Priority int

const (
	Immediate Priority = iota // 0 - Vehicle state, lock status, blinkers
	Quick                     // 1 - GPS, battery charge
	Medium                    // 2 - Most other fields (default)
	Slow                      // 3 - Aux battery, CBB, BLE
)

// PriorityNames maps priority levels to human-readable names
var PriorityNames = map[Priority]string{
	Immediate: "Immediate",
	Quick:     "Quick",
	Medium:    "Medium",
	Slow:      "Slow",
}

// FieldPriorities maps specific fields to their priority levels
// Format: "hash[field]" -> Priority
var FieldPriorities = map[string]Priority{
	// Immediate priority - critical state changes
	"vehicle[state]":                 Immediate,
	"vehicle[hop-on-active]":         Immediate,
	"vehicle[seatbox:lock]":          Immediate,
	"vehicle[handlebar:lock-sensor]": Immediate,
	"vehicle[blinker:state]":         Immediate,
	"power-manager[state]":           Immediate,

	// Quick priority - frequently changing important data.
	// GPS position (latitude/longitude/altitude/course) is deliberately NOT
	// here: while parked it jitters by a few meters every read and would trip a
	// full-snapshot flush every ~30s, which is the single biggest source of
	// standby traffic. Those fields live in NoisyFields instead, so they ride
	// along in snapshots but never trigger one. gps[speed] stays (quantized to
	// whole units, so parked GPS-speed noise rounds to 0) and gps[state] stays
	// (fix acquired/lost is a real, rare event).
	"gps[speed]":         Quick,
	"gps[state]":         Quick,
	"battery:0[charge]":  Quick,
	"battery:0[state]":   Quick,
	"battery:0[present]": Quick,
	"battery:1[charge]":  Quick,
	"battery:1[state]":   Quick,
	"battery:1[present]": Quick,

	// Slow priority - infrequently changing data
	"aux-battery[charge]":            Slow,
	"aux-battery[voltage]":           Slow,
	"aux-battery[charge-status]":     Slow,
	"cb-battery[charge]":             Slow,
	"cb-battery[cell-voltage]":       Slow,
	"cb-battery[current]":            Slow,
	"cb-battery[remaining-capacity]": Slow,
	"cb-battery[time-to-full]":       Slow,
	"cb-battery[time-to-empty]":      Slow,
	"ble[mac-address]":               Slow,
	"ble[status]":                    Slow,
}

// HashPriorities maps Redis hash names to their default priority
// Used when a field isn't explicitly mapped in FieldPriorities
var HashPriorities = map[string]Priority{
	"battery:0": Quick,
	"battery:1": Quick,
}

// NoisyFields contains fields that should be filtered out from change detection
// These fields change too frequently or are not useful for telemetry
var NoisyFields = map[string]bool{
	"gps[timestamp]":           true,
	"internet[signal-quality]": true,
	// GPS position jitters constantly while parked; treat it as noise for
	// change detection so it never triggers a flush. The current values are
	// still read fresh from Redis whenever some other field triggers one, so
	// snapshots keep an accurate position.
	"gps[latitude]":  true,
	"gps[longitude]": true,
	"gps[altitude]":  true,
	"gps[course]":    true,
}

// quantizationBuckets maps "hash[field]" to a bucket size used ONLY for change
// detection. Sensor values like battery voltage/current dither by a few least
// significant units every read; without bucketing each dither registers as a
// "change" and trips a flush. Rounding the value to its bucket before the
// change comparison suppresses sub-bucket noise.
//
// This never affects reported telemetry: payloads are read fresh from Redis at
// flush time, so the exact value is always what gets sent. Units (from the
// Redis field reference): voltages mV, currents mA, gps speed in whole units.
// Battery and ECU temperatures are already whole degrees C, so they need no
// bucketing.
// Main-pack voltage/current buckets are wider than the 50mV/100mA sensor
// resolution on purpose. Parked, the readings dither within a few mV / tens of
// mA (p99 step ~25mV / ~30mA on deep-blue), and at the tight buckets the value
// straddles boundaries and trips Quick flushes ~8/hr while stationary. 100mV /
// 250mA sit just past the dither (the trigger-rate knee: current 100->250mA cut
// 4.1 -> 0.7/hr), so sub-bucket noise stops triggering while a real sag/draw
// still crosses immediately.
var quantizationBuckets = map[string]int{
	"battery:0[voltage]":        100, // mV
	"battery:1[voltage]":        100, // mV
	"battery:0[current]":        250, // mA
	"battery:1[current]":        250, // mA
	"engine-ecu[motor:voltage]": 50,  // mV
	"engine-ecu[motor:current]": 100, // mA
	"aux-battery[voltage]":      50,  // mV
	"cb-battery[cell-voltage]":  50,  // mV
	"cb-battery[current]":       100, // mA
	"gps[speed]":                1,   // round parked GPS-speed noise down to 0
}

// QuantizeForChangeDetection rounds a noisy numeric field to its bucket so that
// sub-bucket dither does not register as a change. Non-numeric or unmapped
// fields are returned unchanged.
func QuantizeForChangeDetection(hash, field, value string) string {
	bucket, ok := quantizationBuckets[hash+"["+field+"]"]
	if !ok || bucket <= 0 || value == "" {
		return value
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return value
	}
	rounded := math.Round(f/float64(bucket)) * float64(bucket)
	return strconv.FormatInt(int64(rounded), 10)
}

// GetFieldPriority returns the priority for a given hash and field
func GetFieldPriority(hash, field string) Priority {
	fullKey := hash + "[" + field + "]"

	// Check if field is in noisy filter
	if NoisyFields[fullKey] {
		return -1 // Signal to skip this field
	}

	// Check exact field mapping first
	if priority, ok := FieldPriorities[fullKey]; ok {
		return priority
	}

	// Battery pack temperatures (temperature:0..N) are whole-degree sensor
	// readings that wiggle by ~1C constantly while parked. Without this they
	// fall through to the battery hash default (Quick) and a single sensor twitch
	// pulls a flush forward every time. Temperature is a slow-moving metric, so
	// class it Slow: it still rides along in every flush (read fresh from Redis),
	// it just stops *triggering* fast flushes.
	if strings.HasPrefix(hash, "battery:") && strings.HasPrefix(field, "temperature") {
		return Slow
	}

	// Fall back to hash-level priority
	if priority, ok := HashPriorities[hash]; ok {
		return priority
	}

	// Default to Medium priority
	return Medium
}

// GetPriorityDeadlines returns a map of priority levels to their deadlines
func GetPriorityDeadlines(config *models.Config) map[Priority]time.Duration {
	immediate, _ := time.ParseDuration(config.Telemetry.Priorities.Immediate)
	quick, _ := time.ParseDuration(config.Telemetry.Priorities.Quick)
	medium, _ := time.ParseDuration(config.Telemetry.Priorities.Medium)
	slow, _ := time.ParseDuration(config.Telemetry.Priorities.Slow)

	return map[Priority]time.Duration{
		Immediate: immediate,
		Quick:     quick,
		Medium:    medium,
		Slow:      slow,
	}
}
