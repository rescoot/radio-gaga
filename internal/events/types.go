package events

import "time"

// Event represents an event message to be sent to the cloud
type Event struct {
	EventType string                 `json:"event_type"`
	Status    string                 `json:"status,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp string                 `json:"timestamp"`
}

// BufferedEvent wraps an Event with retry metadata for persistence
type BufferedEvent struct {
	Event   Event `json:"event"`
	Retries int   `json:"retries"`
}

// NewEvent creates a new event with the current timestamp
func NewEvent(eventType string, status string, data map[string]interface{}) Event {
	return Event{
		EventType: eventType,
		Status:    status,
		Data:      data,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// Event type constants matching sunshine's ScooterEvent types
const (
	// Existing sunshine event types
	EventTypeConnect             = "connect"
	EventTypeDisconnect          = "disconnect"
	EventTypeStateChange         = "state_change"
	EventTypeCommand             = "command"
	EventTypeTripStart           = "trip_start"
	EventTypeTripEnd             = "trip_end"
	EventTypeUnauthorizedMovement = "unauthorized_movement"
	EventTypeAlarm               = "alarm"
	EventTypeHardwareChange      = "hardware_change"
	EventTypeBatteryWarning      = "battery_warning"
	EventTypeTemperatureWarning  = "temperature_warning"
	EventTypeGPSEvent            = "gps_event"

	// New event types (to be added to sunshine)
	EventTypeConnectivity = "connectivity" // Scooter internet connectivity
	EventTypeFault        = "fault"        // Hardware/software faults
)

// Status constants
const (
	StatusTriggered = "triggered"
	StatusCleared   = "cleared"
	StatusLost      = "lost"
	StatusRegained  = "regained"
)

// Hardcoded thresholds for event detection
const (
	BatteryWarningThreshold     = 10 // Battery charge percentage
	TemperatureWarningThreshold = 80 // Engine temperature in Celsius
	BatteryTempWarningThreshold = 60 // Battery temperature in Celsius
)
