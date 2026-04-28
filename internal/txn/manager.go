package txn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

// stagingSuffix and lkgSuffix name the sibling files used during a
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

	// LiveBinaryPath is the canonical binary path (e.g. /usr/bin/radio-gaga).
	// Required only when the transaction kind involves a binary swap; for
	// config-only managers leave empty. Caller typically populates this
	// from os.Executable() at construction time.
	LiveBinaryPath string

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
// context is propagated through with the transaction's deadline.
type ProbeFunc func(ctx context.Context, info ProbeInfo) error

// ProbeInfo tells the probe which binary to spawn and which config to feed
// it. For a config-only swap, BinaryPath is the live (existing) binary and
// ConfigPath is the staging file. For a binary swap, BinaryPath is the
// staging file and ConfigPath is the live config. For combined swaps, both
// are staging.
type ProbeInfo struct {
	Kind       Kind
	BinaryPath string
	ConfigPath string
}

// ErrInflightExists is returned by Run when a prior transaction's pending.json
// is still present. Callers should run RecoverOnBoot first.
var ErrInflightExists = errors.New("a prior pending transaction exists; run RecoverOnBoot first")

// Run performs a transactional replace of config, binary, or both:
//
//  1. Refuse if pending.json already exists.
//  2. Snapshot live → .lkg for each file kind being swapped (or note
//     no_prior_live_* if no live file yet).
//  3. Write candidate(s) → .staging.
//  4. Write pending.json with state=inflight.
//  5. Call probe(ctx). If it errors, roll back: delete .staging and .lkg
//     for each swapped kind, delete pending.json. Live files untouched.
//  6. On probe success: atomic rename for each swapped kind. Update
//     pending.json to state=committed (atomic). Delete .lkg files, delete
//     pending.json. Returns (true, nil).
//
// Crash safety: any crash between steps 2 and 6 leaves pending.json with
// state=inflight (or absent if we crashed in step 1/4). RecoverOnBoot rolls
// back. Crash between renames in step 6 leaves pending.json with
// state=committed; RecoverOnBoot completes the cleanup.
//
// For a binary swap, the running process is still executing the OLD binary
// after Run returns (its in-memory image was loaded from disk before the
// rename). The caller decides how to bring the new binary into use:
// syscall.Exec the new binary, send self SIGTERM and let systemd respawn,
// or just exit (`Restart=always` in the unit covers it).
func (m *Manager) Run(ctx context.Context, txnID string, kind Kind, c Candidate, probe ProbeFunc) (committed bool, err error) {
	if probe == nil {
		return false, errors.New("probe function is required")
	}
	if err := validateKind(kind, c, m); err != nil {
		return false, err
	}

	// 1. Refuse if a prior transaction is still pending.
	prior, err := readPending(m.PendingPath)
	if err != nil {
		return false, fmt.Errorf("inspect pending file: %w", err)
	}
	if prior != nil {
		return false, fmt.Errorf("%w (txn_id=%s, state=%s)", ErrInflightExists, prior.TxnID, prior.State)
	}

	cfgStaging, cfgLkg := m.LiveConfigPath+stagingSuffix, m.LiveConfigPath+lkgSuffix
	binStaging, binLkg := m.LiveBinaryPath+stagingSuffix, m.LiveBinaryPath+lkgSuffix

	swapsConfig := kind == KindConfig || kind == KindBoth
	swapsBinary := kind == KindBinary || kind == KindBoth

	// Defense in depth: if leftover staging/lkg files exist with no pending
	// marker, something went sideways. Clean up before proceeding.
	leftovers := []string{}
	if swapsConfig {
		leftovers = append(leftovers, cfgStaging, cfgLkg)
	}
	if swapsBinary {
		leftovers = append(leftovers, binStaging, binLkg)
	}
	for _, leftover := range leftovers {
		if _, statErr := os.Stat(leftover); statErr == nil {
			m.logf("warning: removing leftover %s before starting txn %s", leftover, txnID)
			if rmErr := os.Remove(leftover); rmErr != nil {
				return false, fmt.Errorf("remove leftover %s: %w", leftover, rmErr)
			}
		}
	}

	// 2. Snapshot LKGs (or note no_prior_live).
	noPriorLiveConfig, noPriorLiveBinary := false, false
	if swapsConfig {
		var err error
		noPriorLiveConfig, err = snapshotLkg(m, m.LiveConfigPath, cfgLkg, txnID)
		if err != nil {
			return false, err
		}
	}
	if swapsBinary {
		var err error
		noPriorLiveBinary, err = snapshotLkg(m, m.LiveBinaryPath, binLkg, txnID)
		if err != nil {
			rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
			return false, err
		}
	}

	// 3. Stage candidates.
	if swapsConfig {
		if err := writeFileSync(cfgStaging, c.Config, 0o644); err != nil {
			rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
			return false, fmt.Errorf("write config staging: %w", err)
		}
		m.logf("txn %s: staged config at %s (%d bytes)", txnID, cfgStaging, len(c.Config))
	}
	if swapsBinary {
		// Binaries need the executable bit. We chmod after write; the
		// file mode passed to OpenFile/Create is filtered by the process
		// umask, so set explicitly.
		if err := writeFileSync(binStaging, c.Binary, 0o755); err != nil {
			rollbackStaging(m, swapsConfig, false, cfgStaging, binStaging)
			rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
			return false, fmt.Errorf("write binary staging: %w", err)
		}
		if err := os.Chmod(binStaging, 0o755); err != nil {
			rollbackStaging(m, swapsConfig, swapsBinary, cfgStaging, binStaging)
			rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
			return false, fmt.Errorf("chmod binary staging: %w", err)
		}
		m.logf("txn %s: staged binary at %s (%d bytes)", txnID, binStaging, len(c.Binary))
	}

	// 4. Write pending.json with state=inflight.
	deadlineUnix := int64(0)
	if dl, ok := ctx.Deadline(); ok {
		deadlineUnix = dl.Unix()
	}
	pending := &Pending{
		TxnID:             txnID,
		Kind:              kind,
		State:             StateInflight,
		StartedAtUnix:     nowUnix(),
		DeadlineUnix:      deadlineUnix,
		NoPriorLiveConfig: noPriorLiveConfig,
		NoPriorLiveBinary: noPriorLiveBinary,
	}
	if err := writePending(m.PendingPath, pending); err != nil {
		rollbackStaging(m, swapsConfig, swapsBinary, cfgStaging, binStaging)
		rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
		return false, fmt.Errorf("write pending: %w", err)
	}
	m.logf("txn %s: marked inflight (kind=%s)", txnID, kind)

	// 5. Probe — caller validates the staged candidate(s).
	info := ProbeInfo{
		Kind:       kind,
		BinaryPath: pickBinaryForProbe(m, binStaging, swapsBinary),
		ConfigPath: pickConfigForProbe(m, cfgStaging, swapsConfig),
	}
	probeErr := probe(ctx, info)
	if probeErr != nil {
		m.logf("txn %s: probe failed: %v — rolling back", txnID, probeErr)
		rollbackStaging(m, swapsConfig, swapsBinary, cfgStaging, binStaging)
		rollbackLkgs(m, swapsConfig, swapsBinary, cfgLkg, binLkg)
		_ = removePending(m.PendingPath)
		return false, fmt.Errorf("probe failed: %w", probeErr)
	}
	m.logf("txn %s: probe succeeded — committing", txnID)

	// 6a. Atomic renames. Order: binary first (so if we crash between, the
	// new binary is on disk but config is unchanged — slightly safer because
	// a binary that requires a new config field would still see the old
	// config field present, just unused; the reverse breaks).
	if swapsBinary {
		if err := os.Rename(binStaging, m.LiveBinaryPath); err != nil {
			return false, fmt.Errorf("commit rename binary %s → %s: %w", binStaging, m.LiveBinaryPath, err)
		}
	}
	if swapsConfig {
		if err := os.Rename(cfgStaging, m.LiveConfigPath); err != nil {
			return false, fmt.Errorf("commit rename config %s → %s: %w", cfgStaging, m.LiveConfigPath, err)
		}
	}

	// 6b. Mark committed atomically.
	pending.State = StateCommitted
	if err := writePending(m.PendingPath, pending); err != nil {
		m.logf("txn %s: WARNING marker write to %s failed after commit rename: %v", txnID, m.PendingPath, err)
		return true, fmt.Errorf("commit succeeded but marker write failed: %w", err)
	}

	// 6c. Delete LKGs and pending.
	if swapsConfig {
		if err := os.Remove(cfgLkg); err != nil && !os.IsNotExist(err) {
			m.logf("txn %s: warning: failed to remove config lkg: %v", txnID, err)
		}
	}
	if swapsBinary {
		if err := os.Remove(binLkg); err != nil && !os.IsNotExist(err) {
			m.logf("txn %s: warning: failed to remove binary lkg: %v", txnID, err)
		}
	}
	if err := removePending(m.PendingPath); err != nil {
		m.logf("txn %s: warning: failed to remove pending: %v", txnID, err)
	}
	m.logf("txn %s: committed", txnID)
	return true, nil
}

// validateKind checks that Candidate has the right fields populated for the
// declared Kind, and that the Manager has the paths it needs.
func validateKind(kind Kind, c Candidate, m *Manager) error {
	switch kind {
	case KindConfig:
		if len(c.Config) == 0 {
			return errors.New("KindConfig requires Candidate.Config")
		}
		if len(c.Binary) > 0 {
			return errors.New("KindConfig must not include Candidate.Binary")
		}
		if m.LiveConfigPath == "" {
			return errors.New("LiveConfigPath required for KindConfig")
		}
	case KindBinary:
		if len(c.Binary) == 0 {
			return errors.New("KindBinary requires Candidate.Binary")
		}
		if len(c.Config) > 0 {
			return errors.New("KindBinary must not include Candidate.Config")
		}
		if m.LiveBinaryPath == "" {
			return errors.New("LiveBinaryPath required for KindBinary")
		}
	case KindBoth:
		if len(c.Config) == 0 || len(c.Binary) == 0 {
			return errors.New("KindBoth requires both Candidate.Config and Candidate.Binary")
		}
		if m.LiveConfigPath == "" || m.LiveBinaryPath == "" {
			return errors.New("LiveConfigPath and LiveBinaryPath required for KindBoth")
		}
	default:
		return fmt.Errorf("unknown txn kind %q", kind)
	}
	return nil
}

// snapshotLkg copies live → lkgPath unless live doesn't exist (initial
// bootstrap), in which case it returns noPriorLive=true and skips the copy.
func snapshotLkg(m *Manager, livePath, lkgPath, txnID string) (noPriorLive bool, err error) {
	info, statErr := os.Stat(livePath)
	if os.IsNotExist(statErr) {
		m.logf("txn %s: no prior live at %s; treating as initial install", txnID, livePath)
		return true, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("stat live %s: %w", livePath, statErr)
	}
	_ = info // (mode preserved by copyFile)
	if err := copyFile(livePath, lkgPath); err != nil {
		return false, fmt.Errorf("snapshot %s → %s: %w", livePath, lkgPath, err)
	}
	m.logf("txn %s: snapshotted %s → %s", txnID, livePath, lkgPath)
	return false, nil
}

// rollbackStaging deletes the staging files we created (call on early-failure
// paths only; the probe-failure path inlines this for clarity).
func rollbackStaging(m *Manager, swapsConfig, swapsBinary bool, cfgStaging, binStaging string) {
	if swapsConfig {
		_ = os.Remove(cfgStaging)
	}
	if swapsBinary {
		_ = os.Remove(binStaging)
	}
	_ = m // m is here for symmetry; could log if useful
}

func rollbackLkgs(m *Manager, swapsConfig, swapsBinary bool, cfgLkg, binLkg string) {
	if swapsConfig {
		_ = os.Remove(cfgLkg)
	}
	if swapsBinary {
		_ = os.Remove(binLkg)
	}
	_ = m
}

// pickBinaryForProbe / pickConfigForProbe decide what to feed to the probe
// child. Staging if we're swapping it, else live.
func pickBinaryForProbe(m *Manager, binStaging string, swapsBinary bool) string {
	if swapsBinary {
		return binStaging
	}
	return m.LiveBinaryPath
}

func pickConfigForProbe(m *Manager, cfgStaging string, swapsConfig bool) string {
	if swapsConfig {
		return cfgStaging
	}
	return m.LiveConfigPath
}

// RecoverOnBoot inspects pending.json and reconciles staging/lkg files.
// Idempotent — calling it twice is fine. Should be invoked exactly once
// during startup, before MQTT connect (so a rolled-back config is what gets
// loaded, and a rolled-back binary is what's on disk for systemd's next
// respawn).
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

	cfgStaging, cfgLkg := m.LiveConfigPath+stagingSuffix, m.LiveConfigPath+lkgSuffix
	binStaging, binLkg := m.LiveBinaryPath+stagingSuffix, m.LiveBinaryPath+lkgSuffix
	swapsConfig := prior.Kind == KindConfig || prior.Kind == KindBoth
	swapsBinary := prior.Kind == KindBinary || prior.Kind == KindBoth

	switch prior.State {
	case StateCommitted:
		m.logf("txn %s: recovery — state=committed; cleaning up lkgs/pending", prior.TxnID)
		if swapsConfig {
			_ = os.Remove(cfgLkg)
			_ = os.Remove(cfgStaging)
		}
		if swapsBinary {
			_ = os.Remove(binLkg)
			_ = os.Remove(binStaging)
		}
		if err := removePending(m.PendingPath); err != nil {
			return fmt.Errorf("remove pending on cleanup: %w", err)
		}
	case StateInflight:
		m.logf("txn %s: recovery — state=inflight (kind=%s); rolling back", prior.TxnID, prior.Kind)
		if swapsConfig {
			if err := rollbackOne(m, m.LiveConfigPath, cfgLkg, cfgStaging, prior.NoPriorLiveConfig, prior.TxnID, "config"); err != nil {
				return err
			}
		}
		if swapsBinary {
			if err := rollbackOne(m, m.LiveBinaryPath, binLkg, binStaging, prior.NoPriorLiveBinary, prior.TxnID, "binary"); err != nil {
				return err
			}
		}
		if err := removePending(m.PendingPath); err != nil {
			return fmt.Errorf("remove pending on rollback: %w", err)
		}
	default:
		return fmt.Errorf("unknown pending state %q (txn %s)", prior.State, prior.TxnID)
	}
	return nil
}

// rollbackOne handles the recovery for a single file kind (config or binary).
// Logic mirrors the package-level invariant: lkg present → restore it;
// no_prior_live → just delete staging; else log loudly and continue.
func rollbackOne(m *Manager, livePath, lkgPath, stagingPath string, noPriorLive bool, txnID, label string) error {
	if noPriorLive {
		m.logf("txn %s: recovery (%s) — no_prior_live; deleting staging (no live to restore)", txnID, label)
		_ = os.Remove(stagingPath)
		_ = os.Remove(livePath)
		return nil
	}
	if _, statErr := os.Stat(lkgPath); statErr == nil {
		if err := os.Rename(lkgPath, livePath); err != nil {
			return fmt.Errorf("rollback rename %s → %s: %w", lkgPath, livePath, err)
		}
		m.logf("txn %s: recovery (%s) — restored live from lkg", txnID, label)
	} else {
		m.logf("txn %s: WARNING recovery (%s) — state=inflight but lkg missing; live unchanged", txnID, label)
	}
	_ = os.Remove(stagingPath)
	return nil
}

// cleanOrphans removes leftover staging/lkg files when no pending marker is
// present. Logs each removal so unexpected files leave a trail.
func (m *Manager) cleanOrphans() {
	candidates := []string{}
	if m.LiveConfigPath != "" {
		candidates = append(candidates, m.LiveConfigPath+stagingSuffix, m.LiveConfigPath+lkgSuffix)
	}
	if m.LiveBinaryPath != "" {
		candidates = append(candidates, m.LiveBinaryPath+stagingSuffix, m.LiveBinaryPath+lkgSuffix)
	}
	for _, path := range candidates {
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
// live → lkg snapshot.
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
// closing.
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
