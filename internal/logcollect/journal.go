package logcollect

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A young boot can start with an old wall clock. Split at one fixed timestamp
// so its pre-sync entries supplement, but never duplicate, the requested window.
func journalCommands(since string, uptime float64, now time.Time) [][]string {
	base := []string{"-o", "short-monotonic", "--no-pager"}
	window, ok := journalWindow(since)
	if !ok || uptime <= 0 || uptime > window.Seconds() {
		if since != "" {
			base = append(base, "--since", convertSince(since))
		}
		return [][]string{base}
	}

	cutoff := now.Add(-window).Truncate(time.Microsecond)
	missingBoot := append(append([]string{}, base...), "--boot=0", "--until", journalTimestamp(cutoff.Add(-time.Microsecond)))
	requested := append(append([]string{}, base...), "--since", journalTimestamp(cutoff))
	return [][]string{missingBoot, requested}
}

func journalTimestamp(t time.Time) string {
	return fmt.Sprintf("@%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

func journalWindow(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	if len(s) < 2 {
		return 0, false
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	default:
		return 0, false
	}
	n, err := strconv.ParseUint(s[:len(s)-1], 10, 64)
	if err != nil || n == 0 || n > uint64((1<<63-1)/unit) {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

func remoteJournalCommand(commands [][]string) string {
	parts := make([]string, 0, len(commands))
	for _, args := range commands {
		quoted := []string{"journalctl"}
		for _, arg := range args {
			quoted = append(quoted, shellQuote(arg))
		}
		parts = append(parts, strings.Join(quoted, " "))
	}
	return strings.Join(parts, " && ")
}
