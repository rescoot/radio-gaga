package utils

import (
	"fmt"
	"testing"
	"time"
)

func TestChronyTrackingRequiresRecentSynchronizedReference(t *testing.T) {
	now := time.Unix(1793101100, 0)
	row := func(stratum int, reference int64, offset float64, leap string) string {
		return fmt.Sprintf("8E84C8F1,192.0.2.1,%d,%d,%.3f,0,0,0,0,0,0,0,10,%s", stratum, reference, offset, leap)
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"healthy", row(3, now.Unix()-60, .001, "Normal"), true},
		{"stale", row(3, now.Unix()-7201, 0, "Normal"), false},
		{"future", row(3, now.Unix()+6, 0, "Normal"), false},
		{"unsynchronized", row(3, now.Unix(), 0, "Not synchronised"), false},
		{"invalid stratum", row(16, now.Unix(), 0, "Normal"), false},
		{"large offset", row(3, now.Unix(), 6, "Normal"), false},
		{"invalid", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChronyTracking(tc.value, now)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
