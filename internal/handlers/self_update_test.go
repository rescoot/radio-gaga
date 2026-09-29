package handlers

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"radio-gaga/internal/models"
)

type MockCommandHandlerClient struct {
	configPath string
}

func (m *MockCommandHandlerClient) SendCommandResponse(requestID, status, message string) {}
func (m *MockCommandHandlerClient) SendCommandResponseWithPID(requestID, status, message string, pid int) {
}
func (m *MockCommandHandlerClient) CleanRetainedMessage(topic string) error { return nil }
func (m *MockCommandHandlerClient) GetCommandParam(command, param string, defaultValue interface{}) interface{} {
	return defaultValue
}
func (m *MockCommandHandlerClient) PublishTelemetryData(current *models.TelemetryData) error {
	return nil
}
func (m *MockCommandHandlerClient) GetConfigPath() string { return m.configPath }
func (m *MockCommandHandlerClient) RequestReconnect()     {}

func TestSelfUpdateAllowsSlowDownloads(t *testing.T) {
	if selfUpdateDownloadTimeout < 2*time.Minute {
		t.Fatalf("download timeout %s is too short for cellular transfers", selfUpdateDownloadTimeout)
	}
	if selfUpdateProbeTimeout >= selfUpdateDownloadTimeout {
		t.Fatalf("probe timeout %s should not consume the download window %s", selfUpdateProbeTimeout, selfUpdateDownloadTimeout)
	}
}

func TestFindSelfUpdateMountPointUsesDeepestContainingMount(t *testing.T) {
	mountInfo := strings.NewReader(`25 1 179:1 / / rw,relatime - ext4 /dev/mmcblk1p1 rw
26 25 0:20 /usr /usr ro,relatime - bind /usr ro
27 26 0:21 / /usr/share/special ro,relatime - bind special ro
`)
	got, err := findSelfUpdateMountPointFrom(mountInfo, "/usr/bin")
	if err != nil {
		t.Fatalf("findSelfUpdateMountPointFrom() error = %v", err)
	}
	if got != "/usr" {
		t.Fatalf("mount point = %q, want /usr", got)
	}
}

func TestEnsureSelfUpdateWritableRemountsExecutableFilesystemAndRechecks(t *testing.T) {
	originalMount, originalMountPoint, originalProbe := selfUpdateMount, selfUpdateMountPoint, selfUpdateProbeWritable
	defer func() {
		selfUpdateMount = originalMount
		selfUpdateMountPoint = originalMountPoint
		selfUpdateProbeWritable = originalProbe
	}()

	probeCalls := 0
	selfUpdateProbeWritable = func(string) error {
		probeCalls++
		if probeCalls == 1 {
			return errors.New("read-only file system")
		}
		return nil
	}
	selfUpdateMountPoint = func(string) (string, error) { return "/usr", nil }
	var mounts []string
	selfUpdateMount = func(target, mode string) error {
		mounts = append(mounts, target+":"+mode)
		return nil
	}

	remountedTarget, err := ensureSelfUpdateWritable("/usr/bin")
	if err != nil {
		t.Fatalf("ensureSelfUpdateWritable() error = %v", err)
	}
	if remountedTarget != "/usr" {
		t.Fatalf("remounted target = %q, want /usr", remountedTarget)
	}
	if probeCalls != 2 {
		t.Fatalf("write probe calls = %d, want 2", probeCalls)
	}
	if fmt.Sprint(mounts) != "[/usr:rw]" {
		t.Fatalf("mounts = %v, want [/usr:rw]", mounts)
	}
}

func TestEnsureSelfUpdateWritableRejectsIneffectiveRemount(t *testing.T) {
	originalMount, originalMountPoint, originalProbe := selfUpdateMount, selfUpdateMountPoint, selfUpdateProbeWritable
	defer func() {
		selfUpdateMount = originalMount
		selfUpdateMountPoint = originalMountPoint
		selfUpdateProbeWritable = originalProbe
	}()

	selfUpdateProbeWritable = func(string) error { return errors.New("read-only file system") }
	selfUpdateMountPoint = func(string) (string, error) { return "/usr", nil }
	var mounts []string
	selfUpdateMount = func(target, mode string) error {
		mounts = append(mounts, target+":"+mode)
		return nil
	}

	remountedTarget, err := ensureSelfUpdateWritable("/usr/bin")
	if err == nil || !strings.Contains(err.Error(), "still not writable") {
		t.Fatalf("ensureSelfUpdateWritable() error = %v", err)
	}
	if remountedTarget != "" {
		t.Fatalf("remounted target = %q after ineffective remount", remountedTarget)
	}
	if fmt.Sprint(mounts) != "[/usr:rw /usr:ro]" {
		t.Fatalf("mounts = %v, want [/usr:rw /usr:ro]", mounts)
	}
}

func TestSelfUpdateDownloadsZstdAndVerifiesUncompressedChecksum(t *testing.T) {
	binary := []byte("a verified executable candidate")
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(binary, nil)
	encoder.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/zstd") {
			t.Errorf("missing zstd support in Accept: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/zstd")
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	digest := sha256.Sum256(binary)
	got, err := downloadSelfUpdateBinary(server.URL, "sha256", fmt.Sprintf("%x", digest[:]))
	if err != nil || string(got) != string(binary) {
		t.Fatalf("compressed download = %q, %v", got, err)
	}
	_, err = downloadSelfUpdateBinary(server.URL, "sha256", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("compressed checksum mismatch = %v", err)
	}
}

func TestSelfUpdateRejectsInvalidZstd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zstd")
		_, _ = w.Write([]byte("not a zstd frame"))
	}))
	defer server.Close()
	_, err := downloadSelfUpdateBinary(server.URL, "sha256", strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("invalid compressed binary must be rejected")
	}
}

func TestSelfUpdateCommitsOnlyAfterCandidateProbeSucceeds(t *testing.T) {
	tempDir := t.TempDir()
	liveBinary := filepath.Join(tempDir, "radio-gaga")
	configPath := filepath.Join(tempDir, "config.yaml")
	oldBinary := []byte("old binary")
	candidate := []byte("#!/bin/sh\n[ \"$1\" = \"-probe\" ] || exit 3\nexit 0\n")
	mustWriteFile(t, liveBinary, oldBinary, 0o755)
	mustWriteFile(t, configPath, []byte("test: true\n"), 0o644)

	server, checksum := binaryServer(t, candidate)
	client := &MockCommandHandlerClient{configPath: configPath}

	originalExecutable, originalRestart := selfUpdateExecutable, selfUpdateRestart
	defer func() {
		selfUpdateExecutable = originalExecutable
		selfUpdateRestart = originalRestart
	}()
	selfUpdateExecutable = func() (string, error) { return liveBinary, nil }
	restartScheduled := false
	selfUpdateRestart = func() error {
		restartScheduled = true
		return nil
	}

	err := handleSelfUpdateCommand(client, map[string]interface{}{
		"url":      server.URL,
		"checksum": checksum,
	}, "request-1", &models.Config{})
	if err != nil {
		t.Fatalf("handleSelfUpdateCommand: %v", err)
	}
	if !restartScheduled {
		t.Fatal("expected restart to be scheduled after commit")
	}
	got, err := os.ReadFile(liveBinary)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(candidate) {
		t.Fatalf("live binary was not replaced with probed candidate: %q", got)
	}
}

func TestSelfUpdateProbeFailurePreservesCurrentBinary(t *testing.T) {
	tempDir := t.TempDir()
	liveBinary := filepath.Join(tempDir, "radio-gaga")
	configPath := filepath.Join(tempDir, "config.yaml")
	oldBinary := []byte("known-good binary")
	candidate := []byte("not an executable for this architecture")
	mustWriteFile(t, liveBinary, oldBinary, 0o755)
	mustWriteFile(t, configPath, []byte("test: true\n"), 0o644)

	server, checksum := binaryServer(t, candidate)
	client := &MockCommandHandlerClient{configPath: configPath}

	originalExecutable, originalRestart := selfUpdateExecutable, selfUpdateRestart
	defer func() {
		selfUpdateExecutable = originalExecutable
		selfUpdateRestart = originalRestart
	}()
	selfUpdateExecutable = func() (string, error) { return liveBinary, nil }
	restartScheduled := false
	selfUpdateRestart = func() error {
		restartScheduled = true
		return nil
	}

	err := handleSelfUpdateCommand(client, map[string]interface{}{
		"url":      server.URL,
		"checksum": checksum,
	}, "request-2", &models.Config{})
	if err == nil || !strings.Contains(err.Error(), "candidate rejected; current binary preserved") {
		t.Fatalf("expected candidate rejection, got %v", err)
	}
	if restartScheduled {
		t.Fatal("restart must not be scheduled after a failed probe")
	}
	got, readErr := os.ReadFile(liveBinary)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldBinary) {
		t.Fatalf("failed candidate changed live binary: %q", got)
	}
}

func TestSelfUpdateValidatesRequestAndChecksum(t *testing.T) {
	binary := []byte("candidate")
	server, checksum := binaryServer(t, binary)
	client := &MockCommandHandlerClient{configPath: "/tmp/config.yaml"}

	tests := []struct {
		name    string
		params  map[string]interface{}
		message string
	}{
		{"missing URL", map[string]interface{}{"checksum": checksum}, "update URL not specified"},
		{"missing checksum", map[string]interface{}{"url": server.URL}, "checksum not specified"},
		{"invalid checksum", map[string]interface{}{"url": server.URL, "checksum": "invalid"}, "invalid checksum format"},
		{"checksum mismatch", map[string]interface{}{"url": server.URL, "checksum": "sha256:" + strings.Repeat("0", 64)}, "checksum mismatch"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := handleSelfUpdateCommand(client, test.params, "request", &models.Config{})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected error containing %q, got %v", test.message, err)
			}
		})
	}
}

func binaryServer(t *testing.T, binary []byte) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(binary)
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(binary)
	return server, fmt.Sprintf("sha256:%x", digest[:])
}

func mustWriteFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}
