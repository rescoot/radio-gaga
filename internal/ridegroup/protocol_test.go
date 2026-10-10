package ridegroup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV1GoldenFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/v1/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			switch filepath.Base(path) {
			case "ipc-activity.json", "ipc-discovery-activity.json", "local-stop-activity.json":
				if _, err := ParseActivity(data); err != nil {
					t.Fatalf("valid activity rejected: %v", err)
				}
			case "control-clear.json", "control-grant-escaped-label.json", "control-grant-five-peers.json", "control-grant.json", "control-status.json":
				if _, err := ParseControl(data); err != nil {
					t.Fatalf("valid control rejected: %v", err)
				}
			case "peer-position.json":
				if _, err := ParsePeerPosition(data); err != nil {
					t.Fatalf("valid peer rejected: %v", err)
				}
			case "peer-status.json":
				if _, err := ParsePeerStatus(data); err != nil {
					t.Fatalf("valid peer status rejected: %v", err)
				}
			case "valid-motion.json", "valid-zero.json":
				if _, err := ParsePosition(data); err != nil {
					t.Fatalf("valid position rejected: %v", err)
				}
			case "invalid-active-stop.json", "invalid-activity-pair.json":
				if _, err := ParseActivity(data); err == nil {
					t.Fatal("invalid activity accepted")
				}
			case "invalid-control-lease.json", "invalid-control-peer-label.json":
				if _, err := ParseControl(data); err == nil {
					t.Fatal("invalid control accepted")
				}
			case "invalid-provenance.json", "invalid-extra-field.json":
				if _, err := ParsePosition(data); err == nil {
					t.Fatal("invalid position accepted")
				}
			case "invalid-stale-time.json":
				if _, err := ParsePosition(data); err != nil {
					t.Fatalf("staleness is stateful, structural parser rejected vector: %v", err)
				}
			case "invalid-peer-source-epoch.json":
				if _, err := ParsePeerPosition(data); err == nil {
					t.Fatal("invalid peer accepted")
				}
			case "wire-activity.json", "wire-discovery-activity.json":
				if _, err := ParseWireActivity(data); err != nil {
					t.Fatalf("valid wire activity rejected: %v", err)
				}
			case "hello.json", "invalid-hello-capability.json":
			default:
				t.Fatalf("unclassified shared fixture %s", filepath.Base(path))
			}
		})
	}
}

func TestPositionEnvelopeBoundsAndExactKeys(t *testing.T) {
	data, err := os.ReadFile("testdata/v1/valid-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePosition(data); err != nil {
		t.Fatal(err)
	}
	invalid := [][]byte{
		append(append([]byte(nil), data...), []byte(" "+string(data))...),
		[]byte(`{"v":1,"type":"ride_group_position","unexpected":true}`),
		make([]byte, MaxEnvelopeBytes+1),
	}
	invalid = append(invalid, []byte(`{"v":1,"type":"ride_group_position","session_id":"00000000-0000-4000-8000-000000000002","revision":1,"transport_epoch":1,"stream_id":"00000000-0000-4000-8000-000000000001","seq":1,"captured_at_ms":1781731200000,"timestamp_source":"producer_fix","lat":0,"lat":1,"lon":0,"accuracy_m":null,"heading_deg":null,"speed_mps":null,"motion_source":"unknown"}`))
	for _, input := range invalid {
		if _, err := ParsePosition(input); err == nil {
			t.Fatalf("accepted malformed envelope of size %d", len(input))
		}
	}
	activity := []byte(`{"v":1,"type":"ride_group_activity","boot_nonce":"00000000-0000-4000-8000-000000000005","seq":1,"active":null,"local_stop":false,"session_id":null,"member_id":null,"challenge":"00000000-0000-4000-8000-000000000006"}`)
	if _, err := ParseActivity(activity); err == nil {
		t.Fatal("accepted null activity boolean")
	}
}

func TestParseGPSFixRequiresProcessedActiveFixAndSourceTime(t *testing.T) {
	valid := `{"latitude":"0.000000","longitude":"0.000000","timestamp":"2026-10-10T05:30:00Z","active":true,"connected":true,"state":"fix-established","fix":"3d","eph":4.2,"speed":"27.0","course":"90.0","updated":"2026-10-10T05:30:01Z"}`
	if _, err := ParseGPSFix([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		`{"lat":0,"lon":0,"time":"2026-10-10T05:30:00Z","active":true,"connected":true,"state":"fix-established","fix":"3d"}`,
		`{"latitude":"0","longitude":"0","timestamp":"2026-10-10T05:30:00Z","active":false,"connected":true,"state":"fix-established","fix":"3d"}`,
		`{"latitude":"0","longitude":"0","timestamp":"2026-10-10T05:30:00Z","active":true,"connected":false,"state":"fix-established","fix":"3d"}`,
		`{"latitude":"0","longitude":"0","timestamp":"2026-10-10T05:30:00Z","active":true,"connected":true,"state":"searching","fix":"none"}`,
		`{"latitude":"NaN","longitude":"0","timestamp":"2026-10-10T05:30:00Z","active":true,"connected":true,"state":"fix-established","fix":"3d"}`,
		`{"latitude":"0","longitude":"0","timestamp":"not-time","active":true,"connected":true,"state":"fix-established","fix":"3d"}`,
	}
	for _, input := range invalid {
		if _, err := ParseGPSFix([]byte(input)); err == nil {
			t.Fatalf("accepted malformed GPS snapshot")
		}
	}
}

func TestMotionEstimatorRequiresFreshCertainDisplacement(t *testing.T) {
	var estimator MotionEstimator
	base := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	if _, ok := estimator.Observe(testGPSFix("52.000000", "13.000000", base, 2), base.Add(time.Second)); !ok {
		t.Fatal("first valid fix rejected")
	}
	motion, ok := estimator.Observe(testGPSFix("52.000100", "13.000000", base.Add(2*time.Second), 2), base.Add(3*time.Second))
	if !ok || motion.MotionSource != "position_delta" || motion.HeadingDeg == nil || motion.SpeedMPS == nil {
		t.Fatalf("northward motion not derived: %+v", motion)
	}
	if *motion.HeadingDeg > 1 && *motion.HeadingDeg < 359 {
		t.Fatalf("heading = %f, want north-zero", *motion.HeadingDeg)
	}
	if *motion.SpeedMPS <= 0 || *motion.SpeedMPS > 40 {
		t.Fatalf("speed = %f", *motion.SpeedMPS)
	}
	noAccuracy, ok := estimator.Observe(testGPSFix("52.000200", "13.000000", base.Add(4*time.Second), 0), base.Add(5*time.Second))
	if !ok || noAccuracy.AccuracyM != nil || noAccuracy.MotionSource != "unknown" || noAccuracy.HeadingDeg != nil || noAccuracy.SpeedMPS != nil {
		t.Fatalf("unknown accuracy fabricated motion: %+v", noAccuracy)
	}
}

func TestMotionEstimatorTimestampAndDateline(t *testing.T) {
	var estimator MotionEstimator
	base := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	if _, ok := estimator.Observe(testGPSFix("0", "179.9999", base, 1), base.Add(time.Second)); !ok {
		t.Fatal("first fix rejected")
	}
	if _, ok := estimator.Observe(testGPSFix("0", "179.9999", base, 1), base.Add(time.Second)); ok {
		t.Fatal("repeated source timestamp accepted")
	}
	sample, ok := estimator.Observe(testGPSFix("0", "-179.9999", base.Add(2*time.Second), 1), base.Add(3*time.Second))
	if !ok || sample.MotionSource != "position_delta" || sample.HeadingDeg == nil || *sample.HeadingDeg < 89 || *sample.HeadingDeg > 91 {
		t.Fatalf("dateline eastward heading incorrect: %+v", sample)
	}
}

func testGPSFix(lat, lon string, timestamp time.Time, eph float64) GPSFix {
	return GPSFix{Latitude: lat, Longitude: lon, Timestamp: timestamp.Format(time.RFC3339Nano), Active: true, Connected: true, State: "fix-established", Fix: "3d", Eph: &eph}
}
