package txn

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// SubprocessProbe returns a ProbeFunc that spawns the binary identified in
// the ProbeInfo with `-probe -config <ProbeInfo.ConfigPath>` and returns an
// error iff the child exits non-zero (or fails to start).
//
// The child's stdout/stderr are forwarded to logSink so probe diagnostics
// surface in the orchestrator's log. The context is propagated via
// exec.CommandContext; canceling it kills the child.
//
// The Manager picks BinaryPath / ConfigPath from ProbeInfo: for a config-only
// swap, BinaryPath is the currently-running binary (Manager.LiveBinaryPath)
// and ConfigPath is the staged config; for a binary swap, BinaryPath is the
// staged binary and ConfigPath is the live config; for both, both are
// staged. So the same SubprocessProbe handles every kind without
// re-binding.
func SubprocessProbe(logSink io.Writer) ProbeFunc {
	return func(ctx context.Context, info ProbeInfo) error {
		if info.BinaryPath == "" {
			return fmt.Errorf("probe: BinaryPath is empty (caller must set Manager.LiveBinaryPath)")
		}
		cmd := exec.CommandContext(ctx, info.BinaryPath, "-probe", "-config", info.ConfigPath)
		cmd.Stdout = logSink
		cmd.Stderr = logSink
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("probe subprocess (%s): %w", info.BinaryPath, err)
		}
		return nil
	}
}
