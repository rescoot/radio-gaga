package telemetry

import (
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
	"vehicle[state]":              Immediate,
	"vehicle[seatbox:lock]":       Immediate,
	"vehicle[handlebar:lock-sensor]": Immediate,
	"vehicle[blinker:state]":      Immediate,
	"power-manager[state]":        Immediate,

	// Quick priority - frequently changing important data
	"gps[latitude]":      Quick,
	"gps[longitude]":     Quick,
	"gps[altitude]":      Quick,
	"gps[speed]":         Quick,
	"gps[course]":        Quick,
	"gps[state]":         Quick,
	"battery:0[charge]":  Quick,
	"battery:0[state]":   Quick,
	"battery:0[present]": Quick,
	"battery:1[charge]":  Quick,
	"battery:1[state]":   Quick,
	"battery:1[present]": Quick,

	// Slow priority - infrequently changing data
	"aux-battery[charge]":           Slow,
	"aux-battery[voltage]":          Slow,
	"aux-battery[charge-status]":    Slow,
	"cb-battery[charge]":            Slow,
	"cb-battery[cell-voltage]":      Slow,
	"cb-battery[current]":           Slow,
	"cb-battery[remaining-capacity]": Slow,
	"cb-battery[time-to-full]":      Slow,
	"cb-battery[time-to-empty]":     Slow,
	"ble[mac-address]":              Slow,
	"ble[status]":                   Slow,
}

// HashPriorities maps Redis hash names to their default priority
// Used when a field isn't explicitly mapped in FieldPriorities
var HashPriorities = map[string]Priority{
	"gps":       Quick,
	"battery:0": Quick,
	"battery:1": Quick,
}

// NoisyFields contains fields that should be filtered out from change detection
// These fields change too frequently or are not useful for telemetry
var NoisyFields = map[string]bool{
	"gps[timestamp]":          true,
	"internet[signal-quality]": true,
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
