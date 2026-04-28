// Package txn implements a transactional replace primitive for radio-gaga's
// on-disk state (config files now, binary later). A transaction stages a
// candidate, runs a caller-supplied probe to validate it, and either commits
// (atomic rename) or rolls back. A pending.json marker on disk lets the boot
// path recover from a crash mid-transaction.
//
// The crash-safety invariant: while a transaction is in flight, pending.json
// exists with state=inflight. After all renames succeed, pending.json is
// updated atomically to state=committed before any LKG cleanup. On boot:
//
//   - no pending.json   → no in-flight txn; normal startup.
//   - state=inflight    → roll back (restore from .lkg, delete .staging).
//   - state=committed   → renames already happened; just clean up LKGs.
//
// This distinguishes "crashed mid-attempt" from "crashed after success but
// before cleanup", which a naive lkg-presence check would conflate.
package txn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Kind is the shape of a transaction. For now only KindConfig is implemented;
// KindBinary and KindBoth come in later phases of the txn replace work.
type Kind string

const (
	KindConfig Kind = "config"
	KindBinary Kind = "binary"
	KindBoth   Kind = "both"
)

// State tracks where a transaction is in its lifecycle. It exists primarily
// for the boot-time recovery path — see the package doc for the invariant.
type State string

const (
	StateInflight  State = "inflight"
	StateCommitted State = "committed"
)

// Pending is the on-disk marker for an in-flight (or just-committed)
// transaction. Always written atomically (tmp + rename) so a torn write at
// boot can never produce a partially-decoded marker.
type Pending struct {
	TxnID         string `json:"txn_id"`
	Kind          Kind   `json:"kind"`
	State         State  `json:"state"`
	StartedAtUnix int64  `json:"started_at_unix"`
	DeadlineUnix  int64  `json:"deadline_unix,omitempty"`
	// NoPriorLive is true when the live file didn't exist at the start of
	// the transaction (initial bootstrap case). Recovery uses this to know
	// "rollback = delete staging" rather than the usual rename-from-lkg —
	// because there's no LKG to roll back to.
	NoPriorLive bool `json:"no_prior_live,omitempty"`
}

// readPending returns (nil, nil) if path does not exist; otherwise parses the
// file or returns a clear error. JSON parse failures are not silently treated
// as "no pending" — recovery should know the file existed but couldn't be
// read, so a human can investigate.
func readPending(path string) (*Pending, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
}

// writePending writes the marker atomically: marshal to a sibling tmp file,
// fsync, then rename over the live name. The rename is atomic on POSIX
// filesystems, so readers either see the previous version or the new one,
// never a half-written file.
func writePending(path string, p *Pending) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pending: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure pending dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".pending-*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp pending: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tmp pending: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync tmp pending: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp pending: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename tmp pending to %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// removePending deletes the marker. Missing-file is not an error.
func removePending(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// nowUnix is wrapped so tests can stub time.
var nowUnix = func() int64 { return time.Now().Unix() }
