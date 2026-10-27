package utils

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"radio-gaga/internal/models"
)

// EnsureVerifiedClock requires a recent NTP reference before verified TLS.
// When the application NTP client is disabled, chrony must report a healthy,
// recently updated system clock instead.
func EnsureVerifiedClock(config *models.NTPConfig) error {
	if config != nil && !config.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "chronyc", "-c", "tracking").Output()
		if err != nil {
			return fmt.Errorf("NTP is disabled and the system time service is unavailable")
		}
		return checkChronyTracking(string(out), time.Now())
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

func checkChronyTracking(value string, now time.Time) error {
	fields := strings.Split(strings.TrimSpace(value), ",")
	if len(fields) != 14 || fields[13] != "Normal" {
		return fmt.Errorf("system time service is not synchronized")
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
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
