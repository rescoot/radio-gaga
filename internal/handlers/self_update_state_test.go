package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"radio-gaga/internal/models"
)

func TestSelfUpdateConcurrentCommandsAndCompletedRedelivery(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(fmt.Sprint(succeeds), func(t *testing.T) {
			dir := t.TempDir()
			live, configPath := filepath.Join(dir, "radio-gaga"), filepath.Join(dir, "config.yaml")
			mustWriteFile(t, live, []byte("working binary"), 0o755)
			mustWriteFile(t, configPath, []byte("config: unchanged\n"), 0o600)
			candidate := []byte("not executable")
			if succeeds {
				candidate = []byte("#!/bin/sh\nexit 0\n")
			}
			var downloads, restarts atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if downloads.Add(1) == 1 {
					close(started)
					<-release
				}
				_, _ = w.Write(candidate)
			}))
			defer server.Close()
			originalExecutable, originalRestart := selfUpdateExecutable, selfUpdateRestart
			defer func() { selfUpdateExecutable, selfUpdateRestart = originalExecutable, originalRestart }()
			selfUpdateExecutable = func() (string, error) { return live, nil }
			selfUpdateRestart = func() error { restarts.Add(1); return nil }
			client := &updateResponseClient{MockCommandHandlerClient: MockCommandHandlerClient{configPath: configPath}, responses: make(chan string, 8), params: make(chan struct{}, 32)}
			payload, _ := json.Marshal(models.CommandMessage{Command: "self_update", RequestID: "delivery", Params: map[string]interface{}{"url": server.URL, "checksum": "sha256:" + updateDigest(candidate)}})
			message := &selfUpdateMessage{payload: payload}
			config := &models.Config{StateDir: filepath.Join(dir, "state")}
			done := make(chan struct{}, 2)
			dispatch := func() {
				HandleCommand(client, nil, nil, context.Background(), config, "test", message)
				done <- struct{}{}
			}
			go dispatch()
			<-started
			go dispatch()
			// Both commands resolve the policy before entering the single-flight.
			for i := 0; i < 4; i++ {
				<-client.params
			}
			select {
			case status := <-client.responses:
				t.Fatalf("premature response before probe: %s", status)
			default:
			}
			close(release)
			<-done
			<-done
			want := "error"
			if succeeds {
				want = "success"
			}
			for i := 0; i < 2; i++ {
				if status := <-client.responses; status != want {
					t.Fatalf("response=%s want=%s", status, want)
				}
			}
			dispatch()
			<-done
			if status := <-client.responses; status != want {
				t.Fatalf("completed replay=%s", status)
			}
			if downloads.Load() != 1 {
				t.Fatalf("duplicate downloaded %d times", downloads.Load())
			}
			wantRestarts := int32(0)
			if succeeds {
				wantRestarts = 1
			}
			if restarts.Load() != wantRestarts {
				t.Fatalf("duplicate installed/restarted %d times", restarts.Load())
			}
			if !succeeds {
				// A deliberate new request can re-probe, without transferring again.
				err := handleSelfUpdateCommand(client, map[string]interface{}{"url": server.URL, "checksum": "sha256:" + updateDigest(candidate)}, "new-attempt", config)
				if err == nil || downloads.Load() != 1 {
					t.Fatalf("failed probe lost verified cache: %v downloads=%d", err, downloads.Load())
				}
				got, _ := os.ReadFile(live)
				if string(got) != "working binary" {
					t.Fatalf("failed probe overwrote live: %q", got)
				}
			}
			got, _ := os.ReadFile(configPath)
			if string(got) != "config: unchanged\n" {
				t.Fatalf("config changed: %q", got)
			}
		})
	}
}

type updateResponseClient struct {
	MockCommandHandlerClient
	responses chan string
	params    chan struct{}
}

func (c *updateResponseClient) SendCommandResponse(_ string, status, _ string) { c.responses <- status }
func (c *updateResponseClient) GetCommandParam(_, _ string, fallback interface{}) interface{} {
	c.params <- struct{}{}
	return fallback
}

type selfUpdateMessage struct{ payload []byte }

func (*selfUpdateMessage) Duplicate() bool   { return true }
func (*selfUpdateMessage) Qos() byte         { return 1 }
func (*selfUpdateMessage) Retained() bool    { return false }
func (*selfUpdateMessage) Topic() string     { return "scooters/test/commands" }
func (*selfUpdateMessage) MessageID() uint16 { return 1 }
func (m *selfUpdateMessage) Payload() []byte { return m.payload }
func (*selfUpdateMessage) Ack()              {}

func TestSelfUpdateCoordinatorBusyAndCanceledWaiter(t *testing.T) {
	dir := t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- coordinatedSelfUpdate(context.Background(), dir, "first", "binary", func() error { close(started); <-release; return nil })
	}()
	<-started
	if err := coordinatedSelfUpdate(context.Background(), dir, "other", "other binary", func() error { t.Error("concurrent install started"); return nil }); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("busy result: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := coordinatedSelfUpdate(ctx, dir, "first", "binary", func() error { t.Error("duplicate started"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSelfUpdateCompletedFailuresAndRequestCollisionPersist(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	run := func() error { calls++; return errors.New("probe rejected") }
	for i := 0; i < 2; i++ {
		if err := coordinatedSelfUpdate(context.Background(), dir, "request", "binary", run); err == nil || err.Error() != "probe rejected" {
			t.Fatalf("failure replay: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("completed failure retried %d times", calls)
	}
	if err := coordinatedSelfUpdate(context.Background(), dir, "request", "changed", run); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("request collision: %v", err)
	}
	if err := coordinatedSelfUpdate(context.Background(), dir, "new request", "binary", run); err == nil || calls != 2 {
		t.Fatalf("new request not allowed: %v calls=%d", err, calls)
	}
}

func TestSelfUpdateCoordinatorCrossProcessLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := coordinatedSelfUpdate(context.Background(), dir, "request", "binary", func() error { t.Error("ran while file lock held"); return nil }); err == nil {
		t.Fatal("cross-process lock ignored")
	}
}

func TestSelfUpdateResultRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	results := []selfUpdateResult{{Request: "expired", Identity: "binary", Finished: now.Add(-selfUpdateCacheTTL - time.Hour)}}
	for i := 0; i < 63; i++ {
		results = append(results, selfUpdateResult{Request: fmt.Sprint(i), Identity: "binary", Finished: now})
	}
	path := filepath.Join(dir, "results.json")
	if err := writeUpdateJSON(path, results); err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"new", "newer"} {
		if err := coordinatedSelfUpdate(context.Background(), dir, request, "binary", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := readUpdateJSON(path, &results, 256*1024); err != nil {
		t.Fatal(err)
	}
	if len(results) != 64 || results[0].Request != "1" {
		t.Fatalf("unbounded/stale results: %v", results)
	}
}

func TestSelfUpdateConfigurationValidation(t *testing.T) {
	for _, value := range []interface{}{"0s", "48h", "invalid", -10.0} {
		client := &updatePolicyClient{value: value}
		if _, err := selfUpdateDownloadPolicy(client); err == nil {
			t.Fatalf("accepted duration %v", value)
		}
	}
	client := &updatePolicyClient{value: "10m"}
	policy, err := selfUpdateDownloadPolicy(client)
	if err != nil || policy.budget != 10*time.Minute || policy.stall != 10*time.Minute {
		t.Fatalf("configured policy=%+v err=%v", policy, err)
	}
}

type updatePolicyClient struct {
	MockCommandHandlerClient
	value interface{}
}

func (c *updatePolicyClient) GetCommandParam(_, _ string, _ interface{}) interface{} { return c.value }

func TestSelfUpdateShutdownDoesNotRecordCompletedFailure(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	err := coordinatedSelfUpdate(ctx, dir, "request", "binary", func() error {
		cancel()
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "results.json")); !os.IsNotExist(err) {
		t.Fatalf("shutdown recorded a completed outcome: %v", err)
	}
	runs := 0
	if err := coordinatedSelfUpdate(context.Background(), dir, "request", "binary", func() error { runs++; return nil }); err != nil || runs != 1 {
		t.Fatalf("redelivery did not resume: %v runs=%d", err, runs)
	}
}

func TestSelfUpdateResultSizeRemainsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 65; i++ {
		_ = coordinatedSelfUpdate(context.Background(), dir, fmt.Sprint(i), "binary", func() error { return errors.New(strings.Repeat("\x00", 2048)) })
	}
	var results []selfUpdateResult
	if err := readUpdateJSON(filepath.Join(dir, "results.json"), &results, 256*1024); err != nil || len(results) != 64 {
		t.Fatalf("result size bound: %v entries=%d", err, len(results))
	}
}
