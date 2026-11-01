package utils

import (
	"fmt"
	"os"
	"path/filepath"

	"bufio"
	"net"
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

func TestCoordinatorVerifiedClock(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "boot")
	if err := os.WriteFile(boot, []byte("boot-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, quality string
		ok            bool
	}{
		{"ntp", `"source":"ntp","external":true`, true},
		{"gps", `"source":"gps","external":true`, true},
		{"nrf", `"source":"nrf","external":true`, false},
		{"user", `"source":"user","external":true`, false},
		{"local", `"source":"local","external":true`, false},
		{"holdover", `"source":"ntp","external":false`, false},
		{"stale", `"source":"ntp","external":true,"age_ns":31000000000`, false},
		{"residual", `"source":"ntp","external":true,"residual_seconds":5,"uncertainty_seconds":1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			l, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, `{"ok":true,"quality":{"boot_id":"boot-1","generation":1,"outcome":"completed","age_ns":0,"residual_seconds":0,"uncertainty_seconds":0,%s}}`+"\n", tc.quality)
			}()
			err = coordinatorVerifiedClock(path, boot)
			if (err == nil) != tc.ok {
				t.Fatalf("got %v, want valid=%v", err, tc.ok)
			}
		})
	}
	if err := coordinatorVerifiedClock(filepath.Join(dir, "missing"), boot); err == nil {
		t.Fatal("missing coordinator accepted")
	}
}
