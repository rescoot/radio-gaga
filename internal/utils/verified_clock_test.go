package utils

import (
	"fmt"
	"os"
	"path/filepath"

	"radio-gaga/internal/models"
	"strings"
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
		{"local pseudo reference", strings.Replace(row(1, now.Unix(), 0, "Normal"), "8E84C8F1", "7F7F0101", 1), false},
		{"zero reference", strings.Replace(row(1, now.Unix(), 0, "Normal"), "8E84C8F1", "00000000", 1), false},
		{"excess dispersion", strings.Replace(row(3, now.Unix(), 0, "Normal"), ",0,0,10,Normal", ",0,6,10,Normal", 1), false},
		{"stale", row(3, now.Unix()-7201, 0, "Normal"), false},
		{"future", row(3, now.Unix()+6, 0, "Normal"), false},
		{"unsynchronized", row(3, now.Unix(), 0, "Not synchronised"), false},
		{"invalid stratum", row(16, now.Unix(), 0, "Normal"), false},
		{"large offset", row(3, now.Unix(), 6, "Normal"), false},
		{"invalid", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChronyTracking(tc.value, now, true)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestChronyTrackingPlatformBoundary(t *testing.T) {
	now := time.Unix(1793101100, 0)
	local := fmt.Sprintf("7F7F0101,127.127.1.1,1,%d,0,0,0,0,0,0,0,6,10,Normal", now.Unix())
	if err := checkChronyTracking(local, now, false); err != nil {
		t.Fatalf("non-Librescoot compatibility: %v", err)
	}
	if err := checkChronyTracking(local, now, true); err == nil {
		t.Fatal("Librescoot accepted local reference")
	}
}

func TestEnsureVerifiedClockLibrescootUsesChronyForBothNTPModes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	tracking := fmt.Sprintf("8E84C8F1,192.0.2.1,3,%d,0,0,0,0,0,0,0,0,10,Normal", now)
	if err := os.WriteFile(filepath.Join(dir, "chronyc"), []byte("#!/bin/sh\necho '"+tracking+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, enabled := range []bool{false, true} {
		if err := EnsureVerifiedClock(&models.NTPConfig{Enabled: enabled}, true); err != nil {
			t.Fatalf("NTP enabled=%v: %v", enabled, err)
		}
	}
}
