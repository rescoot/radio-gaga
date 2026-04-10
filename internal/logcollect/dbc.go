package logcollect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Timeout budget for each SSH invocation. journalctl over a 24h window on DBC
// has historically stayed well under a minute even on busy days; we give it
// three and stream straight to disk.
const (
	dbcProbeTimeout   = 10 * time.Second
	dbcJournalTimeout = 3 * time.Minute
	dbcDmesgTimeout   = 30 * time.Second
)

// collectDBC gathers journal, dmesg and host metadata from the dashboard over
// SSH. If the DBC is unreachable or doesn't respond in time, the whole call
// returns an error and the caller discards the directory.
func collectDBC(ctx context.Context, hostDir, since string) error {
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		return fmt.Errorf("creating dbc dir: %w", err)
	}

	meta, err := probeDBC(ctx)
	if err != nil {
		return err
	}
	meta.Collector = "ssh:" + DBCSSHAddr

	journalPath := filepath.Join(hostDir, "journal.log")
	if n, err := runDBCJournalctl(ctx, since, journalPath); err != nil {
		return fmt.Errorf("dbc journalctl: %w", err)
	} else {
		meta.JournalBytes = n
	}

	dmesgPath := filepath.Join(hostDir, "dmesg.log")
	if n, err := runDBCDmesg(ctx, dmesgPath); err != nil {
		log.Printf("logcollect dbc: dmesg failed: %v", err)
		_ = os.Remove(dmesgPath)
	} else {
		meta.DmesgBytes = n
	}

	return writeJSON(filepath.Join(hostDir, "metadata.json"), meta)
}

// probeDBC runs a single short SSH command that reads everything we need for
// HostMetadata in one round trip: uptime, hostname, kernel, os-release. If
// this fails the DBC is effectively unavailable for collection.
func probeDBC(ctx context.Context) (HostMetadata, error) {
	probeCtx, cancel := context.WithTimeout(ctx, dbcProbeTimeout)
	defer cancel()

	const script = `cat /proc/uptime; echo '---'; hostname; echo '---'; uname -r; echo '---'; cat /etc/os-release 2>/dev/null`
	cmd := exec.CommandContext(probeCtx, "ssh", "-y", DBCSSHAddr, script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return HostMetadata{}, fmt.Errorf("ssh probe: %w", err)
	}

	parts := strings.Split(out.String(), "---\n")
	meta := HostMetadata{Host: "dbc"}
	if len(parts) >= 1 {
		fields := strings.Fields(parts[0])
		if len(fields) >= 1 {
			if secs, err := strconv.ParseFloat(fields[0], 64); err == nil && secs > 0 {
				meta.UptimeSeconds = secs
				meta.BootTimestamp = time.Now().Add(-time.Duration(secs * float64(time.Second))).UTC()
			}
		}
	}
	if len(parts) >= 2 {
		meta.Hostname = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		meta.KernelRelease = strings.TrimSpace(parts[2])
	}
	if len(parts) >= 4 {
		id, ver := parseOSRelease(parts[3])
		meta.OSReleaseID = id
		meta.OSReleaseVer = ver
	}
	return meta, nil
}

// runDBCJournalctl streams `journalctl -o short-monotonic --since X` from the
// DBC straight into outPath. We use ssh stdin piping so nothing lands on the
// DBC's filesystem.
func runDBCJournalctl(ctx context.Context, since, outPath string) (int64, error) {
	jCtx, cancel := context.WithTimeout(ctx, dbcJournalTimeout)
	defer cancel()

	remote := "journalctl -o short-monotonic --no-pager"
	if since != "" {
		remote += " --since " + shellQuote(convertSince(since))
	}
	return streamSSHToFile(jCtx, remote, outPath)
}

func runDBCDmesg(ctx context.Context, outPath string) (int64, error) {
	dCtx, cancel := context.WithTimeout(ctx, dbcDmesgTimeout)
	defer cancel()
	return streamSSHToFile(dCtx, "dmesg", outPath)
}

// streamSSHToFile runs a remote command and writes its stdout to outPath,
// returning the number of bytes written.
func streamSSHToFile(ctx context.Context, remoteCmd, outPath string) (int64, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	cmd := exec.CommandContext(ctx, "ssh", "-y", DBCSSHAddr, remoteCmd)
	cmd.Stdout = f
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		return 0, statErr
	}
	return info.Size(), nil
}

// shellQuote wraps s in single quotes safely for use in a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
