package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"radio-gaga/internal/models"
)

// EnsureVerifiedClock requires a recent NTP reference before verified TLS.
// When the application NTP client is disabled, chrony must report a healthy,
// recently updated system clock instead.
func EnsureVerifiedClock(config *models.NTPConfig, librescoot bool) error {
	if librescoot {
		return coordinatorVerifiedClock("/run/clock-coordinator/control.sock", "/proc/sys/kernel/random/boot_id")
	}
	if config != nil && !config.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "chronyc", "-c", "tracking").Output()
		if err != nil {
			return fmt.Errorf("NTP is disabled and the system time service is unavailable")
		}
		return checkChronyTracking(string(out), time.Now(), librescoot)
	}
	reference, err := QueryNTPTime(config)
	if err != nil {
		return fmt.Errorf("NTP time unavailable: %w", err)
	}
	if absDuration(time.Since(reference)) > 5*time.Second {
		tv := syscall.NsecToTimeval(reference.UnixNano())
		if err := syscall.Settimeofday(&tv); err != nil {
			return fmt.Errorf("cannot synchronize system clock: %w", err)
		}
	}
	if absDuration(time.Since(reference)) > 5*time.Second {
		return fmt.Errorf("system clock differs from NTP")
	}
	return nil
}

func coordinatorVerifiedClock(socket, bootPath string) error {
	var started, finished unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &started); err != nil {
		return err
	}
	boot, err := os.ReadFile(bootPath)
	if err != nil {
		return fmt.Errorf("clock boot identity unavailable: %w", err)
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return fmt.Errorf("clock coordinator unavailable: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(map[string]string{"op": "status"}); err != nil {
		return err
	}
	var response struct {
		OK      bool `json:"ok"`
		Quality struct {
			BootID      string   `json:"boot_id"`
			Generation  uint64   `json:"generation"`
			Source      string   `json:"source"`
			External    bool     `json:"external"`
			AgeNS       *int64   `json:"age_ns"`
			Residual    *float64 `json:"residual_seconds"`
			Uncertainty *float64 `json:"uncertainty_seconds"`
			Outcome     string   `json:"outcome"`
			Preparing   *bool    `json:"preparing"`
		} `json:"quality"`
	}
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return fmt.Errorf("invalid clock status: %w", err)
	}
	q := response.Quality
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &finished); err != nil {
		return err
	}
	elapsed := finished.Nano() - started.Nano()
	if elapsed < 0 || elapsed > int64(30*time.Second) {
		return fmt.Errorf("stale coordinator response")
	}
	if !response.OK || strings.TrimSpace(string(boot)) == "" || q.BootID != strings.TrimSpace(string(boot)) || q.Generation == 0 ||
		q.Preparing == nil || *q.Preparing || !q.External || (q.Source != "gps" && q.Source != "ntp") || q.AgeNS == nil || q.Residual == nil || q.Uncertainty == nil || *q.AgeNS < 0 || *q.AgeNS > int64(30*time.Second)-elapsed ||
		q.Outcome == "unknown" || q.Outcome == "in-flight" || q.Outcome == "" ||
		math.IsNaN(*q.Residual) || math.IsInf(*q.Residual, 0) || math.IsNaN(*q.Uncertainty) || math.IsInf(*q.Uncertainty, 0) || *q.Uncertainty < 0 || math.Abs(*q.Residual)+*q.Uncertainty > 5 {
		return fmt.Errorf("clock coordinator has no fresh external confirmation")
	}
	return nil
}

func checkChronyTracking(value string, now time.Time, librescoot bool) error {
	fields := strings.Split(strings.TrimSpace(value), ",")
	if len(fields) != 14 || fields[13] != "Normal" {
		return fmt.Errorf("system time service is not synchronized")
	}
	if librescoot {
		refID, err := strconv.ParseUint(fields[0], 16, 32)
		if err != nil || len(fields[0]) != 8 || refID == 0 || refID>>24 == 127 {
			return fmt.Errorf("system time service has no external reference")
		}
	}
	stratum, err := strconv.Atoi(fields[2])
	if err != nil || stratum < 1 || stratum > 15 {
		return fmt.Errorf("system time service has no valid reference")
	}
	reference, err := strconv.ParseFloat(fields[3], 64)
	if err != nil || math.IsNaN(reference) || math.IsInf(reference, 0) {
		return fmt.Errorf("invalid NTP reference time")
	}
	age := float64(now.UnixNano())/1e9 - reference
	if age < -5 || age > 7200 {
		return fmt.Errorf("system time service reference is stale")
	}
	offset, err := strconv.ParseFloat(fields[4], 64)
	if err != nil || math.IsNaN(offset) || math.IsInf(offset, 0) || math.Abs(offset) > 5 {
		return fmt.Errorf("system clock is not sufficiently synchronized")
	}
	if librescoot {
		rootDelay, err := strconv.ParseFloat(fields[10], 64)
		if err != nil || math.IsNaN(rootDelay) || math.IsInf(rootDelay, 0) || rootDelay < 0 {
			return fmt.Errorf("invalid system time reference delay")
		}
		rootDispersion, err := strconv.ParseFloat(fields[11], 64)
		if err != nil || math.IsNaN(rootDispersion) || math.IsInf(rootDispersion, 0) || rootDispersion < 0 || math.Abs(offset)+rootDelay/2+rootDispersion > 5 {
			return fmt.Errorf("system time reference uncertainty is too large")
		}
	}
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
