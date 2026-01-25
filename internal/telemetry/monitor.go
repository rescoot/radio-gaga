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
)

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
	return &Monitor{
		redisClient:       redisClient,
		config:            config,
		priorityDeadlines: GetPriorityDeadlines(config),
		priorityPending:   make(map[Priority]map[string]map[string]string),
		priorityTimers:    make(map[Priority]*time.Timer),
		lastValues:        make(map[string]string),
		stopCh:            make(chan struct{}),
	}
}

// SetFlusher sets the callback for flushing telemetry
func (m *Monitor) SetFlusher(flusher TelemetryFlusher) {
	m.flusher = flusher
}

// Start begins monitoring Redis for changes
func (m *Monitor) Start(ctx context.Context) {
	log.Println("[Monitor] Starting priority-based telemetry monitor...")

	// Initialize priority pending maps
	for p := Immediate; p <= Slow; p++ {
		m.priorityPending[p] = make(map[string]map[string]string)
	}

	// Subscribe to Redis keyspace notifications for hash changes
	// We monitor the hashes that contain telemetry data
	hashesToWatch := []string{
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

	// Create pub/sub subscriber
	pubsub := m.redisClient.Subscribe(ctx, hashesToWatch...)
	defer pubsub.Close()

	log.Printf("[Monitor] Subscribed to %d Redis channels for change notifications", len(hashesToWatch))

	// Message processing loop
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			log.Println("[Monitor] Context cancelled, stopping monitor")
			m.stopAllTimers()
			return
		case <-m.stopCh:
			log.Println("[Monitor] Stop signal received, stopping monitor")
			m.stopAllTimers()
			return
		case msg := <-ch:
			if msg == nil {
				continue
			}
			// Handle pub/sub message - the channel name is the hash that changed
			m.handleHashChange(ctx, msg.Channel, msg.Payload)
		}
	}
}

// Stop stops the monitor
func (m *Monitor) Stop() {
	close(m.stopCh)
}

// handleHashChange processes a change notification for a hash
func (m *Monitor) handleHashChange(ctx context.Context, hash, payload string) {
	// The payload from Redis pub/sub on a hash channel is typically the field that changed
	// But we need to re-read the entire hash to get the current values
	// For simplicity, we'll read all fields and compare with last known values

	fields, err := m.redisClient.HGetAll(ctx, hash).Result()
	if err != nil {
		log.Printf("[Monitor] Failed to read hash %s: %v", hash, err)
		return
	}

	for field, value := range fields {
		m.handleFieldChange(hash, field, value)
	}
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
	hashes := []string{
		"vehicle", "battery:0", "battery:1", "aux-battery", "cb-battery",
		"engine-ecu", "gps", "internet", "modem", "power-manager",
		"power-mux", "ble", "dashboard", "system",
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, hash := range hashes {
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
