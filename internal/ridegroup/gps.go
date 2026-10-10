package ridegroup

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

type GPSFix struct {
	Latitude  string   `json:"latitude"`
	Longitude string   `json:"longitude"`
	Timestamp string   `json:"timestamp"`
	Active    bool     `json:"active"`
	Connected bool     `json:"connected"`
	State     string   `json:"state"`
	Fix       string   `json:"fix"`
	Eph       *float64 `json:"eph"`
}

type FixSample struct {
	Latitude     float64
	Longitude    float64
	CapturedAt   time.Time
	AccuracyM    *float64
	HeadingDeg   *float64
	SpeedMPS     *float64
	MotionSource string
}

type MotionEstimator struct {
	previous *GPSFixPoint
}

type GPSFixPoint struct {
	Latitude   float64
	Longitude  float64
	CapturedAt time.Time
	AccuracyM  *float64
}

func parseGPSTimestamp(data []byte) (time.Time, bool) {
	var timestamp struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(data, &timestamp) != nil {
		return time.Time{}, false
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, timestamp.Timestamp)
	return capturedAt, err == nil
}

func ParseGPSFix(data []byte) (GPSFix, error) {
	if len(data) == 0 || len(data) > MaxEnvelopeBytes*4 {
		return GPSFix{}, fmt.Errorf("invalid GPS fix")
	}
	var fix GPSFix
	if err := json.Unmarshal(data, &fix); err != nil {
		return GPSFix{}, fmt.Errorf("invalid GPS fix")
	}
	if !fix.Active || !fix.Connected || fix.State != "fix-established" || (fix.Fix != "2d" && fix.Fix != "3d") {
		return GPSFix{}, fmt.Errorf("GPS fix is not established")
	}
	lat, err := strconv.ParseFloat(fix.Latitude, 64)
	if err != nil || !isFinite(lat) || lat < -90 || lat > 90 {
		return GPSFix{}, fmt.Errorf("invalid GPS fix")
	}
	lon, err := strconv.ParseFloat(fix.Longitude, 64)
	if err != nil || !isFinite(lon) || lon < -180 || lon > 180 {
		return GPSFix{}, fmt.Errorf("invalid GPS fix")
	}
	if _, err := time.Parse(time.RFC3339, fix.Timestamp); err != nil {
		if _, err = time.Parse(time.RFC3339Nano, fix.Timestamp); err != nil {
			return GPSFix{}, fmt.Errorf("invalid GPS fix")
		}
	}
	return fix, nil
}

func (e *MotionEstimator) Observe(fix GPSFix, now time.Time) (FixSample, bool) {
	lat, errLat := strconv.ParseFloat(fix.Latitude, 64)
	lon, errLon := strconv.ParseFloat(fix.Longitude, 64)
	captured, errTime := time.Parse(time.RFC3339Nano, fix.Timestamp)
	if errLat != nil || errLon != nil || errTime != nil || captured.After(now) || now.Sub(captured) > 5*time.Second || !fix.Active || !fix.Connected || fix.State != "fix-established" || (fix.Fix != "2d" && fix.Fix != "3d") || !validCoordinates(lat, lon) {
		return FixSample{}, false
	}
	if e.previous != nil && !captured.After(e.previous.CapturedAt) {
		return FixSample{}, false
	}
	var accuracy *float64
	if fix.Eph != nil && isFinite(*fix.Eph) && *fix.Eph > 0 {
		v := *fix.Eph
		accuracy = &v
	}
	sample := FixSample{Latitude: lat, Longitude: lon, CapturedAt: captured, AccuracyM: accuracy, MotionSource: "unknown"}
	current := &GPSFixPoint{Latitude: lat, Longitude: lon, CapturedAt: captured, AccuracyM: accuracy}
	if prev := e.previous; prev != nil {
		dt := captured.Sub(prev.CapturedAt).Seconds()
		if dt >= 1 && dt <= 5 && prev.AccuracyM != nil && accuracy != nil {
			distance, heading := geodesicMotion(prev.Latitude, prev.Longitude, lat, lon)
			if distance > *prev.AccuracyM+*accuracy {
				speed := distance / dt
				if speed <= 40 {
					sample.SpeedMPS = &speed
					sample.HeadingDeg = &heading
					sample.MotionSource = "position_delta"
				}
			}
		}
	}
	e.previous = current
	return sample, true
}

func (e *MotionEstimator) Reset() { e.previous = nil }

func geodesicMotion(lat1, lon1, lat2, lon2 float64) (float64, float64) {
	const earthRadiusM = 6371008.8
	toRadians := math.Pi / 180
	phi1, phi2 := lat1*toRadians, lat2*toRadians
	dPhi := (lat2 - lat1) * toRadians
	dLon := math.Mod((lon2-lon1)*toRadians+math.Pi, 2*math.Pi) - math.Pi
	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) + math.Cos(phi1)*math.Cos(phi2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	distance := 2 * earthRadiusM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	y := math.Sin(dLon) * math.Cos(phi2)
	x := math.Cos(phi1)*math.Sin(phi2) - math.Sin(phi1)*math.Cos(phi2)*math.Cos(dLon)
	heading := math.Mod(math.Atan2(y, x)/toRadians+360, 360)
	return distance, heading
}

func validCoordinates(lat, lon float64) bool {
	return isFinite(lat) && isFinite(lon) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

func validAccuracy(value *float64) bool { return value == nil || (isFinite(*value) && *value > 0) }

func validMotion(heading, speed *float64, source string) bool {
	if heading == nil || speed == nil {
		return heading == nil && speed == nil && source == "unknown"
	}
	return source == "position_delta" && isFinite(*heading) && *heading >= 0 && *heading < 360 && isFinite(*speed) && *speed >= 0 && *speed <= 40
}

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
