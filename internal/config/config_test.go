package config

import (
	"testing"

	"radio-gaga/internal/models"
)

func TestValidatePriorityOrdering_ValidOrder(t *testing.T) {
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

	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected no error for valid priority ordering, got: %v", err)
	}
}

func TestValidatePriorityOrdering_EqualDurations(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "1s",
				Medium:    "1s",
				Slow:      "1s",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected no error for equal durations, got: %v", err)
	}
}

func TestValidatePriorityOrdering_ImmediateGreaterThanQuick(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "10s",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when immediate > quick, got nil")
	}
}

func TestValidatePriorityOrdering_QuickGreaterThanMedium(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "2m",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when quick > medium, got nil")
	}
}

func TestValidatePriorityOrdering_MediumGreaterThanSlow(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "1s",
				Quick:     "5s",
				Medium:    "1h",
				Slow:      "15m",
			},
		},
	}

	err := validatePriorityOrdering(config)
	if err == nil {
		t.Error("Expected error when medium > slow, got nil")
	}
}

func TestValidatePriorityOrdering_InvalidDuration(t *testing.T) {
	config := &models.Config{
		Telemetry: models.TelemetryConfig{
			Priorities: models.PriorityConfig{
				Immediate: "invalid",
				Quick:     "5s",
				Medium:    "1m",
				Slow:      "15m",
			},
		},
	}

	// When durations are invalid, we return nil and let the general duration validation catch it
	err := validatePriorityOrdering(config)
	if err != nil {
		t.Errorf("Expected nil for invalid duration (handled elsewhere), got: %v", err)
	}
}
