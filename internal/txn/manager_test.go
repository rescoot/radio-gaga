package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestManager builds a Manager rooted in a tmpdir with a seeded live config
// and returns the manager + paths for assertions. All transactional artifacts
// (.staging, .lkg, pending.json) live alongside the live file.
func newTestManager(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(live, []byte("original: true\n"), 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	pending := filepath.Join(dir, ".txn-pending.json")
	m := &Manager{LiveConfigPath: live, PendingPath: pending}
	return m, live, live + lkgSuffix, pending
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to not exist, got %v", path, err)
	}
}

func TestRun_ProbeSuccessCommits(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)

	committed, err := m.Run(context.Background(), "txn-1", KindConfig, []byte("new: true\n"),
		func(ctx context.Context, _ string) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !committed {
		t.Fatalf("expected committed=true")
	}

	if got := readFile(t, live); got != "new: true\n" {
		t.Errorf("live file content = %q, want %q", got, "new: true\n")
	}
	mustNotExist(t, live+stagingSuffix)
	mustNotExist(t, lkg)
	mustNotExist(t, pending)
}

func TestRun_ProbeFailureRollsBack(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)
	probeErr := errors.New("simulated probe failure")

	committed, err := m.Run(context.Background(), "txn-2", KindConfig, []byte("new: true\n"),
		func(ctx context.Context, _ string) error { return probeErr })
	if committed {
		t.Errorf("expected committed=false")
	}
	if err == nil || !errors.Is(err, probeErr) {
		t.Errorf("expected wrapped probe error, got %v", err)
	}

	if got := readFile(t, live); got != "original: true\n" {
		t.Errorf("live file should be untouched on rollback, got %q", got)
	}
	mustNotExist(t, live+stagingSuffix)
	mustNotExist(t, lkg)
	mustNotExist(t, pending)
}

func TestRun_RefusesWhenPendingExists(t *testing.T) {
	m, _, _, pending := newTestManager(t)
	if err := writePending(pending, &Pending{TxnID: "stale", Kind: KindConfig, State: StateInflight}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	committed, err := m.Run(context.Background(), "txn-new", KindConfig, []byte("x"),
		func(ctx context.Context, _ string) error { return nil })
	if committed || err == nil {
		t.Fatalf("expected rejection, got committed=%v err=%v", committed, err)
	}
	if !errors.Is(err, ErrInflightExists) {
		t.Errorf("expected ErrInflightExists, got %v", err)
	}
}

func TestRun_RejectsUnsupportedKind(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	_, err := m.Run(context.Background(), "txn-binary", KindBinary, []byte("x"),
		func(ctx context.Context, _ string) error { return nil })
	if err == nil {
		t.Fatalf("expected error for KindBinary in step 1")
	}
}

func TestRun_RemovesLeftoverArtifactsBeforeStarting(t *testing.T) {
	m, live, lkg, _ := newTestManager(t)
	// Simulate leftover from a prior crash that had no pending marker.
	if err := os.WriteFile(live+stagingSuffix, []byte("orphan-staging"), 0o644); err != nil {
		t.Fatalf("seed orphan staging: %v", err)
	}
	if err := os.WriteFile(lkg, []byte("orphan-lkg"), 0o644); err != nil {
		t.Fatalf("seed orphan lkg: %v", err)
	}

	committed, err := m.Run(context.Background(), "txn-3", KindConfig, []byte("new: true\n"),
		func(ctx context.Context, _ string) error { return nil })
	if err != nil || !committed {
		t.Fatalf("Run: committed=%v err=%v", committed, err)
	}
	if got := readFile(t, live); got != "new: true\n" {
		t.Errorf("live = %q, want new content", got)
	}
}

// Recovery: state=inflight + lkg present → rollback to lkg, live restored.
func TestRecoverOnBoot_InflightWithLkgRollsBack(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)

	// Simulate mid-txn crash: live was already replaced with new content,
	// lkg holds the original, pending says inflight, staging may or may
	// not still exist.
	if err := os.WriteFile(live, []byte("new: true\n"), 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := os.WriteFile(lkg, []byte("original: true\n"), 0o644); err != nil {
		t.Fatalf("seed lkg: %v", err)
	}
	if err := writePending(pending, &Pending{TxnID: "crashed", Kind: KindConfig, State: StateInflight}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}

	if got := readFile(t, live); got != "original: true\n" {
		t.Errorf("live should be restored from lkg, got %q", got)
	}
	mustNotExist(t, lkg)
	mustNotExist(t, pending)
}

// Recovery: state=inflight but lkg missing (the documented "impossible"
// state) — clear pending, leave live untouched, log loudly. We don't error.
func TestRecoverOnBoot_InflightWithoutLkgClearsPending(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)
	mustNotExist(t, lkg)

	if err := os.WriteFile(live, []byte("whatever\n"), 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := writePending(pending, &Pending{TxnID: "weird", Kind: KindConfig, State: StateInflight}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	if got := readFile(t, live); got != "whatever\n" {
		t.Errorf("live = %q, want unchanged", got)
	}
	mustNotExist(t, pending)
}

// Recovery: state=committed → renames already happened, just clean up.
func TestRecoverOnBoot_CommittedCleansUp(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)

	// Simulate post-rename, pre-cleanup crash.
	if err := os.WriteFile(live, []byte("new: true\n"), 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := os.WriteFile(lkg, []byte("original: true\n"), 0o644); err != nil {
		t.Fatalf("seed lkg: %v", err)
	}
	if err := writePending(pending, &Pending{TxnID: "almost", Kind: KindConfig, State: StateCommitted}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}

	if got := readFile(t, live); got != "new: true\n" {
		t.Errorf("live should be the committed new content, got %q", got)
	}
	mustNotExist(t, lkg)
	mustNotExist(t, pending)
}

// Recovery: no pending file but staging/lkg orphans present — clean them.
func TestRecoverOnBoot_NoPendingClearsOrphans(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)
	staging := live + stagingSuffix

	if err := os.WriteFile(staging, []byte("orphan-staging"), 0o644); err != nil {
		t.Fatalf("seed orphan staging: %v", err)
	}
	if err := os.WriteFile(lkg, []byte("orphan-lkg"), 0o644); err != nil {
		t.Fatalf("seed orphan lkg: %v", err)
	}
	mustNotExist(t, pending)

	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	mustNotExist(t, staging)
	mustNotExist(t, lkg)
	if got := readFile(t, live); got != "original: true\n" {
		t.Errorf("live should be the original (untouched), got %q", got)
	}
}

// End-to-end: a successful run followed by another successful run leaves
// the file pair in a clean state (no orphans accumulating).
func TestRun_SequentialRunsLeaveCleanState(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)

	for i, content := range []string{"first\n", "second\n", "third\n"} {
		committed, err := m.Run(context.Background(), "txn-seq", KindConfig, []byte(content),
			func(ctx context.Context, _ string) error { return nil })
		if err != nil || !committed {
			t.Fatalf("iter %d: committed=%v err=%v", i, committed, err)
		}
		if got := readFile(t, live); got != content {
			t.Errorf("iter %d: live = %q, want %q", i, got, content)
		}
		mustNotExist(t, live+stagingSuffix)
		mustNotExist(t, lkg)
		mustNotExist(t, pending)
	}
}

// Recovery is idempotent — calling it twice is safe.
func TestRecoverOnBoot_Idempotent(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// Initial-bootstrap path: no live config when Run is called. The Manager
// should skip the LKG snapshot, stage + probe + commit, and end up with a
// live config equal to the candidate.
func TestRun_NoPriorLiveCommitsLeavesLive(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.yaml")
	pending := filepath.Join(dir, ".txn-pending.json")
	m := &Manager{LiveConfigPath: live, PendingPath: pending}

	// Sanity: live doesn't exist yet.
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live should not exist at start: %v", err)
	}

	committed, err := m.Run(context.Background(), "txn-bootstrap", KindConfig, []byte("first: true\n"),
		func(ctx context.Context, _ string) error { return nil })
	if err != nil || !committed {
		t.Fatalf("Run: committed=%v err=%v", committed, err)
	}
	if got := readFile(t, live); got != "first: true\n" {
		t.Errorf("live = %q, want first content", got)
	}
	mustNotExist(t, live+stagingSuffix)
	mustNotExist(t, live+lkgSuffix)
	mustNotExist(t, pending)
}

// Initial-bootstrap probe failure: no live should remain since there was none.
func TestRun_NoPriorLiveRollbackLeavesNoLive(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.yaml")
	pending := filepath.Join(dir, ".txn-pending.json")
	m := &Manager{LiveConfigPath: live, PendingPath: pending}

	probeErr := errors.New("probe rejected")
	committed, err := m.Run(context.Background(), "txn-bootstrap-fail", KindConfig, []byte("doomed\n"),
		func(ctx context.Context, _ string) error { return probeErr })
	if committed {
		t.Errorf("expected not committed")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("expected wrapped probe error, got %v", err)
	}
	mustNotExist(t, live)
	mustNotExist(t, live+stagingSuffix)
	mustNotExist(t, live+lkgSuffix)
	mustNotExist(t, pending)
}

// Recovery from a crashed initial-bootstrap inflight: no LKG, no_prior_live=true.
// Rollback = delete staging, leave live nonexistent, clear pending.
func TestRecoverOnBoot_InflightNoPriorLiveCleansStaging(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.yaml")
	pending := filepath.Join(dir, ".txn-pending.json")
	m := &Manager{LiveConfigPath: live, PendingPath: pending}

	staging := live + stagingSuffix
	if err := os.WriteFile(staging, []byte("staged"), 0o644); err != nil {
		t.Fatalf("seed staging: %v", err)
	}
	if err := writePending(pending, &Pending{TxnID: "crashed-bootstrap", Kind: KindConfig, State: StateInflight, NoPriorLive: true}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := m.RecoverOnBoot(); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	mustNotExist(t, live)
	mustNotExist(t, staging)
	mustNotExist(t, pending)
}

// Probe respects context: a probe that honors deadline and returns ctx.Err()
// rolls back like any other probe failure.
func TestRun_ProbeDeadlineRollsBack(t *testing.T) {
	m, live, lkg, pending := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	committed, err := m.Run(ctx, "txn-cancel", KindConfig, []byte("doomed\n"),
		func(ctx context.Context, _ string) error { return ctx.Err() })
	if committed {
		t.Fatalf("expected not committed")
	}
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := readFile(t, live); got != "original: true\n" {
		t.Errorf("live = %q, want original", got)
	}
	mustNotExist(t, lkg)
	mustNotExist(t, pending)
}
