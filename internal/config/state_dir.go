package config

import (
	"log"
	"os"
	"path/filepath"
)

// stateDirCandidates is the ordered list of directories defaultStateDir tries.
// Exposed as a var so tests can override it without touching the filesystem.
var stateDirCandidates = []string{
	"/data/radio-gaga",    // LibreScoot et al — /data is the persistent partition
	"/var/lib/radio-gaga", // stock Linux / FHS — root is writable
	"/tmp/radio-gaga",     // last resort, ephemeral
}

// defaultStateDir picks a writable directory for radio-gaga's on-disk state
// when the operator hasn't passed -state-dir. The probe is "parent must
// already exist as a directory, then MkdirAll the candidate" — we skip a
// candidate if its parent doesn't exist, so we don't accidentally create a
// /data tree on a stock rootfs that just happens to be writable.
//
// The final candidate /tmp/radio-gaga always works and is the ephemeral
// fallback (state survives only until reboot).
func defaultStateDir() string {
	for _, dir := range stateDirCandidates {
		parent := filepath.Dir(dir)
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		return dir
	}
	// Should be unreachable: /tmp always exists. Log and return anyway.
	last := stateDirCandidates[len(stateDirCandidates)-1]
	if err := os.MkdirAll(last, 0o755); err != nil {
		log.Printf("Warning: could not create any state directory; using %s without MkdirAll: %v", last, err)
	}
	return last
}
