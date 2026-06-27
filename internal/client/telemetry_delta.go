package client

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"reflect"
	"strings"
	"sync/atomic"

	"radio-gaga/internal/models"
)

// fullResyncEvery bounds how many deltas we send before forcing a fresh full
// snapshot, so a missed message can never leave the server permanently stale.
const fullResyncEvery = 20

// telemetryToMap marshals a TelemetryData to the same v2 JSON shape the server
// expects, then back into a generic map so we can diff it leaf by leaf.
func telemetryToMap(current *models.TelemetryData) (map[string]any, error) {
	b, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// computeDelta returns the nested subset of curr whose leaves differ from prev,
// plus a list of dotted-path keys present in prev but gone from curr (so the
// server can null them; deep_merge cannot delete a key on its own).
func computeDelta(prev, curr map[string]any) (delta map[string]any, cleared []string) {
	delta = map[string]any{}
	diffMap("", prev, curr, delta, &cleared)
	return delta, cleared
}

func diffMap(prefix string, prev, curr, delta map[string]any, cleared *[]string) {
	for k, cv := range curr {
		pv, ok := prev[k]
		if !ok {
			delta[k] = cv
			continue
		}
		cm, cIsMap := cv.(map[string]any)
		pm, pIsMap := pv.(map[string]any)
		if cIsMap && pIsMap {
			sub := map[string]any{}
			diffMap(joinKey(prefix, k), pm, cm, sub, cleared)
			if len(sub) > 0 {
				delta[k] = sub
			}
		} else if !reflect.DeepEqual(pv, cv) {
			delta[k] = cv
		}
	}
	for k := range prev {
		if _, ok := curr[k]; !ok {
			*cleared = append(*cleared, joinKey(prefix, k))
		}
	}
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// parkedGPSSmoothingMetres is the displacement (from the last REPORTED position)
// below which a new parked GPS fix is treated as jitter and suppressed. Deep
// Blue's parked fixes step <5m between samples (p99 ~2.7m, max ~4.9m) while
// slow-walking ~16m over time, so 5m drops every observed jitter step while
// still re-reporting a genuine reposition.
const parkedGPSSmoothingMetres = 5.0

// smoothParkedGPS holds the last reported lat/lng while the scooter is not
// driving, only letting the reported position move once a fix lands >= the
// smoothing threshold away, so sub-threshold GPS noise never enters a delta.
// Driving always reports the raw fix (live tracking). It mutates td.GPS to the
// reported position and returns the new last-reported state to persist.
func smoothParkedGPS(td *models.TelemetryData, lastLat, lastLng float64, lastValid, driving bool) (repLat, repLng float64, valid bool) {
	cur := td.GPS
	curValid := gpsFixValid(cur.Lat, cur.Lng)

	if driving || !curValid {
		return cur.Lat, cur.Lng, curValid
	}
	if lastValid && haversineMetres(lastLat, lastLng, cur.Lat, cur.Lng) < parkedGPSSmoothingMetres {
		td.GPS.Lat = lastLat
		td.GPS.Lng = lastLng
		return lastLat, lastLng, true
	}
	return cur.Lat, cur.Lng, true
}

// gpsFixValid rejects the null island / unset fix.
func gpsFixValid(lat, lng float64) bool {
	return lat != 0 || lng != 0
}

// haversineMetres is the great-circle distance between two lat/lng points.
func haversineMetres(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0
	rad := math.Pi / 180
	dlat := (lat2 - lat1) * rad
	dlng := (lng2 - lng1) * rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlng/2)*math.Sin(dlng/2)
	return earthRadiusM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// publishTelemetrySmart sends a full snapshot or a delta depending on what has
// changed since the last send. It serialises sends under telMu so lastSentMap
// stays consistent across the ticker, monitor-flush and state-change callers.
func (s *ScooterMQTTClient) publishTelemetrySmart(current *models.TelemetryData, forceFull bool) error {
	state := current.VehicleState.State

	s.telMu.Lock()
	defer s.telMu.Unlock()

	// Smooth parked GPS: hold the last reported position until a fix lands
	// >= the threshold away, so jitter does not ride into deltas. Mutates
	// current.GPS before it is serialised for diffing.
	s.lastGPSLat, s.lastGPSLng, s.lastGPSValid =
		smoothParkedGPS(current, s.lastGPSLat, s.lastGPSLng, s.lastGPSValid, state == "ready-to-drive")

	curr, err := telemetryToMap(current)
	if err != nil {
		return fmt.Errorf("failed to map telemetry: %v", err)
	}

	full := forceFull ||
		s.lastSentMap == nil ||
		s.flushesSinceFull >= fullResyncEvery ||
		state != s.lastSentState

	if full {
		if err := s.publishTelemetryData(current); err != nil {
			return err
		}
		s.lastSentMap = curr
		s.lastSentState = state
		s.flushesSinceFull = 0
		return nil
	}

	delta, cleared := computeDelta(s.lastSentMap, curr)
	if err := s.publishTelemetryDelta(delta, cleared); err != nil {
		return err
	}
	s.lastSentMap = curr
	s.lastSentState = state
	s.flushesSinceFull++
	return nil
}

// publishTelemetryDelta publishes a sparse {type:"delta"} message carrying only
// the changed leaves (plus any cleared keys) to the same telemetry topic. An
// empty delta is still sent: it is a few dozen bytes and keeps the server's
// last-seen/liveness fresh on otherwise-idle interval ticks.
func (s *ScooterMQTTClient) publishTelemetryDelta(delta map[string]any, cleared []string) error {
	msg := make(map[string]any, len(delta)+3)
	for k, v := range delta {
		msg[k] = v
	}
	msg["type"] = "delta"
	msg["version"] = 2
	if len(cleared) > 0 {
		msg["cleared"] = cleared
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal delta: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/telemetry", s.config.Scooter.Identifier)
	token := s.mqttClient.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(models.MQTTPublishTimeout) || token.Error() != nil {
		failures := atomic.AddInt32(&s.consecutivePublishFailures, 1)
		log.Printf("Delta publish failure #%d: %v", failures, token.Error())
		if failures >= models.MaxConsecutivePublishFailures {
			log.Printf("Reached %d consecutive publish failures, forcing reconnect", failures)
			atomic.StoreInt32(&s.consecutivePublishFailures, 0)
			go s.forceReconnect()
		}
		return fmt.Errorf("failed to publish delta: %v", token.Error())
	}

	atomic.StoreInt32(&s.consecutivePublishFailures, 0)
	if s.config.Debug {
		log.Printf("Published delta to %s (%d changed, %d cleared, %d bytes): %s",
			topic, len(delta), len(cleared), len(payload), strings.TrimSpace(string(payload)))
	} else {
		log.Printf("Published delta to %s (%d changed, %d cleared, %d bytes)",
			topic, len(delta), len(cleared), len(payload))
	}
	s.updateCloudStatus()
	return nil
}
