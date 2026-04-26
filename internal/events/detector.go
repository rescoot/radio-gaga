package events

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
	"radio-gaga/internal/notifications"
	"radio-gaga/internal/redisbus"
)

// EventPublisher is the interface for publishing events
type EventPublisher interface {
	PublishEvent(event Event) error
	IsConnected() bool
}

// TelemetryFlusher is the interface for triggering telemetry flush
type TelemetryFlusher interface {
	FlushAllPending()
}

// EventListener receives event notifications
type EventListener interface {
	Notify(event Event)
	ShouldNotify(eventType string) bool
	ChannelName() string
}

// Detector monitors Redis for conditions that trigger events
type Detector struct {
	redisClient   *redis.Client
	config        *models.Config
	publisher     EventPublisher
	buffer        *Buffer
	flusher       TelemetryFlusher
	ruleEvaluator *notifications.RuleEvaluator

	listeners []EventListener

	mu        sync.Mutex
	lastState map[string]string

	stopCh chan struct{}
}

// NewDetector creates a new event detector
func NewDetector(redisClient *redis.Client, config *models.Config) *Detector {
	return &Detector{
		redisClient:   redisClient,
		config:        config,
		buffer:        NewBuffer(config.Events.BufferPath, config.Events.MaxRetries),
		ruleEvaluator: notifications.NewRuleEvaluator(config.Notifications.Rules, redisClient),
		lastState:     make(map[string]string),
		stopCh:        make(chan struct{}),
	}
}

// SetPublisher sets the event publisher
func (d *Detector) SetPublisher(publisher EventPublisher) {
	d.publisher = publisher
}

// AddListener registers an event listener for notifications
func (d *Detector) AddListener(listener EventListener) {
	d.listeners = append(d.listeners, listener)
}

// SetTelemetryFlusher sets the telemetry flusher for coordination
func (d *Detector) SetTelemetryFlusher(flusher TelemetryFlusher) {
	d.flusher = flusher
}

// Register hooks the detector into a redisbus to receive change notifications.
// Must be called before bus.Start. No-op if events are disabled.
func (d *Detector) Register(bus *redisbus.Bus) {
	if d.config.Events.Enabled != nil && !*d.config.Events.Enabled {
		return
	}

	channels := []string{
		"alarm",
		"battery:0",
		"battery:1",
		"cb-battery",
		"power-manager",
		"internet",
		"vehicle",
		"gps",
		"engine-ecu",
	}
	existing := make(map[string]struct{}, len(channels))
	for _, ch := range channels {
		existing[ch] = struct{}{}
	}
	for _, src := range d.ruleEvaluator.Sources() {
		if _, ok := existing[src]; !ok {
			channels = append(channels, src)
			existing[src] = struct{}{}
		}
	}

	for _, ch := range channels {
		bus.OnHash(ch, func(ctx context.Context, fields map[string]string) {
			d.dispatchHash(ctx, ch, fields)
		})
		bus.OnPayload(ch, func(ctx context.Context, payload string) {
			for _, match := range d.ruleEvaluator.EvaluateMessage(ctx, ch, payload) {
				d.sendEvent(ruleMatchToEvent(match))
			}
		})
	}
}

// Start blocks until ctx is cancelled or Stop is called. Hash/payload events
// arrive via the redisbus; this goroutine just owns the fault-stream watcher
// and the lifecycle.
func (d *Detector) Start(ctx context.Context) {
	if d.config.Events.Enabled != nil && !*d.config.Events.Enabled {
		log.Println("[EventDetector] Events disabled in config, not starting")
		return
	}

	log.Println("[EventDetector] Starting event detection...")

	go d.watchFaultStream(ctx)

	select {
	case <-ctx.Done():
		log.Println("[EventDetector] Context cancelled, stopping")
	case <-d.stopCh:
		log.Println("[EventDetector] Stop signal received, stopping")
	}
}

// Stop stops the detector
func (d *Detector) Stop() {
	close(d.stopCh)
}

// dispatchHash checks for event-triggering conditions when a hash changes.
// Called from the redisbus with the already-fetched hash contents.
func (d *Detector) dispatchHash(ctx context.Context, hash string, fields map[string]string) {
	switch hash {
	case "alarm":
		d.checkAlarmEvents(ctx, fields)
	case "battery:0", "battery:1":
		d.checkBatteryEvents(hash, fields)
	case "cb-battery":
		d.checkCBBatteryEvents(fields)
	case "power-manager":
		d.checkPowerEvents(fields)
	case "internet":
		d.checkConnectivityEvents(fields)
	case "vehicle":
		d.checkVehicleEvents(fields)
	case "gps":
		d.checkGPSEvents(ctx, fields)
	case "engine-ecu":
		d.checkTemperatureEvents(hash, fields)
	}

	// Evaluate configurable notification rules for this hash
	for _, match := range d.ruleEvaluator.EvaluateHash(ctx, hash, fields) {
		d.sendEvent(ruleMatchToEvent(match))
	}
}

// ruleMatchToEvent converts a notifications.RuleMatch to an Event
func ruleMatchToEvent(match notifications.RuleMatch) Event {
	data := map[string]interface{}{
		"rule":       match.RuleName,
		"conditions": match.CondValues,
		"channels":   match.Channels,
	}
	if match.Message != "" {
		data["message"] = match.Message
	}
	return NewEvent(EventTypeNotificationRule, "", data)
}

// checkAlarmEvents checks for alarm status changes
func (d *Detector) checkAlarmEvents(ctx context.Context, _ map[string]string) {
	stateKey := "alarm:status"

	// The pub/sub payload is the field name, not the value; read from hash
	status, err := d.redisClient.HGet(ctx, "alarm", "status").Result()
	if err != nil {
		log.Printf("[EventDetector] Failed to read alarm status: %v", err)
		return
	}

	d.mu.Lock()
	lastStatus := d.lastState[stateKey]
	d.lastState[stateKey] = status
	d.mu.Unlock()

	if lastStatus != "" && lastStatus != status && status != "delay-armed" {
		d.sendEvent(NewEvent(EventTypeAlarm, status, nil))
	}
}

// checkBatteryEvents checks for battery-related events
func (d *Detector) checkBatteryEvents(battery string, fields map[string]string) {
	chargeKey := battery + ":charge"
	presentKey := battery + ":present"
	tempKey := battery + ":temperature"

	charge := parseInt(fields["charge"])
	present := fields["present"] == "true"
	temp := parseInt(fields["temperature"])

	d.mu.Lock()
	lastCharge := d.lastState[chargeKey]
	d.lastState[chargeKey] = fields["charge"]
	d.lastState[presentKey] = fields["present"]
	lastTemp := d.lastState[tempKey]
	d.lastState[tempKey] = fields["temperature"]
	d.mu.Unlock()

	// Battery critical event
	if present && charge <= BatteryWarningThreshold && lastCharge != fields["charge"] {
		d.sendEvent(NewEvent(EventTypeBatteryWarning, StatusTriggered, map[string]interface{}{
			"battery": battery,
			"charge":  charge,
		}))
	}

	// Battery temperature warning
	if temp >= BatteryTempWarningThreshold && lastTemp != fields["temperature"] {
		d.sendEvent(NewEvent(EventTypeTemperatureWarning, StatusTriggered, map[string]interface{}{
			"component":   battery,
			"temperature": temp,
		}))
	}
}

// checkCBBatteryEvents checks for CB battery events
func (d *Detector) checkCBBatteryEvents(fields map[string]string) {
	chargeKey := "cb-battery:charge"
	charge := parseInt(fields["charge"])

	d.mu.Lock()
	lastCharge := d.lastState[chargeKey]
	d.lastState[chargeKey] = fields["charge"]
	d.mu.Unlock()

	if charge <= BatteryWarningThreshold && lastCharge != fields["charge"] {
		d.sendEvent(NewEvent(EventTypeBatteryWarning, StatusTriggered, map[string]interface{}{
			"battery": "cb-battery",
			"charge":  charge,
		}))
	}
}

// checkPowerEvents checks for power state events
func (d *Detector) checkPowerEvents(fields map[string]string) {
	stateKey := "power-manager:state"
	nrfKey := "power-manager:nrf-reset-reason"

	state := fields["state"]
	nrfReason := fields["nrf-reset-reason"]

	d.mu.Lock()
	lastState := d.lastState[stateKey]
	d.lastState[stateKey] = state
	lastNrf := d.lastState[nrfKey]
	d.lastState[nrfKey] = nrfReason
	d.mu.Unlock()

	// Power state change
	if lastState != "" && lastState != state {
		d.sendEvent(NewEvent(EventTypeStateChange, "", map[string]interface{}{
			"from": lastState,
			"to":   state,
		}))
	}

	// NRF reset event
	if lastNrf != "" && lastNrf != nrfReason && nrfReason != "" {
		nrfCount := parseInt(fields["nrf-reset-count"])
		d.sendEvent(NewEvent(EventTypeFault, StatusTriggered, map[string]interface{}{
			"type":   "nrf_reset",
			"reason": fmt.Sprintf("0x%x", parseInt(nrfReason)),
			"count":  nrfCount,
		}))
	}
}

// checkConnectivityEvents checks for internet connectivity events
func (d *Detector) checkConnectivityEvents(fields map[string]string) {
	statusKey := "internet:status"
	status := fields["status"]

	d.mu.Lock()
	lastStatus := d.lastState[statusKey]
	d.lastState[statusKey] = status
	d.mu.Unlock()

	if lastStatus != "" && lastStatus != status {
		eventStatus := StatusLost
		if status == "connected" {
			eventStatus = StatusRegained
		}

		d.sendEvent(NewEvent(EventTypeConnectivity, eventStatus, map[string]interface{}{
			"status": status,
		}))
	}
}

// checkVehicleEvents checks for vehicle state and lock changes
func (d *Detector) checkVehicleEvents(fields map[string]string) {
	// Vehicle state changes
	stateKey := "vehicle:state"
	// Apply the hop-on-active override so the cloud-facing state change
	// event matches what telemetry reports: "parked" is externally
	// represented as "stand-by" while hop-on is engaged.
	state := fields["state"]
	if fields["hop-on-active"] == "true" {
		state = "stand-by"
	}

	d.mu.Lock()
	lastState := d.lastState[stateKey]
	d.lastState[stateKey] = state
	d.mu.Unlock()

	if lastState != "" && lastState != state {
		d.sendEvent(NewEvent(EventTypeStateChange, "", map[string]interface{}{
			"from": lastState,
			"to":   state,
		}))
	}

	// Lock changes
	handlebarKey := "vehicle:handlebar"
	seatboxKey := "vehicle:seatbox"

	handlebar := fields["handlebar:lock-sensor"]
	seatbox := fields["seatbox:lock"]

	d.mu.Lock()
	lastHandlebar := d.lastState[handlebarKey]
	d.lastState[handlebarKey] = handlebar
	lastSeatbox := d.lastState[seatboxKey]
	d.lastState[seatboxKey] = seatbox
	d.mu.Unlock()

	if lastHandlebar != "" && lastHandlebar != handlebar {
		d.sendEvent(NewEvent(EventTypeHardwareChange, "", map[string]interface{}{
			"lock":  "handlebar",
			"state": handlebar,
		}))
	}

	if lastSeatbox != "" && lastSeatbox != seatbox {
		d.sendEvent(NewEvent(EventTypeHardwareChange, "", map[string]interface{}{
			"lock":  "seatbox",
			"state": seatbox,
		}))
	}
}

// checkGPSEvents checks for GPS fix events and unauthorized movement
func (d *Detector) checkGPSEvents(ctx context.Context, fields map[string]string) {
	stateKey := "gps:state"
	state := fields["state"]

	d.mu.Lock()
	lastState := d.lastState[stateKey]
	d.lastState[stateKey] = state
	d.mu.Unlock()

	if lastState != "" && lastState != state {
		eventStatus := StatusLost
		if state == "fix-3d" || state == "fix-2d" {
			eventStatus = StatusRegained
		}

		d.sendEvent(NewEvent(EventTypeGPSEvent, eventStatus, map[string]interface{}{
			"state": state,
		}))
	}

	// Detect unauthorized movement: GPS speed > 0 while vehicle is locked
	gpsSpeed := parseInt(fields["speed"])
	if gpsSpeed > 0 {
		vehicleState, err := d.redisClient.HGet(ctx, "vehicle", "state").Result()
		if err == nil && (vehicleState == "standby" || vehicleState == "hibernating" || vehicleState == "locked") {
			movementKey := "unauthorized_movement:active"
			d.mu.Lock()
			lastMovement := d.lastState[movementKey]
			d.lastState[movementKey] = "true"
			d.mu.Unlock()

			if lastMovement != "true" {
				d.sendEvent(NewEvent(EventTypeUnauthorizedMovement, StatusTriggered, map[string]interface{}{
					"gps_speed":     gpsSpeed,
					"vehicle_state": vehicleState,
				}))
			}
		}
	} else {
		d.mu.Lock()
		d.lastState["unauthorized_movement:active"] = ""
		d.mu.Unlock()
	}
}

// checkTemperatureEvents checks for temperature warning events
func (d *Detector) checkTemperatureEvents(component string, fields map[string]string) {
	tempKey := component + ":temperature"
	temp := parseInt(fields["temperature"])

	d.mu.Lock()
	lastTemp := d.lastState[tempKey]
	d.lastState[tempKey] = fields["temperature"]
	d.mu.Unlock()

	if temp >= TemperatureWarningThreshold && lastTemp != fields["temperature"] {
		d.sendEvent(NewEvent(EventTypeTemperatureWarning, StatusTriggered, map[string]interface{}{
			"component":   component,
			"temperature": temp,
		}))
	}
}

// watchFaultStream monitors the events:faults Redis stream for fault events
func (d *Detector) watchFaultStream(ctx context.Context) {
	lastID := "$" // Start from new messages

	// Check if stream exists first
	exists, err := d.redisClient.Exists(ctx, "events:faults").Result()
	if err != nil || exists == 0 {
		log.Println("[EventDetector] Fault stream does not exist, skipping fault monitoring")
		return
	}

	log.Println("[EventDetector] Monitoring fault stream...")

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		default:
		}

		// Read from stream with blocking - use short timeout to avoid connection issues
		streams, err := d.redisClient.XRead(ctx, &redis.XReadArgs{
			Streams: []string{"events:faults", lastID},
			Count:   10,
			Block:   1000, // 1 second timeout
		}).Result()

		if err != nil {
			// redis.Nil means timeout with no messages - expected
			if err == redis.Nil {
				continue
			}
			// Log other errors but don't spam - check every 30 seconds
			select {
			case <-ctx.Done():
				return
			case <-d.stopCh:
				return
			default:
				// Silently continue on errors - stream reading is best-effort
				continue
			}
		}

		for _, stream := range streams {
			for _, msg := range stream.Messages {
				lastID = msg.ID
				d.handleFaultMessage(msg.Values)
			}
		}
	}
}

// handleFaultMessage processes a fault message from the stream
func (d *Detector) handleFaultMessage(values map[string]interface{}) {
	group, hasGroup := values["group"].(string)
	codeStr, hasCode := values["code"].(string)

	if !hasGroup || !hasCode {
		return
	}

	data := map[string]interface{}{
		"group": group,
		"code":  parseInt(codeStr),
	}

	if desc, ok := values["description"].(string); ok {
		data["description"] = desc
	}

	d.sendEvent(NewEvent(EventTypeFault, StatusTriggered, data))
}

// sendEvent sends an event, buffering if not connected
func (d *Detector) sendEvent(event Event) {
	log.Printf("[EventDetector] Event: %s %s %v", event.EventType, event.Status, event.Data)

	// Notify listeners.
	// If the event has an explicit "channels" list (rule-generated events), only notify
	// listeners whose ChannelName() is in that list. Otherwise use ShouldNotify (built-in events).
	var targetChannels map[string]struct{}
	if event.Data != nil {
		if chRaw, ok := event.Data["channels"]; ok {
			if chList, ok := chRaw.([]string); ok && len(chList) > 0 {
				targetChannels = make(map[string]struct{}, len(chList))
				for _, ch := range chList {
					targetChannels[ch] = struct{}{}
				}
			}
		}
	}

	for _, l := range d.listeners {
		if targetChannels != nil {
			if _, ok := targetChannels[l.ChannelName()]; ok {
				l.Notify(event)
			}
		} else if l.ShouldNotify(event.EventType) {
			l.Notify(event)
		}
	}

	if d.publisher != nil && d.publisher.IsConnected() {
		if err := d.publisher.PublishEvent(event); err != nil {
			log.Printf("[EventDetector] Failed to publish event, buffering: %v", err)
			d.buffer.Add(event)
		} else {
			go d.FlushBufferedEvents(context.Background())
			if d.flusher != nil {
				go d.flusher.FlushAllPending()
			}
		}
	} else {
		log.Println("[EventDetector] Not connected, buffering event")
		d.buffer.Add(event)
	}
}

// FlushBufferedEvents attempts to send all buffered events
func (d *Detector) FlushBufferedEvents(ctx context.Context) {
	if d.publisher == nil || !d.publisher.IsConnected() {
		return
	}

	d.buffer.Flush(func(event Event) error {
		return d.publisher.PublishEvent(event)
	})
}

// InitializeBaseline sets the initial state from current Redis values
func (d *Detector) InitializeBaseline(ctx context.Context) {
	hashes := map[string][]string{
		"alarm":         {"status"},
		"battery:0":     {"charge", "present", "temperature"},
		"battery:1":     {"charge", "present", "temperature"},
		"cb-battery":    {"charge"},
		"power-manager": {"state", "nrf-reset-reason"},
		"internet":      {"status"},
		"vehicle":       {"state", "handlebar:lock-sensor", "seatbox:lock"},
		"gps":           {"state"},
		"engine-ecu":    {"temperature"},
	}

	// Also include fields from notification rules
	for _, rule := range d.config.Notifications.Rules {
		for _, cond := range rule.Conditions {
			if cond.Field != "" {
				hashes[cond.Source] = append(hashes[cond.Source], cond.Field)
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for hash, fields := range hashes {
		// Deduplicate fields
		seen := make(map[string]struct{})
		for _, field := range fields {
			if _, ok := seen[field]; ok {
				continue
			}
			seen[field] = struct{}{}
			value, err := d.redisClient.HGet(ctx, hash, field).Result()
			if err != nil && err != redis.Nil {
				continue
			}
			stateKey := hash + ":" + field
			d.lastState[stateKey] = value
		}
	}

	log.Printf("[EventDetector] Initialized baseline with %d field values", len(d.lastState))
}

func parseInt(s string) int {
	val, _ := strconv.Atoi(s)
	return val
}
