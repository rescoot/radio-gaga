package client

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"radio-gaga/internal/models"
)

// roundtrip mimics how the client diffs: marshal to JSON then back to a generic
// map, so number types match what computeDelta sees on the wire (float64).
func roundtrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestComputeDelta_NoChange(t *testing.T) {
	prev := roundtrip(t, map[string]any{"vehicle_state": map[string]any{"state": "stand-by", "speed": 0}})
	curr := roundtrip(t, map[string]any{"vehicle_state": map[string]any{"state": "stand-by", "speed": 0}})

	delta, cleared := computeDelta(prev, curr)
	if len(delta) != 0 {
		t.Errorf("expected empty delta, got %v", delta)
	}
	if len(cleared) != 0 {
		t.Errorf("expected no cleared, got %v", cleared)
	}
}

func TestComputeDelta_ChangedLeafOnly(t *testing.T) {
	prev := roundtrip(t, map[string]any{
		"vehicle_state": map[string]any{"state": "stand-by"},
		"engine":        map[string]any{"speed": 0, "odometer": 1000},
	})
	curr := roundtrip(t, map[string]any{
		"vehicle_state": map[string]any{"state": "stand-by"},
		"engine":        map[string]any{"speed": 12, "odometer": 1000},
	})

	delta, cleared := computeDelta(prev, curr)
	if len(cleared) != 0 {
		t.Errorf("expected no cleared, got %v", cleared)
	}
	want := map[string]any{"engine": map[string]any{"speed": float64(12)}}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("delta = %v, want %v (only the changed leaf, no unchanged sections or siblings)", delta, want)
	}
}

func TestComputeDelta_NewKeyAndClearedKey(t *testing.T) {
	prev := roundtrip(t, map[string]any{
		"battery0": map[string]any{"level": 80, "serial_number": "ABC"},
	})
	curr := roundtrip(t, map[string]any{
		"battery0": map[string]any{"level": 80, "present": true},
	})

	delta, cleared := computeDelta(prev, curr)

	wantDelta := map[string]any{"battery0": map[string]any{"present": true}}
	if !reflect.DeepEqual(delta, wantDelta) {
		t.Errorf("delta = %v, want %v", delta, wantDelta)
	}
	wantCleared := []string{"battery0.serial_number"}
	if !reflect.DeepEqual(cleared, wantCleared) {
		t.Errorf("cleared = %v, want %v", cleared, wantCleared)
	}
}

func TestComputeDelta_ArrayChange(t *testing.T) {
	prev := roundtrip(t, map[string]any{"battery0": map[string]any{"temps": []int{20, 21, 20, 22}}})
	curr := roundtrip(t, map[string]any{"battery0": map[string]any{"temps": []int{20, 21, 20, 23}}})

	delta, _ := computeDelta(prev, curr)
	inner, ok := delta["battery0"].(map[string]any)
	if !ok || inner["temps"] == nil {
		t.Fatalf("expected battery0.temps in delta, got %v", delta)
	}
}

func TestComputeDelta_TopLevelCleared(t *testing.T) {
	prev := roundtrip(t, map[string]any{"navigation": map[string]any{"destination": "x"}, "engine": map[string]any{"speed": 1}})
	curr := roundtrip(t, map[string]any{"engine": map[string]any{"speed": 1}})

	delta, cleared := computeDelta(prev, curr)
	if len(delta) != 0 {
		t.Errorf("expected empty delta, got %v", delta)
	}
	sort.Strings(cleared)
	if !reflect.DeepEqual(cleared, []string{"navigation"}) {
		t.Errorf("cleared = %v, want [navigation]", cleared)
	}
}

func TestTelemetryToMap_V2Shape(t *testing.T) {
	td := &models.TelemetryData{Version: 2}
	td.VehicleState = models.VehicleState{State: "stand-by"}
	td.Engine = models.EngineData{Speed: 7}

	m, err := telemetryToMap(td)
	if err != nil {
		t.Fatalf("telemetryToMap: %v", err)
	}
	vs, ok := m["vehicle_state"].(map[string]any)
	if !ok {
		t.Fatalf("expected vehicle_state section, got %v", m["vehicle_state"])
	}
	if vs["state"] != "stand-by" {
		t.Errorf("vehicle_state.state = %v, want stand-by", vs["state"])
	}
}
