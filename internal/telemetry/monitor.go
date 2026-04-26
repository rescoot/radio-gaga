package telemetry

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
	"radio-gaga/internal/redisbus"
)

var watchedHashes = []string{
	"vehicle",
	"battery:0",
	"battery:1",
	"aux-battery",
	"cb-battery",
	"engine-ecu",
	"gps",
	"internet",
	"modem",
	"power-manager",
	"power-mux",
	"ble",
	"dashboard",
	"system",
}

// TelemetryFlusher is the interface for sending telemetry
type TelemetryFlusher interface {
	FlushTelemetry() error
}

// Monitor watches Redis for field changes and triggers priority-based flushes
type Monitor struct {
	redisClient *redis.Client
	config      *models.Config

	mu sync.Mutex

	// Per-priority state
	priorityDeadlines map[Priority]time.Duration
	priorityPending   map[Priority]map[string]map[string]string // priority -> hash -> field -> value
	priorityTimers    map[Priority]*time.Timer

	// Track last values to detect actual changes
	lastValues map[string]string

	// Callback for flushing telemetry
	flusher TelemetryFlusher

	// Channels for signaling
	stopCh chan struct{}
}

// NewMonitor creates a new telemetry monitor
func NewMonitor(redisClient *redis.Client, config *models.Config) *Monitor {
	m := &Monitor{
		redisClient:       redisClient,
		config:            config,
		priorityDeadlines: GetPriorityDeadlines(config),
		priorityPending:   make(map[Priority]map[string]map[string]string),
		priorityTimers:    make(map[Priority]*time.Timer),
		lastValues:        make(map[string]string),
		stopCh:            make(chan struct{}),
	}
	for p := Immediate; p <= Slow; p++ {
		m.priorityPending[p] = make(map[string]map[string]string)
	}
	return m
}

// Register hooks the monitor into a redisbus to receive change notifications.
// Must be called before bus.Start.
func (m *Monitor) Register(bus *redisbus.Bus) {
	for _, hash := range watchedHashes {
		bus.OnHash(hash, func(_ context.Context, fields map[string]string) {
			for field, value := range fields {
				m.handleFieldChange(hash, field, value)
			}
		})
	}
}

// SetFlusher sets the callback for flushing telemetry
func (m *Monitor) SetFlusher(flusher TelemetryFlusher) {
	m.flusher = flusher
}

// Start blocks until ctx is cancelled or Stop is called, then drains pending
// timers. Field changes arrive via the redisbus, not from this goroutine.
func (m *Monitor) Start(ctx context.Context) {
	log.Println("[Monitor] Starting priority-based telemetry monitor...")
	select {
	case <-ctx.Done():
		log.Println("[Monitor] Context cancelled, stopping monitor")
	case <-m.stopCh:
		log.Println("[Monitor] Stop signal received, stopping monitor")
	}
	m.stopAllTimers()
}

// Stop stops the monitor
func (m *Monitor) Stop() {
	close(m.stopCh)
}

// handleFieldChange processes a single field change
func (m *Monitor) handleFieldChange(hash, field, value string) {
	fullKey := hash + "[" + field + "]"

	// Get priority for this field
	priority := GetFieldPriority(hash, field)
	if priority < 0 {
		// Field is filtered (noisy)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if value actually changed
	if m.lastValues[fullKey] == value {
		return
	}
	m.lastValues[fullKey] = value

	// Add to priority-specific pending map
	if m.priorityPending[priority][hash] == nil {
		m.priorityPending[priority][hash] = make(map[string]string)
	}
	m.priorityPending[priority][hash][field] = value

	// Start deadline timer if not already running (deadline semantics - no reset!)
	if m.priorityTimers[priority] == nil {
		deadline := m.priorityDeadlines[priority]
		m.priorityTimers[priority] = time.AfterFunc(deadline, func() {
			m.flushPriority(priority)
		})
	}
}

// flushPriority flushes all pending changes when a priority deadline fires
func (m *Monitor) flushPriority(triggeringPriority Priority) {
	m.mu.Lock()

	// Clear the timer reference for the triggering priority
	m.priorityTimers[triggeringPriority] = nil

	// Count total pending changes across all priorities
	var allChanges []string
	totalPending := 0

	for p := Immediate; p <= Slow; p++ {
		for hash, fields := range m.priorityPending[p] {
			for field := range fields {
				allChanges = append(allChanges, hash+"["+field+"]")
				totalPending++
			}
		}
	}

	if totalPending == 0 {
		m.mu.Unlock()
		return
	}

	// Clear all pending changes and timers
	for p := Immediate; p <= Slow; p++ {
		m.priorityPending[p] = make(map[string]map[string]string)
		if m.priorityTimers[p] != nil {
			m.priorityTimers[p].Stop()
			m.priorityTimers[p] = nil
		}
	}

	m.mu.Unlock()

	// Log the flush
	sort.Strings(allChanges)
	log.Printf("[Monitor] Flush (triggered by %s): %d changes: %s",
		PriorityNames[triggeringPriority], totalPending, strings.Join(allChanges, ", "))

	// Trigger telemetry flush
	if m.flusher != nil {
		if err := m.flusher.FlushTelemetry(); err != nil {
			log.Printf("[Monitor] Failed to flush telemetry: %v", err)
		}
	}
}

// FlushAllPending forces an immediate flush of all pending changes
func (m *Monitor) FlushAllPending() {
	m.mu.Lock()

	// Count total pending changes
	var allChanges []string
	totalPending := 0

	for p := Immediate; p <= Slow; p++ {
		for hash, fields := range m.priorityPending[p] {
			for field := range fields {
				allChanges = append(allChanges, hash+"["+field+"]")
				totalPending++
			}
		}
	}

	if totalPending == 0 {
		m.mu.Unlock()
		return
	}

	// Clear all pending changes and timers
	for p := Immediate; p <= Slow; p++ {
		m.priorityPending[p] = make(map[string]map[string]string)
		if m.priorityTimers[p] != nil {
			m.priorityTimers[p].Stop()
			m.priorityTimers[p] = nil
		}
	}

	m.mu.Unlock()

	sort.Strings(allChanges)
	log.Printf("[Monitor] Flush (manual): %d changes: %s", totalPending, strings.Join(allChanges, ", "))

	// Trigger telemetry flush
	if m.flusher != nil {
		if err := m.flusher.FlushTelemetry(); err != nil {
			log.Printf("[Monitor] Failed to flush telemetry: %v", err)
		}
	}
}

// InitializeBaseline sets the initial state from current Redis values
// This prevents a flood of change notifications on first connection
func (m *Monitor) InitializeBaseline(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, hash := range watchedHashes {
		fields, err := m.redisClient.HGetAll(ctx, hash).Result()
		if err != nil {
			log.Printf("[Monitor] Failed to read baseline for %s: %v", hash, err)
			continue
		}

		for field, value := range fields {
			fullKey := hash + "[" + field + "]"
			m.lastValues[fullKey] = value
		}
	}

	log.Printf("[Monitor] Initialized baseline with %d field values", len(m.lastValues))
}

// stopAllTimers stops all running priority timers
func (m *Monitor) stopAllTimers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for p := Immediate; p <= Slow; p++ {
		if m.priorityTimers[p] != nil {
			m.priorityTimers[p].Stop()
			m.priorityTimers[p] = nil
		}
	}
}

// HasPendingChanges returns true if there are any pending changes
func (m *Monitor) HasPendingChanges() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for p := Immediate; p <= Slow; p++ {
		if len(m.priorityPending[p]) > 0 {
			return true
		}
	}
	return false
}
