package client

import (
	"encoding/json"
	"fmt"
	"log"
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

// publishTelemetrySmart sends a full snapshot or a delta depending on what has
// changed since the last send. It serialises sends under telMu so lastSentMap
// stays consistent across the ticker, monitor-flush and state-change callers.
func (s *ScooterMQTTClient) publishTelemetrySmart(current *models.TelemetryData, forceFull bool) error {
	curr, err := telemetryToMap(current)
	if err != nil {
		return fmt.Errorf("failed to map telemetry: %v", err)
	}
	state := current.VehicleState.State

	s.telMu.Lock()
	defer s.telMu.Unlock()

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
