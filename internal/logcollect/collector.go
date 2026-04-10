// Package logcollect builds diagnostic log bundles in the universal v2 format
// shared with librescoot/lsc and parsed by sunshine. The resulting directory
// tree is:
//
//	{outputDir}/
//	  metadata.json          // bundle-level info (version, window, hosts, tool)
//	  {host-slug}/           // one per collected host (mdb, dbc)
//	    metadata.json        // per-host boot/kernel/os info + collector origin
//	    journal.log          // journalctl -o short-monotonic --since X
//	    dmesg.log            // dmesg (monotonic format), best-effort
//	    redis/{key}.json     // optional; MDB only. HGETALL snapshot, ':' → '-'
//
// All three producers/consumers (radio-gaga, lsc, sunshine) must agree on this
// layout byte-for-byte.
package logcollect

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

// bundleFormatVersion is the integer written into metadata.json's "version"
// field. Bump in lockstep with lsc and sunshine when the layout changes.
const bundleFormatVersion = 2

// toolName identifies the producer in bundle metadata.
const toolName = "radio-gaga"

// DBCSSHAddr is where radio-gaga reaches the dashboard over the internal USB
// network. Kept as a var so tests can override it.
var DBCSSHAddr = "root@192.168.7.2"

// redisSnapshotKeys lists the hash keys we HGETALL into redis/<key>.json.
// Empty/missing keys are skipped silently so the same list works on both
// librescoot and unu firmwares.
var redisSnapshotKeys = []string{
	"settings",
	"vehicle",
	"gps", "gps:filtered", "gps:raw",
	"battery:0", "battery:1",
	"aux-battery", "cb-battery",
	"engine-ecu",
	"power-manager", "power-mux",
	"modem", "internet",
	"alarm", "ble",
	"system", "dashboard", "ota",
	"navigation", "keycard",
	"version:mdb", "version:dbc",
}

// ToolMeta identifies the producer that built the bundle.
type ToolMeta struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// BundleMetadata is the top-level metadata.json written at the bundle root.
// Field order and names are part of the cross-repo format v2 contract.
type BundleMetadata struct {
	Version     int       `json:"version"`
	CollectedAt time.Time `json:"collected_at"`
	Since       string    `json:"since"`
	Until       time.Time `json:"until"`
	RequestID   string    `json:"request_id"`
	Hosts       []string  `json:"hosts"`
	Tool        ToolMeta  `json:"tool"`
}

// HostMetadata is written into <host>/metadata.json. Sunshine uses
// BootTimestamp to convert monotonic log timestamps into wall time.
type HostMetadata struct {
	Host          string    `json:"host"`
	Hostname      string    `json:"hostname,omitempty"`
	BootTimestamp time.Time `json:"boot_timestamp"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	KernelRelease string    `json:"kernel_release,omitempty"`
	OSReleaseID   string    `json:"os_release_id,omitempty"`
	OSReleaseVer  string    `json:"os_release_version,omitempty"`
	JournalBytes  int64     `json:"journal_bytes"`
	DmesgBytes    int64     `json:"dmesg_bytes"`
	Collector     string    `json:"collector"`
}

// Options controls what Collect does.
type Options struct {
	Since     string    // any journalctl --since value ("24h", "2h", "2026-04-10 12:00", ...)
	Until     time.Time // optional explicit end-of-window; defaults to collection time
	RequestID string    // copied into the bundle metadata
	// SkipDBC disables the SSH collection step, mostly for tests.
	SkipDBC bool
}

// Collect builds the bundle tree under outputDir. It always tries MDB first,
// then best-effort DBC. Any DBC failure is logged and swallowed so the caller
// still gets a usable MDB-only bundle.
func Collect(ctx context.Context, redisClient *redis.Client, outputDir string, opts Options) (*BundleMetadata, error) {
	if opts.Since == "" {
		opts.Since = "24h"
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}

	collectedAt := time.Now().UTC()
	until := opts.Until
	if until.IsZero() {
		until = collectedAt
	} else {
		until = until.UTC()
	}

	bundle := &BundleMetadata{
		Version:     bundleFormatVersion,
		CollectedAt: collectedAt,
		Since:       opts.Since,
		Until:       until,
		RequestID:   opts.RequestID,
		Hosts:       []string{"mdb"},
		Tool:        ToolMeta{Name: toolName, Version: toolVersion()},
	}

	mdbDir := filepath.Join(outputDir, "mdb")
	if err := collectMDB(ctx, redisClient, mdbDir, opts.Since); err != nil {
		return nil, fmt.Errorf("collecting mdb logs: %w", err)
	}

	if !opts.SkipDBC {
		dbcDir := filepath.Join(outputDir, "dbc")
		if err := collectDBC(ctx, dbcDir, opts.Since); err != nil {
			log.Printf("logcollect: skipping DBC (%v)", err)
			// Leave any partial files in place if useful; nothing to clean up
			// beyond the empty directory.
			_ = os.RemoveAll(dbcDir)
		} else {
			bundle.Hosts = append(bundle.Hosts, "dbc")
		}
	}

	if err := writeJSON(filepath.Join(outputDir, "metadata.json"), bundle); err != nil {
		return nil, fmt.Errorf("writing bundle metadata: %w", err)
	}
	return bundle, nil
}

// toolVersion returns the running radio-gaga module version, falling back to
// "dev" when the binary was built without module info (go run, tests).
func toolVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// collectMDB gathers local journal/dmesg/redis snapshots into hostDir.
func collectMDB(ctx context.Context, redisClient *redis.Client, hostDir, since string) error {
	if err := os.MkdirAll(filepath.Join(hostDir, "redis"), 0755); err != nil {
		return fmt.Errorf("creating mdb dirs: %w", err)
	}

	meta := HostMetadata{
		Host:          "mdb",
		BootTimestamp: readBootTimestamp(),
		UptimeSeconds: readUptime(),
		Hostname:      readHostname(),
		KernelRelease: readKernelRelease(),
		Collector:     "local",
	}
	if id, ver := readOSRelease("/etc/os-release"); id != "" || ver != "" {
		meta.OSReleaseID = id
		meta.OSReleaseVer = ver
	}

	journalPath := filepath.Join(hostDir, "journal.log")
	if n, err := runJournalctl(ctx, since, journalPath); err != nil {
		log.Printf("logcollect mdb: journalctl failed: %v", err)
	} else {
		meta.JournalBytes = n
	}

	dmesgPath := filepath.Join(hostDir, "dmesg.log")
	if n, err := runDmesg(ctx, dmesgPath); err != nil {
		log.Printf("logcollect mdb: dmesg failed: %v", err)
		// Remove any empty/partial file so the bundle simply omits dmesg.log
		// instead of claiming an empty capture.
		_ = os.Remove(dmesgPath)
	} else {
		meta.DmesgBytes = n
	}

	snapshotRedis(ctx, redisClient, filepath.Join(hostDir, "redis"))

	return writeJSON(filepath.Join(hostDir, "metadata.json"), meta)
}

// runJournalctl executes `journalctl -o short-monotonic --since X --no-pager`
// and writes stdout to outPath. Returns the number of bytes written.
func runJournalctl(ctx context.Context, since, outPath string) (int64, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	args := []string{"-o", "short-monotonic", "--no-pager"}
	if since != "" {
		args = append(args, "--since", convertSince(since))
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		return 0, statErr
	}
	return info.Size(), nil
}

// runDmesg writes `dmesg` output to outPath. We do not pass --since because
// busybox dmesg on scooters does not support it.
func runDmesg(ctx context.Context, outPath string) (int64, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	cmd := exec.CommandContext(ctx, "dmesg")
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		return 0, statErr
	}
	return info.Size(), nil
}

// snapshotRedis writes each non-empty hash from redisSnapshotKeys as
// redis/<key>.json. Returns the number of keys captured.
func snapshotRedis(ctx context.Context, client *redis.Client, dir string) int {
	if client == nil {
		return 0
	}
	count := 0
	for _, key := range redisSnapshotKeys {
		data, err := client.HGetAll(ctx, key).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		filename := strings.ReplaceAll(key, ":", "-") + ".json"
		if err := writeJSON(filepath.Join(dir, filename), data); err != nil {
			log.Printf("logcollect mdb: write redis snapshot %s: %v", key, err)
			continue
		}
		count++
	}
	return count
}

// writeJSON serializes v as indented JSON to path.
func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// readBootTimestamp derives the kernel boot wall time from /proc/uptime.
// Matches the value lsc used to embed in its old metadata.json.
func readBootTimestamp() time.Time {
	uptime := readUptime()
	if uptime <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(uptime * float64(time.Second))).UTC()
}

func readUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return secs
}

func readHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

func readKernelRelease() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readOSRelease pulls ID and VERSION_ID out of an os-release-format file.
func readOSRelease(path string) (id, version string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	return parseOSRelease(string(data))
}

func parseOSRelease(content string) (id, version string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") {
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		} else if strings.HasPrefix(line, "VERSION_ID=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"`)
		}
	}
	return id, version
}

// convertSince turns short radio-gaga/mqtt durations into something journalctl
// understands. Unrecognized values are passed through unchanged so absolute
// timestamps ("2026-04-10 12:00") still work.
func convertSince(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	// Already an absolute timestamp or journalctl-native phrase.
	if strings.ContainsAny(s, " -:") {
		return s
	}
	suffix := s[len(s)-1]
	num := s[:len(s)-1]
	if _, err := strconv.Atoi(num); err != nil {
		return s
	}
	unit := ""
	switch suffix {
	case 'm':
		unit = "minute"
	case 'h':
		unit = "hour"
	case 'd':
		unit = "day"
	case 'w':
		unit = "week"
	default:
		return s
	}
	if num != "1" {
		unit += "s"
	}
	return num + " " + unit + " ago"
}
