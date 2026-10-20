package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func fastUpdatePolicy() updateDownloadPolicy {
	policy := defaultUpdateDownloadPolicy()
	policy.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	policy.jitter = func(cap time.Duration) time.Duration { return cap / 2 }
	return policy
}

func updateDigest(binary []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(binary))
}

func interruptedUpdate(w http.ResponseWriter, wire []byte) {
	w.Header().Set("Content-Length", fmt.Sprint(len(wire)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wire[:len(wire)/2])
	w.(http.Flusher).Flush()
}

func TestUpdateResumesWireRepresentationAfterRestart(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint("zstd=", compressed), func(t *testing.T) {
			binary := bytes.Repeat([]byte("a candidate executable\n"), 100)
			wire := binary
			if compressed {
				encoder, _ := zstd.NewWriter(nil)
				wire = encoder.EncodeAll(binary, nil)
				encoder.Close()
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v1"`)
				if compressed {
					w.Header().Set("Content-Type", "application/zstd")
				}
				if calls.Add(1) == 1 {
					interruptedUpdate(w, wire)
					return
				}
				offset := len(wire) / 2
				if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", offset) || r.Header.Get("If-Range") != `"v1"` {
					t.Errorf("unsafe/missing range: %v", r.Header)
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(wire)-1, len(wire)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(wire[offset:])
			}))
			defer server.Close()
			dir := t.TempDir()
			policy := fastUpdatePolicy()
			policy.attempts = 1
			if _, err := downloadSelfUpdateBinary(context.Background(), server.Client(), dir, server.URL, "sha256", updateDigest(binary), policy); err == nil {
				t.Fatal("interrupted download succeeded")
			}
			partial, _ := os.ReadFile(filepath.Join(dir, "payload"))
			if !bytes.Equal(partial, wire[:len(wire)/2]) {
				t.Fatal("partial wire bytes not retained")
			}
			got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), dir, server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
			if err != nil || !bytes.Equal(got, binary) {
				t.Fatalf("resume = %q, %v", got, err)
			}
			got, err = downloadSelfUpdateBinary(context.Background(), server.Client(), dir, server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
			if err != nil || !bytes.Equal(got, binary) || calls.Load() != 2 {
				t.Fatalf("verified cache downloaded again: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestUpdateRangeRecovery(t *testing.T) {
	for _, mode := range []string{"ignored", "changed object", "malformed", "wrong start", "wrong total", "wrong length", "changed etag", "changed representation", "416", "missing validator", "weak validator"} {
		t.Run(mode, func(t *testing.T) {
			binary := []byte("the complete new candidate binary")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				etag := `"v1"`
				if mode == "missing validator" {
					etag = ""
				}
				if mode == "weak validator" {
					etag = `W/"v1"`
				}
				w.Header().Set("ETag", etag)
				if n == 1 {
					wire := binary
					if mode == "changed object" {
						wire = bytes.Repeat([]byte("x"), len(binary))
					}
					interruptedUpdate(w, wire)
					return
				}
				if n == 2 {
					if mode == "missing validator" || mode == "weak validator" {
						if r.Header.Get("Range") != "" {
							t.Error("resumed without strong validator")
						}
					} else if r.Header.Get("Range") == "" {
						t.Error("missing range request")
					}
					switch mode {
					case "malformed", "wrong start", "wrong total", "wrong length", "changed etag", "changed representation":
						cr := fmt.Sprintf("bytes %d-%d/%d", len(binary)/2, len(binary)-1, len(binary))
						switch mode {
						case "malformed":
							cr = "bananas"
						case "wrong start":
							cr = fmt.Sprintf("bytes 0-%d/%d", len(binary)-1, len(binary))
						case "wrong total":
							cr = fmt.Sprintf("bytes %d-%d/%d", len(binary)/2, len(binary)-1, len(binary)+1)
						case "wrong length":
							w.Header().Set("Content-Length", "1")
						case "changed etag":
							w.Header().Set("ETag", `"v2"`)
						case "changed representation":
							w.Header().Set("Content-Type", "application/zstd")
						}
						w.Header().Set("Content-Range", cr)
						w.WriteHeader(http.StatusPartialContent)
						return
					case "416":
						w.Header().Set("Content-Range", "bytes */2")
						w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
						return
					case "changed object":
						w.Header().Set("ETag", `"v2"`)
					}
				} else if r.Header.Get("Range") != "" {
					t.Error("did not discard invalid partial")
				}
				_, _ = w.Write(binary)
			}))
			defer server.Close()
			got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
			if err != nil || !bytes.Equal(got, binary) {
				t.Fatalf("range recovery: %q, %v", got, err)
			}
			if calls.Load() > 3 {
				t.Fatalf("unexpected retries: %d", calls.Load())
			}
		})
	}
}

func TestUpdateRetryBackoffAndHTTPClassification(t *testing.T) {
	for _, status := range []int{408, 425, 429, 500, 502, 503, 504, 400, 401, 403, 404, 410, 501, 505} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(status) }))
			defer server.Close()
			policy := fastUpdatePolicy()
			policy.attempts = 8
			var waits []time.Duration
			policy.sleep = func(_ context.Context, delay time.Duration) error { waits = append(waits, delay); return nil }
			_, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", strings.Repeat("0", 64), policy)
			if err == nil {
				t.Fatal("HTTP failure succeeded")
			}
			retry := status == 408 || status == 425 || status == 429 || (status >= 500 && status != 501 && status != 505)
			if retry {
				want := []time.Duration{2500 * time.Millisecond, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
				if calls.Load() != 8 || !reflect.DeepEqual(waits, want) {
					t.Fatalf("calls=%d waits=%v", calls.Load(), waits)
				}
			} else if calls.Load() != 1 || len(waits) != 0 {
				t.Fatalf("permanent failure retried: calls=%d waits=%v", calls.Load(), waits)
			}
		})
	}
}

func TestUpdateRetryAfterAndBudget(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, value := range []string{"30", now.Add(30 * time.Second).Format(http.TimeFormat), "600", "invalid"} {
		t.Run(value, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", value)
				w.WriteHeader(503)
			}))
			defer server.Close()
			policy := fastUpdatePolicy()
			clock := now
			policy.now = func() time.Time { return clock }
			policy.budget = time.Minute
			var waits []time.Duration
			policy.sleep = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				clock = clock.Add(delay)
				return nil
			}
			_, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", strings.Repeat("0", 64), policy)
			if err == nil {
				t.Fatal("budget exhaustion succeeded")
			}
			if value == "600" {
				if calls.Load() != 1 || len(waits) != 0 {
					t.Fatal("retried before long Retry-After expired")
				}
			} else if value != "invalid" && (len(waits) == 0 || waits[0] != 30*time.Second) {
				t.Fatalf("Retry-After waits=%v", waits)
			}
			var sum time.Duration
			for _, delay := range waits {
				sum += delay
			}
			if sum >= policy.budget {
				t.Fatal("waits exceeded budget")
			}
		})
	}
}

func TestUpdateCancellationDuringBodyPreservesPartial(t *testing.T) {
	binary := []byte("verified candidate executable")
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(binary)))
		_, _ = w.Write(binary[:4])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	policy := fastUpdatePolicy()
	// Cancel only once the client has read and persisted actual progress.
	resets := 0
	policy.afterFunc = func(_ time.Duration, _ func()) updateWatchdog {
		return &fakeUpdateWatchdog{reset: func() {
			resets++
			if resets > 1 {
				cancel()
			}
		}}
	}
	_, err := downloadSelfUpdateBinary(ctx, server.Client(), dir, server.URL, "sha256", updateDigest(binary), policy)
	<-started
	if err == nil {
		t.Fatal("canceled transfer succeeded")
	}
	partial, _ := os.ReadFile(filepath.Join(dir, "payload"))
	if string(partial) != string(binary[:4]) {
		t.Fatalf("lost canceled partial: %q", partial)
	}
}

type fakeUpdateWatchdog struct {
	reset func()
	stop  func()
}

func (w *fakeUpdateWatchdog) Reset(time.Duration) bool {
	if w.reset != nil {
		w.reset()
	}
	return true
}
func (w *fakeUpdateWatchdog) Stop() bool {
	if w.stop != nil {
		w.stop()
	}
	return true
}

func TestUpdateStallWatchdogAndProgress(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprint(stall), func(t *testing.T) {
			binary := []byte("candidate executable")
			expire := make(chan func(), 1)
			var resets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stall {
					(<-expire)()
					<-r.Context().Done()
					return
				}
				_, _ = w.Write(binary)
			}))
			defer server.Close()
			policy := fastUpdatePolicy()
			policy.attempts = 1
			policy.afterFunc = func(delay time.Duration, callback func()) updateWatchdog {
				if delay != selfUpdateStallTimeout {
					t.Errorf("stall timeout=%s", delay)
				}
				if stall {
					expire <- callback
				}
				return &fakeUpdateWatchdog{reset: func() { resets.Add(1) }}
			}
			_, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", updateDigest(binary), policy)
			if stall && err == nil {
				t.Fatal("stalled transfer succeeded")
			}
			if !stall && (err != nil || resets.Load() == 0) {
				t.Fatalf("progress did not reset watchdog: %v resets=%d", err, resets.Load())
			}
		})
	}
}

func TestUpdateCancellationDuringBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	policy := fastUpdatePolicy()
	policy.sleep = func(ctx context.Context, delay time.Duration) error { cancel(); return waitForUpdateRetry(ctx, delay) }
	_, err := downloadSelfUpdateBinary(ctx, server.Client(), t.TempDir(), server.URL, "sha256", strings.Repeat("0", 64), policy)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestUpdateCacheIdentityStalenessAndCorruption(t *testing.T) {
	for _, mode := range []string{"url", "checksum", "stale", "corrupt metadata", "corrupt candidate", "oversized", "nonregular"} {
		t.Run(mode, func(t *testing.T) {
			binary := []byte("candidate")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write(binary) }))
			defer server.Close()
			dir := t.TempDir()
			url, checksum := server.URL, updateDigest(binary)
			state := updateDownloadState{Identity: updateIdentity(url, "sha256", checksum), Complete: true, Total: int64(len(binary)), Updated: time.Now()}
			mustWriteFile(t, filepath.Join(dir, "payload"), binary, 0o600)
			switch mode {
			case "url":
				state.Identity = updateIdentity(url+"/other", "sha256", checksum)
			case "checksum":
				state.Identity = updateIdentity(url, "sha256", strings.Repeat("0", 64))
			case "stale":
				state.Updated = time.Now().Add(-selfUpdateCacheTTL - time.Hour)
			case "corrupt candidate":
				mustWriteFile(t, filepath.Join(dir, "payload"), bytes.Repeat([]byte("x"), len(binary)), 0o600)
			case "oversized":
				file, _ := os.OpenFile(filepath.Join(dir, "payload"), os.O_WRONLY, 0o600)
				if err := file.Truncate(selfUpdateMaxWireBytes + 1); err != nil {
					t.Fatal(err)
				}
				file.Close()
			case "nonregular":
				os.Remove(filepath.Join(dir, "payload"))
				os.Mkdir(filepath.Join(dir, "payload"), 0o700)
			}
			if err := writeUpdateJSON(filepath.Join(dir, "download.json"), state); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt metadata" {
				mustWriteFile(t, filepath.Join(dir, "download.json"), []byte("{"), 0o600)
			}
			got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), dir, url, "sha256", checksum, fastUpdatePolicy())
			if mode == "corrupt candidate" {
				if err == nil || calls.Load() != 0 {
					t.Fatal("corrupt cached candidate must be discarded without a blind redownload")
				}
			} else if err != nil || !bytes.Equal(got, binary) || calls.Load() != 1 {
				t.Fatalf("invalid cache recovery: %q %v calls=%d", got, err, calls.Load())
			}
		})
	}
}

func TestUpdateRejectsChecksumAndOversizedResponsesWithoutRetry(t *testing.T) {
	for _, mode := range []string{"checksum", "encoded", "oversized", "decoded limit"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				switch mode {
				case "encoded":
					w.Header().Set("Content-Encoding", "gzip")
				case "oversized":
					w.Header().Set("Content-Length", fmt.Sprint(selfUpdateMaxWireBytes+1))
					return
				case "decoded limit":
					w.Header().Set("Content-Type", "application/zstd")
					encoder, _ := zstd.NewWriter(w, zstd.WithWindowSize(1024*1024))
					_, _ = io.CopyN(encoder, zeroUpdateReader{}, selfUpdateMaxBytes+1)
					encoder.Close()
					return
				}
				_, _ = w.Write([]byte("wrong checksum"))
			}))
			defer server.Close()
			_, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", strings.Repeat("0", 64), fastUpdatePolicy())
			if err == nil || calls.Load() != 1 {
				t.Fatalf("invalid response retried or accepted: %v calls=%d", err, calls.Load())
			}
		})
	}
}

type zeroUpdateReader struct{}

func (zeroUpdateReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestUpdate416VerifiesCompleteWireBytes(t *testing.T) {
	binary := []byte("complete binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", len(binary)) {
			t.Error("missing completion range")
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(binary)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer server.Close()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "payload"), binary, 0o600)
	state := updateDownloadState{Identity: updateIdentity(server.URL, "sha256", updateDigest(binary)), URL: server.URL, ETag: `"v1"`, Total: int64(len(binary)), Updated: time.Now()}
	if err := writeUpdateJSON(filepath.Join(dir, "download.json"), state); err != nil {
		t.Fatal(err)
	}
	got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), dir, server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("416 completion: %q %v", got, err)
	}
}

func TestUpdateProgressCanContinueBeyondFiveMinutes(t *testing.T) {
	binary := []byte("a slowly progressing executable")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(binary) }))
	defer server.Close()
	policy := fastUpdatePolicy()
	clock := time.Now()
	start := clock
	policy.now = func() time.Time { return clock }
	policy.afterFunc = func(_ time.Duration, _ func()) updateWatchdog {
		return &fakeUpdateWatchdog{reset: func() { clock = clock.Add(10 * time.Minute) }}
	}
	transport := server.Client().Transport
	client := &http.Client{Transport: updateTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) < time.Hour {
			t.Error("active attempt has a short wall-clock cutoff")
		}
		return transport.RoundTrip(request)
	})}
	got, err := downloadSelfUpdateBinary(context.Background(), client, t.TempDir(), server.URL, "sha256", updateDigest(binary), policy)
	if err != nil || !bytes.Equal(got, binary) || clock.Sub(start) < 20*time.Minute {
		t.Fatalf("slow progress: %q %v elapsed=%s", got, err, clock.Sub(start))
	}
}

type updateTestRoundTripper func(*http.Request) (*http.Response, error)

func (f updateTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateChangedRedirectTargetDoesNotAppend(t *testing.T) {
	binary := []byte("the new complete binary")
	var entries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"same"`)
		if r.URL.Path == "/other" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", len(binary)/2, len(binary)-1, len(binary)))
			w.WriteHeader(206)
			_, _ = w.Write(binary[len(binary)/2:])
			return
		}
		switch entries.Add(1) {
		case 1:
			interruptedUpdate(w, bytes.Repeat([]byte("x"), len(binary)))
		case 2:
			http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
		default:
			if r.Header.Get("Range") != "" {
				t.Error("changed final URL did not discard partial")
			}
			_, _ = w.Write(binary)
		}
	}))
	defer server.Close()
	got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
	if err != nil || !bytes.Equal(got, binary) || entries.Load() != 3 {
		t.Fatalf("changed redirect: %q %v entries=%d", got, err, entries.Load())
	}
}

func TestUpdateCompressedToRawFallbackReplacesPartial(t *testing.T) {
	binary := bytes.Repeat([]byte("candidate binary"), 100)
	encoder, _ := zstd.NewWriter(nil)
	wire := encoder.EncodeAll(binary, nil)
	encoder.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"compressed"`)
			w.Header().Set("Content-Type", "application/zstd")
			interruptedUpdate(w, wire)
			return
		}
		if r.Header.Get("If-Range") != `"compressed"` {
			t.Error("missing compressed validator")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"raw"`)
		_, _ = w.Write(binary)
	}))
	defer server.Close()
	got, err := downloadSelfUpdateBinary(context.Background(), server.Client(), t.TempDir(), server.URL, "sha256", updateDigest(binary), fastUpdatePolicy())
	if err != nil || !bytes.Equal(got, binary) || calls.Load() != 2 {
		t.Fatalf("raw fallback: %v calls=%d", err, calls.Load())
	}
}

func TestUpdateBudgetExhaustionRetainsPartial(t *testing.T) {
	binary := []byte("a complete executable candidate")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"v1"`)
		interruptedUpdate(w, binary)
	}))
	defer server.Close()
	policy := fastUpdatePolicy()
	policy.budget = time.Second
	dir := t.TempDir()
	_, err := downloadSelfUpdateBinary(context.Background(), server.Client(), dir, server.URL, "sha256", updateDigest(binary), policy)
	partial, _ := os.ReadFile(filepath.Join(dir, "payload"))
	if err == nil || !strings.Contains(err.Error(), "budget") || calls.Load() != 1 || !bytes.Equal(partial, binary[:len(binary)/2]) {
		t.Fatalf("budget: %v calls=%d partial=%q", err, calls.Load(), partial)
	}
}

func TestDefaultUpdateJitterIsBounded(t *testing.T) {
	policy := defaultUpdateDownloadPolicy()
	for i := 0; i < 100; i++ {
		if got := policy.jitter(time.Minute); got < 30*time.Second || got > time.Minute {
			t.Fatalf("unbounded jitter: %s", got)
		}
	}
}
