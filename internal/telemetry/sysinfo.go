package telemetry

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultUptimeRefresh backs the slow interval when the configured slow
// priority is missing or unparseable.
const defaultUptimeRefresh = time.Hour

var (
	uptimeMu     sync.Mutex
	uptimeValue  float64
	uptimeReadAt time.Time
)

// uptimeSeconds returns the host uptime, re-reading /proc/uptime at most once
// per refresh. Uptime is monotonic and always "changes", so reading it fresh on
// every packet would ride into every delta; sampling it on the slow cadence
// keeps it a slow-path field.
func uptimeSeconds(refresh time.Duration) float64 {
	if refresh <= 0 {
		refresh = defaultUptimeRefresh
	}

	uptimeMu.Lock()
	defer uptimeMu.Unlock()

	if uptimeReadAt.IsZero() || time.Since(uptimeReadAt) >= refresh {
		uptimeValue = readUptimeSeconds()
		uptimeReadAt = time.Now()
	}
	return uptimeValue
}

// readUptimeSeconds returns seconds since boot from /proc/uptime, or 0 when
// the file cannot be read or parsed.
func readUptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return seconds
}

// readBootID returns the kernel boot id reported by /proc, or "" when the
// file cannot be read.
func readBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
