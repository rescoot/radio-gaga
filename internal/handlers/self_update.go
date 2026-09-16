package handlers

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"radio-gaga/internal/handlers/commands"
	"radio-gaga/internal/models"
	"radio-gaga/internal/txn"
	"radio-gaga/internal/utils"
)

const (
	selfUpdateDeadline = 60 * time.Second
	selfUpdateMaxBytes = 64 * 1024 * 1024
)

var (
	selfUpdateExecutable = os.Executable
	selfUpdateRestart    = commands.HandleRestartCommand
)

// handleSelfUpdateCommand downloads and verifies the candidate, executes that
// exact binary in probe mode, and only then atomically installs it. The probe
// must start, connect to MQTT, subscribe to the command topic, and remain
// connected briefly. A failed probe leaves the running binary untouched.
func handleSelfUpdateCommand(client CommandHandlerClient, params map[string]interface{}, requestID string, _ *models.Config) error {
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

	binary, err := downloadSelfUpdateBinary(updateURL, parts[0], parts[1])
	if err != nil {
		return err
	}

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

	remounted, err := ensureSelfUpdateWritable(filepath.Dir(executablePath))
	if err != nil {
		return err
	}
	if remounted {
		defer restoreSelfUpdateReadOnly()
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

	ctx, cancel := context.WithTimeout(context.Background(), selfUpdateDeadline)
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

func downloadSelfUpdateBinary(url, algorithm, expectedChecksum string) ([]byte, error) {
	hasher, err := utils.CreateHash(algorithm)
	if err != nil {
		return nil, fmt.Errorf("failed to create hash: %w", err)
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // Device clocks may be invalid.
	httpClient := &http.Client{Transport: transport, Timeout: selfUpdateDeadline}

	response, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to download new binary: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download new binary: HTTP %d", response.StatusCode)
	}

	limited := io.LimitReader(response.Body, selfUpdateMaxBytes+1)
	binary, err := io.ReadAll(io.TeeReader(limited, hasher))
	if err != nil {
		return nil, fmt.Errorf("failed to download new binary: %w", err)
	}
	if len(binary) > selfUpdateMaxBytes {
		return nil, fmt.Errorf("new binary exceeds %d-byte limit", selfUpdateMaxBytes)
	}

	calculatedChecksum := fmt.Sprintf("%x", hasher.Sum(nil))
	if !strings.EqualFold(calculatedChecksum, expectedChecksum) {
		return nil, fmt.Errorf("checksum mismatch. Expected: %s, got: %s", expectedChecksum, calculatedChecksum)
	}
	log.Printf("Self-update checksum verification successful: %s", calculatedChecksum)
	return binary, nil
}

// ensureSelfUpdateWritable remounts / read-write when the executable directory
// is on a read-only root filesystem. It returns true only when it remounted /.
func ensureSelfUpdateWritable(dir string) (bool, error) {
	probe, err := os.CreateTemp(dir, ".radio-gaga-write-test-*")
	if err == nil {
		name := probe.Name()
		_ = probe.Close()
		_ = os.Remove(name)
		return false, nil
	}

	log.Printf("Self-update executable directory is not writable; remounting root read-write")
	if mountErr := syscall.Mount("", "/", "", syscall.MS_REMOUNT, ""); mountErr != nil {
		return false, fmt.Errorf("make executable directory writable (initial error: %v): %w", err, mountErr)
	}
	return true, nil
}

func restoreSelfUpdateReadOnly() {
	log.Printf("Self-update remounting root read-only")
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		log.Printf("Warning: failed to remount root read-only: %v", err)
	}
}
