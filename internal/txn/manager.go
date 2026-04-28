package txn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

// stagingSuffix and lkgSuffix name the sibling files used during a config
// transaction. Suffixed files MUST live in the same directory as the live
// file so atomic os.Rename works (cross-filesystem rename fails with EXDEV).
const (
	stagingSuffix = ".staging"
	lkgSuffix     = ".lkg"
)

// Manager owns the file paths and orchestrates transactions for one set of
// state files. Construct with paths and a logger; call RecoverOnBoot once at
// startup, then Run for each transaction.
type Manager struct {
	// LiveConfigPath is the canonical config file (e.g.
	// /data/radio-gaga/config.yaml). Staging and LKG files live alongside
	// with .staging and .lkg suffixes.
	LiveConfigPath string

	// PendingPath is the txn marker file. Typically ".txn-pending.json" in
	// the same directory as the live config so everything related is in one
	// place; it can be elsewhere as long as the orchestrator and recovery
	// agree on the location.
	PendingPath string

	// Logger receives structured progress lines. Pass log.Default() to send
	// to stderr / journald.
	Logger *log.Logger
}

// ProbeFunc validates a candidate. The Manager calls it once between staging
// and committing; nil error means commit, any error means roll back. The
// context is propagated through with the transaction's deadline. The Manager
// passes the staging path (where the candidate config file lives mid-txn) so
// the probe can invoke `radio-gaga -probe -config <staging>` against it.
type ProbeFunc func(ctx context.Context, stagingConfigPath string) error

// ErrInflightExists is returned by Run when a prior transaction's pending.json
// is still present. Callers should run RecoverOnBoot first.
var ErrInflightExists = errors.New("a prior pending transaction exists; run RecoverOnBoot first")

// Run performs a config-only transactional replace. Steps:
//
//  1. Refuse if pending.json already exists (caller must reconcile first).
//  2. Snapshot live → .lkg.
//  3. Write candidate → .staging.
//  4. Write pending.json with state=inflight.
//  5. Call probe(ctx). If it errors, roll back: delete .staging and .lkg,
//     delete pending.json. Live file is untouched throughout. Returns
//     (false, probe error).
//  6. On probe success: atomic rename .staging → live. Update pending.json
//     to state=committed (atomic). Delete .lkg, delete pending.json. Return
//     (true, nil).
//
// Crash safety: any crash between steps 2 and 6 leaves pending.json with
// state=inflight (or absent if we crashed in step 1/4). RecoverOnBoot rolls
// back. Crash between renames in step 6 leaves pending.json with
// state=committed; RecoverOnBoot completes the cleanup.
func (m *Manager) Run(ctx context.Context, txnID string, kind Kind, candidate []byte, probe ProbeFunc) (committed bool, err error) {
	if kind != KindConfig {
		return false, fmt.Errorf("txn kind %q not yet supported (only %q is)", kind, KindConfig)
	}
	if probe == nil {
		return false, errors.New("probe function is required")
	}

	// 1. Refuse if a prior transaction is still pending.
	prior, err := readPending(m.PendingPath)
	if err != nil {
		return false, fmt.Errorf("inspect pending file: %w", err)
	}
	if prior != nil {
		return false, fmt.Errorf("%w (txn_id=%s, state=%s)", ErrInflightExists, prior.TxnID, prior.State)
	}

	stagingPath := m.LiveConfigPath + stagingSuffix
	lkgPath := m.LiveConfigPath + lkgSuffix

	// Defense in depth: if leftover staging/lkg files exist with no pending
	// marker, something went sideways. Clean up before proceeding so we
	// don't conflate them with the new transaction.
	for _, leftover := range []string{stagingPath, lkgPath} {
		if _, statErr := os.Stat(leftover); statErr == nil {
			m.logf("warning: removing leftover %s before starting txn %s", leftover, txnID)
			if rmErr := os.Remove(leftover); rmErr != nil {
				return false, fmt.Errorf("remove leftover %s: %w", leftover, rmErr)
			}
		}
	}

	// 2. Snapshot live → .lkg.
	if err := copyFile(m.LiveConfigPath, lkgPath); err != nil {
		return false, fmt.Errorf("snapshot live to lkg: %w", err)
	}
	m.logf("txn %s: snapshotted %s → %s", txnID, m.LiveConfigPath, lkgPath)

	// 3. Write candidate → .staging.
	if err := writeFileSync(stagingPath, candidate, 0o644); err != nil {
		_ = os.Remove(lkgPath)
		return false, fmt.Errorf("write staging: %w", err)
	}
	m.logf("txn %s: staged candidate at %s (%d bytes)", txnID, stagingPath, len(candidate))

	// 4. Write pending.json with state=inflight.
	deadlineUnix := int64(0)
	if dl, ok := ctx.Deadline(); ok {
		deadlineUnix = dl.Unix()
	}
	pending := &Pending{
		TxnID:         txnID,
		Kind:          kind,
		State:         StateInflight,
		StartedAtUnix: nowUnix(),
		DeadlineUnix:  deadlineUnix,
	}
	if err := writePending(m.PendingPath, pending); err != nil {
		_ = os.Remove(lkgPath)
		_ = os.Remove(stagingPath)
		return false, fmt.Errorf("write pending: %w", err)
	}
	m.logf("txn %s: marked inflight", txnID)

	// 5. Probe — caller validates the staged candidate.
	probeErr := probe(ctx, stagingPath)
	if probeErr != nil {
		m.logf("txn %s: probe failed: %v — rolling back", txnID, probeErr)
		// Roll back: live was never touched. Just delete the staging artifacts.
		_ = os.Remove(stagingPath)
		_ = os.Remove(lkgPath)
		_ = removePending(m.PendingPath)
		return false, fmt.Errorf("probe failed: %w", probeErr)
	}
	m.logf("txn %s: probe succeeded — committing", txnID)

	// 6a. Atomic rename .staging → live.
	if err := os.Rename(stagingPath, m.LiveConfigPath); err != nil {
		// We're still in inflight. Leave pending.json so RecoverOnBoot can
		// roll back if we crash now. Surface the error.
		return false, fmt.Errorf("commit rename %s → %s: %w", stagingPath, m.LiveConfigPath, err)
	}

	// 6b. Mark committed atomically. Crash anywhere between here and the
	// final cleanup leaves pending.committed=true; RecoverOnBoot will
	// complete cleanup on next boot, which is identical to the success path.
	pending.State = StateCommitted
	if err := writePending(m.PendingPath, pending); err != nil {
		// Surfacing this is awkward — the rename happened, so the new config
		// is live. Best we can do is log; on next boot the recovery path
		// (state=inflight, but live == staging-was) will roll back. To be
		// safe, return success but flag the marker write.
		m.logf("txn %s: WARNING marker write to %s failed after commit rename: %v", txnID, m.PendingPath, err)
		return true, fmt.Errorf("commit succeeded but marker write failed: %w", err)
	}

	// 6c. Delete LKG, delete pending.
	if err := os.Remove(lkgPath); err != nil && !os.IsNotExist(err) {
		m.logf("txn %s: warning: failed to remove lkg %s: %v", txnID, lkgPath, err)
	}
	if err := removePending(m.PendingPath); err != nil {
		m.logf("txn %s: warning: failed to remove pending %s: %v", txnID, m.PendingPath, err)
	}
	m.logf("txn %s: committed", txnID)
	return true, nil
}

// RecoverOnBoot inspects pending.json and reconciles staging/lkg files. Idempotent
// — calling it twice in a row is fine; it's a no-op the second time. Should be
// invoked exactly once during startup, before MQTT connect (so a rolled-back
// config is what gets loaded).
func (m *Manager) RecoverOnBoot() error {
	prior, err := readPending(m.PendingPath)
	if err != nil {
		return fmt.Errorf("read pending on boot: %w", err)
	}
	if prior == nil {
		// No pending → maybe orphan staging/lkg from an earlier crash before
		// we wrote the pending marker. Clean them defensively.
		m.cleanOrphans()
		return nil
	}

	stagingPath := m.LiveConfigPath + stagingSuffix
	lkgPath := m.LiveConfigPath + lkgSuffix

	switch prior.State {
	case StateCommitted:
		m.logf("txn %s: recovery — state=committed; cleaning up lkg/staging/pending", prior.TxnID)
		_ = os.Remove(lkgPath)
		_ = os.Remove(stagingPath)
		if err := removePending(m.PendingPath); err != nil {
			return fmt.Errorf("remove pending on cleanup: %w", err)
		}
	case StateInflight:
		m.logf("txn %s: recovery — state=inflight; rolling back to lkg", prior.TxnID)
		// Restore live from lkg if lkg is present.
		if _, statErr := os.Stat(lkgPath); statErr == nil {
			if err := os.Rename(lkgPath, m.LiveConfigPath); err != nil {
				return fmt.Errorf("rollback rename %s → %s: %w", lkgPath, m.LiveConfigPath, err)
			}
			m.logf("txn %s: recovery — restored live from lkg", prior.TxnID)
		} else {
			// lkg missing during inflight is the documented "impossible
			// state" — log loudly and clear the pending so the next boot is
			// normal. Live file is whatever it was; might be old, might be
			// new, but consistent with the on-disk state we have.
			m.logf("txn %s: WARNING recovery — state=inflight but lkg missing; live file is unchanged, clearing pending", prior.TxnID)
		}
		_ = os.Remove(stagingPath)
		if err := removePending(m.PendingPath); err != nil {
			return fmt.Errorf("remove pending on rollback: %w", err)
		}
	default:
		return fmt.Errorf("unknown pending state %q (txn %s)", prior.State, prior.TxnID)
	}
	return nil
}

// cleanOrphans removes leftover staging/lkg files when no pending marker is
// present. Logs each removal so unexpected files leave a trail.
func (m *Manager) cleanOrphans() {
	for _, path := range []string{m.LiveConfigPath + stagingSuffix, m.LiveConfigPath + lkgSuffix} {
		if _, err := os.Stat(path); err == nil {
			m.logf("recovery: removing orphan %s (no pending marker)", path)
			_ = os.Remove(path)
		}
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// copyFile copies src to dst, preserving permissions of src. Used for the
// live → lkg snapshot. Source is opened read-only; dst is created with the
// same permissions as src so rolling back leaves the original mode.
func copyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src %s: %w", src, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("open dst %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("fsync dst %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dst %s: %w", dst, err)
	}
	return nil
}

// writeFileSync writes data to path with the given mode and fsyncs before
// closing. Used for the candidate staging file so a crash between write and
// rename doesn't lose data the kernel was about to flush.
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := out.Write(data); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(path)
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
