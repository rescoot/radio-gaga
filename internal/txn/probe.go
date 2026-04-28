package txn

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// SubprocessProbe returns a ProbeFunc that spawns `binaryPath -probe -config <stagingPath>`
// and returns an error iff the child exits non-zero (or fails to start). The
// staging path is provided by the Manager at probe time, so the same probe
// function works for repeated transactions without re-binding the path.
//
// The child's stdout/stderr are forwarded to logSink so probe diagnostics
// surface in the orchestrator's log. The context is propagated via
// exec.CommandContext; canceling it kills the child.
//
// For a config-only transaction, binaryPath is the currently-running radio-gaga
// (resolved via os.Executable()) — the same binary, validating the new config.
// For a binary-swap transaction (later phase), binaryPath is the staged
// candidate at <exe>.staging — testing whether the *new* binary can start and
// reach the cloud with the staged config.
func SubprocessProbe(binaryPath string, logSink io.Writer) ProbeFunc {
	return func(ctx context.Context, stagingConfigPath string) error {
		cmd := exec.CommandContext(ctx, binaryPath, "-probe", "-config", stagingConfigPath)
		cmd.Stdout = logSink
		cmd.Stderr = logSink
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("probe subprocess (%s): %w", binaryPath, err)
		}
		return nil
	}
}
