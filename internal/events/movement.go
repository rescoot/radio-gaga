package events

import (
	"context"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Vehicle and power states for unauthorized movement detection.
//
// vehicle:state values originate from vehicle-service. Detection only arms in
// fully unattended states. Transitional states (shutting-down, waiting-*,
// updating) leave the user physically next to the scooter, so they are
// excluded.
//
// power-manager:state values are checked separately. While hibernating /
// suspending / booting, the GPS receiver may be cold-starting and produce
// large position jumps that have nothing to do with motion.
var unattendedVehicleStates = map[string]struct{}{
	"stand-by": {},
	"locked":   {},
}

var transitionalVehicleStates = map[string]struct{}{
	"parked":                       {},
	"ready-to-drive":               {},
	"shutting-down":                {},
	"waiting-seatbox":              {},
	"waiting-hibernation":          {},
	"waiting-hibernation-advanced": {},
	"waiting-hibernation-seatbox":  {},
	"waiting-hibernation-confirm":  {},
	"updating":                     {},
}

var hibernatingPowerStates = map[string]struct{}{
	"hibernating":                {},
	"hibernating-imminent":       {},
	"hibernating-manual":         {},
	"hibernating-manual-imminent": {},
	"hibernating-timer":          {},
	"hibernating-timer-imminent": {},
	"suspending":                 {},
	"suspending-imminent":        {},
	"booting":                    {},
}

const (
	// Minimum displacement from the anchor before we even consider
	// confirmation. Below this is well within typical urban GPS jitter.
	movementAnchorThreshold = 20.0 // metres

	// Sampling cadence and window for the local confirmation pass.
	// 12 samples × 5s = 60s observation, comfortably faster than the
	// stand-by telemetry cadence (5min) so cloud only sees the verdict.
	confirmationSampleInterval = 5 * time.Second
	confirmationSampleCount    = 12

	// Minimum number of samples that must pass the consistency check for
	// the displacement to be considered confirmed.
	confirmationMinConsistent = 6

	// Minimum sustained displacement (final smoothed position vs anchor)
	// for a confirmed event. Higher than the trigger threshold to avoid
	// drifting into and out of the gate.
	confirmedDisplacementThreshold = 30.0 // metres

	// Per-sample inter-segment movement threshold for "consistent motion"
	// (the smoothed delta between consecutive samples).
	consistentSegmentThreshold = 3.0 // metres

	// EMA smoothing factor for the local confirmation positions.
	emaAlpha = 0.3

	// After a confirmed/rejected verdict, suppress re-arming for this long
	// to avoid flapping at the boundary. State changes always reset.
	rearmCooldown = 2 * time.Minute

	// Minimum time the vehicle must have been in its current state before
	// we arm. Mirrors the cloud-side stability requirement.
	stateStabilityRequirement = 30 * time.Second

	// Implausible jump ceiling. Single readings beyond this from the
	// anchor are dropped as cold-start glitches.
	implausibleJumpMetres = 5000.0

	// Required GPS state for trustworthy fixes.
	requiredGPSState = "fix-established"
)

type gpsSample struct {
	lat       float64
	lng       float64
	at        time.Time
}

// MovementMonitor performs on-device unauthorized-movement detection with
// local multi-sample confirmation before publishing an event.
//
// Cloud receives one of:
//   - status=confirmed: high-confidence event, sunshine alerts immediately.
//   - status=rejected:  debug-only, sunshine logs but takes no action.
//   - status=triggered: legacy/single-shot signal (only used as a fallback
//     when local sampling cannot run, e.g. GPS lost mid-confirmation).
type MovementMonitor struct {
	redisClient *redis.Client
	publisher   EventPublisher

	mu              sync.Mutex
	anchor          *gpsSample
	anchorState     string
	stateEnteredAt  time.Time
	confirming      bool
	cooldownUntil   time.Time
}

// NewMovementMonitor wires a monitor to redis and the event publisher.
func NewMovementMonitor(redisClient *redis.Client) *MovementMonitor {
	return &MovementMonitor{redisClient: redisClient}
}

// SetPublisher is called by Detector after construction.
func (m *MovementMonitor) SetPublisher(p EventPublisher) {
	m.publisher = p
}

// OnVehicleStateChange resets the anchor whenever the vehicle state changes.
// Called by the detector's vehicle handler.
func (m *MovementMonitor) OnVehicleStateChange(newState string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.anchor = nil
	m.anchorState = newState
	m.stateEnteredAt = time.Now()
	m.cooldownUntil = time.Time{}
}

// OnGPSUpdate is called whenever the gps hash changes. It is the entry point
// for trigger evaluation and (asynchronously) for confirmation.
func (m *MovementMonitor) OnGPSUpdate(ctx context.Context, fields map[string]string) {
	if fields["state"] != requiredGPSState {
		return
	}
	lat, lng, ok := parseLatLng(fields)
	if !ok {
		return
	}

	vehicleState, _ := m.redisClient.HGet(ctx, "vehicle", "state").Result()
	powerState, _ := m.redisClient.HGet(ctx, "power-manager", "state").Result()

	if !m.detectionArmed(vehicleState, powerState) {
		return
	}

	m.mu.Lock()
	if !m.cooldownUntil.IsZero() && time.Now().Before(m.cooldownUntil) {
		m.mu.Unlock()
		return
	}
	if m.confirming {
		m.mu.Unlock()
		return
	}

	if m.anchor == nil || m.anchorState != vehicleState {
		m.anchor = &gpsSample{lat: lat, lng: lng, at: time.Now()}
		m.anchorState = vehicleState
		if m.stateEnteredAt.IsZero() {
			m.stateEnteredAt = time.Now()
		}
		m.mu.Unlock()
		return
	}

	if time.Since(m.stateEnteredAt) < stateStabilityRequirement {
		m.mu.Unlock()
		return
	}

	dist := haversineMetres(m.anchor.lat, m.anchor.lng, lat, lng)
	if dist < movementAnchorThreshold {
		m.mu.Unlock()
		return
	}
	if dist > implausibleJumpMetres {
		// Cold-start teleport. Drop and re-anchor.
		m.anchor = &gpsSample{lat: lat, lng: lng, at: time.Now()}
		m.mu.Unlock()
		return
	}

	m.confirming = true
	anchor := *m.anchor
	m.mu.Unlock()

	log.Printf("[MovementMonitor] Trigger: %.1fm from anchor in state=%q, starting local confirmation", dist, vehicleState)
	go m.runConfirmation(ctx, anchor, vehicleState, powerState)
}

// runConfirmation polls GPS at fast cadence and decides confirmed/rejected.
func (m *MovementMonitor) runConfirmation(ctx context.Context, anchor gpsSample, vehicleStateAtTrigger, powerStateAtTrigger string) {
	defer func() {
		m.mu.Lock()
		m.confirming = false
		m.cooldownUntil = time.Now().Add(rearmCooldown)
		m.mu.Unlock()
	}()

	samples := make([]gpsSample, 0, confirmationSampleCount)
	ticker := time.NewTicker(confirmationSampleInterval)
	defer ticker.Stop()

	collect := func() bool {
		fields, err := m.redisClient.HGetAll(ctx, "gps").Result()
		if err != nil {
			return false
		}
		if fields["state"] != requiredGPSState {
			return false
		}
		lat, lng, ok := parseLatLng(fields)
		if !ok {
			return false
		}
		if d := haversineMetres(anchor.lat, anchor.lng, lat, lng); d > implausibleJumpMetres {
			return false
		}
		samples = append(samples, gpsSample{lat: lat, lng: lng, at: time.Now()})

		// Bail if state changes mid-confirmation: user touched the scooter,
		// not unauthorized.
		if vs, _ := m.redisClient.HGet(ctx, "vehicle", "state").Result(); vs != vehicleStateAtTrigger {
			return false
		}
		return true
	}

	if !collect() {
		m.publishVerdict(StatusRejected, "lost_fix_or_state_change", anchor, samples, vehicleStateAtTrigger, powerStateAtTrigger)
		return
	}

	for i := 1; i < confirmationSampleCount; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !collect() {
				m.publishVerdict(StatusRejected, "lost_fix_or_state_change", anchor, samples, vehicleStateAtTrigger, powerStateAtTrigger)
				return
			}
		}
	}

	smoothed := emaSmooth(samples)
	consistent := consistentSegments(smoothed)
	finalDisp := haversineMetres(anchor.lat, anchor.lng, smoothed[len(smoothed)-1].lat, smoothed[len(smoothed)-1].lng)

	if consistent >= confirmationMinConsistent && finalDisp >= confirmedDisplacementThreshold {
		m.publishVerdict(StatusConfirmed, "", anchor, samples, vehicleStateAtTrigger, powerStateAtTrigger)
		// Re-anchor at the new location so the next confirmed displacement
		// is measured from where the scooter is now.
		m.mu.Lock()
		m.anchor = &gpsSample{lat: smoothed[len(smoothed)-1].lat, lng: smoothed[len(smoothed)-1].lng, at: time.Now()}
		m.mu.Unlock()
		return
	}

	reason := "below_threshold"
	if consistent < confirmationMinConsistent {
		reason = "inconsistent_movement"
	}
	m.publishVerdict(StatusRejected, reason, anchor, samples, vehicleStateAtTrigger, powerStateAtTrigger)
}

func (m *MovementMonitor) publishVerdict(status, rejectionReason string, anchor gpsSample, samples []gpsSample, vehicleState, powerState string) {
	if m.publisher == nil {
		return
	}
	smoothed := emaSmooth(samples)
	var finalDisp float64
	var lastLat, lastLng float64
	if len(smoothed) > 0 {
		last := smoothed[len(smoothed)-1]
		lastLat, lastLng = last.lat, last.lng
		finalDisp = haversineMetres(anchor.lat, anchor.lng, last.lat, last.lng)
	}

	data := map[string]interface{}{
		"vehicle_state":   vehicleState,
		"power_state":     powerState,
		"anchor":          map[string]float64{"lat": anchor.lat, "lng": anchor.lng},
		"current":         map[string]float64{"lat": lastLat, "lng": lastLng},
		"distance_meters": int(finalDisp + 0.5),
		"sample_count":    len(samples),
		"window_seconds":  int(confirmationSampleInterval.Seconds()) * confirmationSampleCount,
	}
	if rejectionReason != "" {
		data["rejection_reason"] = rejectionReason
	}

	log.Printf("[MovementMonitor] Verdict: %s (%s) disp=%.1fm samples=%d", status, rejectionReason, finalDisp, len(samples))
	if err := m.publisher.PublishEvent(NewEvent(EventTypeUnauthorizedMovement, status, data)); err != nil {
		log.Printf("[MovementMonitor] Publish failed: %v", err)
	}
}

// detectionArmed returns true when the current state combination permits
// unauthorized-movement detection.
func (m *MovementMonitor) detectionArmed(vehicleState, powerState string) bool {
	if _, transitional := transitionalVehicleStates[vehicleState]; transitional {
		return false
	}
	if _, ok := unattendedVehicleStates[vehicleState]; !ok {
		return false
	}
	if _, hibernating := hibernatingPowerStates[powerState]; hibernating {
		return false
	}
	return true
}

func parseLatLng(fields map[string]string) (float64, float64, bool) {
	latStr, lngStr := fields["latitude"], fields["longitude"]
	if latStr == "" || lngStr == "" {
		return 0, 0, false
	}
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return 0, 0, false
	}
	lng, err := strconv.ParseFloat(lngStr, 64)
	if err != nil {
		return 0, 0, false
	}
	if lat == 0 && lng == 0 {
		return 0, 0, false
	}
	return lat, lng, true
}

func haversineMetres(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMetres = 6371000.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMetres * c
}

func emaSmooth(samples []gpsSample) []gpsSample {
	if len(samples) == 0 {
		return nil
	}
	out := make([]gpsSample, len(samples))
	out[0] = samples[0]
	for i := 1; i < len(samples); i++ {
		out[i] = gpsSample{
			lat: emaAlpha*samples[i].lat + (1-emaAlpha)*out[i-1].lat,
			lng: emaAlpha*samples[i].lng + (1-emaAlpha)*out[i-1].lng,
			at:  samples[i].at,
		}
	}
	return out
}

func consistentSegments(smoothed []gpsSample) int {
	if len(smoothed) < 2 {
		return 0
	}
	count := 0
	for i := 0; i < len(smoothed)-1; i++ {
		if haversineMetres(smoothed[i].lat, smoothed[i].lng, smoothed[i+1].lat, smoothed[i+1].lng) >= consistentSegmentThreshold {
			count++
		}
	}
	return count
}
