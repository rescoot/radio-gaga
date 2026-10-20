package handlers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"radio-gaga/internal/handlers/commands"
	"radio-gaga/internal/models"
	"radio-gaga/internal/txn"
)

const (
	selfUpdateProbeTimeout = 60 * time.Second
	selfUpdateMaxBytes     = 64 * 1024 * 1024
)

var (
	selfUpdateExecutable = os.Executable
	selfUpdateRestart    = commands.HandleRestartCommand
	selfUpdateMount      = func(target, mode string) error {
		output, err := exec.Command("mount", "-o", "remount,"+mode, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount -o remount,%s %s: %w: %s", mode, target, err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	selfUpdateMountPoint    = findSelfUpdateMountPoint
	selfUpdateProbeWritable = probeSelfUpdateWritable
)

// handleSelfUpdateCommand downloads and verifies the candidate, executes that
// exact binary in probe mode, and only then atomically installs it. The probe
// must start, connect to MQTT, subscribe to the command topic, and remain
// connected briefly. A failed probe leaves the running binary untouched.
func handleSelfUpdateCommand(client CommandHandlerClient, params map[string]interface{}, requestID string, config *models.Config) error {
	return handleSelfUpdateCommandContext(context.Background(), client, params, requestID, config)
}

func handleSelfUpdateCommandContext(ctx context.Context, client CommandHandlerClient, params map[string]interface{}, requestID string, config *models.Config) error {
	updateURL, ok := params["url"].(string)
	if !ok || updateURL == "" {
		return fmt.Errorf("update URL not specified or invalid")
	}

	checksum, ok := params["checksum"].(string)
	if !ok || checksum == "" {
		return fmt.Errorf("checksum not specified or invalid")
	}

	parts := strings.SplitN(checksum, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid checksum format. Expected format: algorithm:value")
	}

	if err := validateUpdateChecksum(parts[0], parts[1]); err != nil {
		return err
	}
	dir, err := selfUpdateStateDir(client, config)
	if err != nil {
		return err
	}
	policy, err := selfUpdateDownloadPolicy(client)
	if err != nil {
		return err
	}
	return coordinatedSelfUpdate(ctx, dir, requestID, updateIdentity(updateURL, parts[0], parts[1]), func() error {
		httpClient := updateHTTPClient()
		defer httpClient.CloseIdleConnections()
		binary, err := downloadSelfUpdateBinary(ctx, httpClient, dir, updateURL, parts[0], parts[1], policy)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return installSelfUpdate(ctx, client, binary, requestID)
	})
}

func installSelfUpdate(ctx context.Context, client CommandHandlerClient, binary []byte, requestID string) error {
	configPath := client.GetConfigPath()
	if configPath == "" {
		return fmt.Errorf("config path unavailable; cannot test candidate")
	}

	executablePath, err := selfUpdateExecutable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return fmt.Errorf("resolve current executable symlinks: %w", err)
	}

	remountedTarget, err := ensureSelfUpdateWritable(filepath.Dir(executablePath))
	if err != nil {
		return err
	}
	if remountedTarget != "" {
		defer restoreSelfUpdateReadOnly(remountedTarget)
	}

	manager := &txn.Manager{
		LiveConfigPath: configPath,
		LiveBinaryPath: executablePath,
		PendingPath:    filepath.Join(filepath.Dir(configPath), ".txn-pending.json"),
		Logger:         log.Default(),
	}

	txnID := "self-update-" + requestID
	if requestID == "" {
		txnID = fmt.Sprintf("self-update-%d", time.Now().UnixNano())
	}

	ctx, cancel := context.WithTimeout(ctx, selfUpdateProbeTimeout)
	defer cancel()

	committed, runErr := manager.Run(
		ctx,
		txnID,
		txn.KindBinary,
		txn.Candidate{Binary: binary},
		txn.SubprocessProbe(log.Default().Writer()),
	)
	if !committed {
		return fmt.Errorf("candidate rejected; current binary preserved: %w", runErr)
	}
	if runErr != nil {
		log.Printf("Self-update committed with cleanup warning: %v", runErr)
	}

	// Return to the normal command dispatcher so it can publish the success
	// response. The delayed SIGTERM then lets systemd start the probed binary.
	if err := selfUpdateRestart(); err != nil {
		return fmt.Errorf("update committed but failed to schedule restart: %w", err)
	}
	return nil
}

// ensureSelfUpdateWritable remounts the filesystem containing the executable
// directory read-write. Stock OSTree scooters mount /usr separately from /, so
// remounting / alone does not make /usr/bin writable. The returned mount point
// is empty when no remount was needed.
func ensureSelfUpdateWritable(dir string) (string, error) {
	initialErr := selfUpdateProbeWritable(dir)
	if initialErr == nil {
		return "", nil
	}

	target, err := selfUpdateMountPoint(dir)
	if err != nil {
		return "", fmt.Errorf("locate executable filesystem (initial error: %v): %w", initialErr, err)
	}
	// Use the platform's mount helper rather than a bare mount(2) call so it
	// supplies the existing device and filesystem details for the remount.
	log.Printf("Self-update executable directory is not writable; remounting %s read-write", target)
	if err := selfUpdateMount(target, "rw"); err != nil {
		return "", fmt.Errorf("make executable directory writable (initial error: %v): %w", initialErr, err)
	}
	if err := selfUpdateProbeWritable(dir); err != nil {
		// We cannot proceed, but still make a best-effort attempt to restore the
		// original read-only state before returning the useful write error.
		_ = selfUpdateMount(target, "ro")
		return "", fmt.Errorf("remount of %s reported success but executable directory is still not writable: %w", target, err)
	}
	return target, nil
}

func findSelfUpdateMountPoint(path string) (string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer file.Close()
	return findSelfUpdateMountPointFrom(file, path)
}

func findSelfUpdateMountPointFrom(mountInfo io.Reader, path string) (string, error) {
	path = filepath.Clean(path)
	best := ""
	scanner := bufio.NewScanner(mountInfo)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountPoint := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if path == mountPoint || (mountPoint != "/" && strings.HasPrefix(path, mountPoint+"/")) || mountPoint == "/" {
			if len(mountPoint) > len(best) {
				best = mountPoint
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if best == "" {
		return "", fmt.Errorf("no mount point found for %s", path)
	}
	return best, nil
}

func probeSelfUpdateWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".radio-gaga-write-test-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func restoreSelfUpdateReadOnly(target string) {
	log.Printf("Self-update remounting %s read-only", target)
	if err := selfUpdateMount(target, "ro"); err != nil {
		// Restoring read-only is deliberately best-effort. It must never turn a
		// successfully installed update into a failed command response.
		log.Printf("Warning: failed to remount %s read-only: %v", target, err)
	}
}
