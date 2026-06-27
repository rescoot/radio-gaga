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

func TestSmoothParkedGPS(t *testing.T) {
	// last reported position
	lat0, lng0 := 52.523000, 13.457700

	t.Run("parked sub-threshold jitter holds the last reported position", func(t *testing.T) {
		// ~2m north of the last reported fix
		td := &models.TelemetryData{GPS: models.GPSData{Lat: 52.523018, Lng: lng0}}
		rl, rg, valid := smoothParkedGPS(td, lat0, lng0, true, false)
		if td.GPS.Lat != lat0 || td.GPS.Lng != lng0 {
			t.Errorf("jitter must be held to last reported, got (%v,%v)", td.GPS.Lat, td.GPS.Lng)
		}
		if rl != lat0 || rg != lng0 || !valid {
			t.Errorf("returned state = (%v,%v,%v), want held (%v,%v,true)", rl, rg, valid, lat0, lng0)
		}
	})

	t.Run("parked move beyond threshold reports the new fix", func(t *testing.T) {
		// ~20m north
		td := &models.TelemetryData{GPS: models.GPSData{Lat: 52.523180, Lng: lng0}}
		rl, _, _ := smoothParkedGPS(td, lat0, lng0, true, false)
		if td.GPS.Lat != 52.523180 {
			t.Errorf("a real move must report the new fix, got %v", td.GPS.Lat)
		}
		if rl != 52.523180 {
			t.Errorf("returned last-reported must advance, got %v", rl)
		}
	})

	t.Run("driving always reports the raw fix", func(t *testing.T) {
		td := &models.TelemetryData{GPS: models.GPSData{Lat: 52.523018, Lng: lng0}}
		smoothParkedGPS(td, lat0, lng0, true, true)
		if td.GPS.Lat != 52.523018 {
			t.Errorf("driving must keep the raw fix, got %v", td.GPS.Lat)
		}
	})

	t.Run("no prior reported position adopts the current fix", func(t *testing.T) {
		td := &models.TelemetryData{GPS: models.GPSData{Lat: 52.5, Lng: 13.4}}
		rl, rg, valid := smoothParkedGPS(td, 0, 0, false, false)
		if rl != 52.5 || rg != 13.4 || !valid {
			t.Errorf("first fix should be adopted, got (%v,%v,%v)", rl, rg, valid)
		}
	})

	t.Run("null-island fix is treated as invalid and not held", func(t *testing.T) {
		td := &models.TelemetryData{GPS: models.GPSData{Lat: 0, Lng: 0}}
		_, _, valid := smoothParkedGPS(td, lat0, lng0, true, false)
		if valid {
			t.Error("0,0 fix must be reported invalid")
		}
	})
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
