package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"radio-gaga/internal/utils"
)

const (
	selfUpdateDownloadBudget = 2 * time.Hour
	selfUpdateStallTimeout   = 90 * time.Second
	selfUpdateMaxWireBytes   = selfUpdateMaxBytes + 1024*1024
	selfUpdateCacheTTL       = 7 * 24 * time.Hour
	selfUpdateAccept         = "application/zstd, application/octet-stream"
)

type updateDownloadPolicy struct {
	budget, stall time.Duration
	attempts      int
	now           func() time.Time
	sleep         func(context.Context, time.Duration) error
	jitter        func(time.Duration) time.Duration
	afterFunc     func(time.Duration, func()) updateWatchdog
}

type updateWatchdog interface {
	Reset(time.Duration) bool
	Stop() bool
}

func defaultUpdateDownloadPolicy() updateDownloadPolicy {
	return updateDownloadPolicy{
		budget: selfUpdateDownloadBudget, stall: selfUpdateStallTimeout, attempts: 6,
		now: time.Now, sleep: waitForUpdateRetry,
		afterFunc: func(delay time.Duration, expire func()) updateWatchdog { return time.AfterFunc(delay, expire) },
		jitter:    func(cap time.Duration) time.Duration { return cap/2 + time.Duration(rand.Int64N(int64(cap/2)+1)) },
	}
}

func waitForUpdateRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func updateHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Device clocks may be invalid; preserve existing checksum-based update verification.
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout: 90 * time.Second, DisableCompression: true, MaxResponseHeaderBytes: 32 * 1024,
	}}
}

type updateDownloadState struct {
	Identity string    `json:"identity"`
	ETag     string    `json:"etag"`
	URL      string    `json:"url"`
	Zstd     bool      `json:"zstd"`
	Total    int64     `json:"total"`
	Complete bool      `json:"complete"`
	Updated  time.Time `json:"updated"`
}

func updateIdentity(url, algorithm, checksum string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + algorithm + ":" + strings.ToLower(checksum) + "\x00" + selfUpdateAccept + "\x00identity"))
	return hex.EncodeToString(sum[:])
}

func validateUpdateChecksum(algorithm, checksum string) error {
	h, err := utils.CreateHash(algorithm)
	if err != nil {
		return err
	}
	decoded, err := hex.DecodeString(checksum)
	if err != nil || len(decoded) != h.Size() {
		return fmt.Errorf("invalid %s checksum", algorithm)
	}
	return nil
}

// The cache holds one wire representation, never decoded fragments. A strong
// validator is required to append bytes; the final checksum always covers the
// decoded executable. Callers hold the self-update lock for the cache lifetime.
func downloadSelfUpdateBinary(ctx context.Context, client *http.Client, dir, url, algorithm, checksum string, policy updateDownloadPolicy) ([]byte, error) {
	if err := validateUpdateChecksum(algorithm, checksum); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil || len(url) > 8192 || (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Host == "" {
		return nil, fmt.Errorf("invalid update URL")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, policy.budget)
	defer cancel()
	deadline := policy.now().Add(policy.budget)
	payloadPath, statePath := filepath.Join(dir, "payload"), filepath.Join(dir, "download.json")
	identity := updateIdentity(url, algorithm, checksum)
	state := updateDownloadState{}
	clear := func() error {
		state = updateDownloadState{}
		for _, path := range []string{statePath, payloadPath} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	stateErr := readUpdateJSON(statePath, &state, 64*1024)
	info, statErr := os.Lstat(payloadPath)
	if stateErr != nil || state.Identity != identity || policy.now().Sub(state.Updated) > selfUpdateCacheTTL || statErr != nil || !info.Mode().IsRegular() || info.Size() > selfUpdateMaxWireBytes || (state.Total >= 0 && info.Size() > state.Total) || (state.Complete && info.Size() != state.Total) {
		if err := clear(); err != nil {
			return nil, err
		}
	}
	verify := func() ([]byte, error) {
		binary, err := verifyUpdatePayload(payloadPath, state.Zstd, algorithm, checksum)
		if err != nil {
			if clearErr := clear(); clearErr != nil {
				return nil, fmt.Errorf("%w (discard cache: %v)", err, clearErr)
			}
		}
		return binary, err
	}
	if state.Complete {
		binary, err := verify()
		if err == nil {
			err = ctx.Err()
		}
		return binary, err
	}

	var lastErr error
	for attempt := 0; attempt < policy.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := deadline.Sub(policy.now())
		if remaining <= 0 {
			return nil, fmt.Errorf("self-update download budget exhausted: %w", context.DeadlineExceeded)
		}
		// Every attempt has a deadline and a no-progress watchdog. There is no
		// throughput threshold or short wall-clock cutoff for active transfers.
		attemptCtx, attemptCancel := context.WithTimeout(ctx, remaining)
		binary, retry, retryAfter, attemptErr := downloadUpdateAttempt(attemptCtx, attemptCancel, client, request, payloadPath, statePath, &state, identity, algorithm, checksum, policy)
		attemptCancel()
		if attemptErr == nil {
			return binary, ctx.Err()
		}
		lastErr = attemptErr
		if !retry || attempt+1 == policy.attempts {
			break
		}
		cap := min(5*time.Second*time.Duration(1<<attempt), 2*time.Minute)
		delay := max(policy.jitter(cap), retryAfter)
		if delay >= deadline.Sub(policy.now()) {
			return nil, fmt.Errorf("self-update retry exceeds download budget: %w", lastErr)
		}
		log.Printf("Self-update download attempt %d failed: %v; retrying in %s", attempt+1, lastErr, delay)
		if err := policy.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("self-update download failed: %w", lastErr)
}

func downloadUpdateAttempt(ctx context.Context, cancel context.CancelFunc, client *http.Client, template *http.Request, payloadPath, statePath string, state *updateDownloadState, identity, algorithm, checksum string, policy updateDownloadPolicy) ([]byte, bool, time.Duration, error) {
	offset := int64(0)
	if info, err := os.Stat(payloadPath); err == nil && state.Identity == identity && strongUpdateETag(state.ETag) {
		offset = info.Size()
	}
	req := template.Clone(ctx)
	req.Header.Set("Accept", selfUpdateAccept)
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", state.ETag)
	}
	watchdog := policy.afterFunc(policy.stall, cancel)
	defer watchdog.Stop()
	response, err := client.Do(req)
	if err != nil {
		var networkErr net.Error
		retry := errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		return nil, retry, 0, fmt.Errorf("download request: %w", err)
	}
	defer response.Body.Close()
	watchdog.Reset(policy.stall)
	retryAfter := updateRetryAfter(response.Header.Get("Retry-After"), policy.now())
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusTooEarly || (response.StatusCode >= 500 && response.StatusCode != 501 && response.StatusCode != 505) {
		return nil, retryAfter <= 5*time.Minute, retryAfter, fmt.Errorf("download HTTP %d", response.StatusCode)
	}
	reset := func() error {
		*state = updateDownloadState{}
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(payloadPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	protocolFailure := func(message string) ([]byte, bool, time.Duration, error) {
		if err := reset(); err != nil {
			return nil, false, 0, err
		}
		return nil, true, 0, errors.New(message)
	}
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
		contentRange := response.Header.Get("Content-Range")
		total, parseErr := strconv.ParseInt(strings.TrimPrefix(contentRange, "bytes */"), 10, 64)
		if strings.HasPrefix(contentRange, "bytes */") && parseErr == nil && total == offset && response.Header.Get("ETag") == state.ETag && response.Request.URL.String() == state.URL {
			if binary, err := verifyUpdatePayload(payloadPath, state.Zstd, algorithm, checksum); err == nil {
				state.Total, state.Complete, state.Updated = total, true, policy.now()
				return binary, false, 0, writeUpdateJSON(statePath, state)
			}
		}
		return protocolFailure("range no longer satisfiable; restarting representation")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return nil, false, 0, fmt.Errorf("download HTTP %d", response.StatusCode)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, false, 0, fmt.Errorf("unsupported update Content-Encoding %q", encoding)
	}
	if len(response.Request.URL.String()) > 8192 {
		return nil, false, 0, fmt.Errorf("update response URL exceeds size limit")
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	compressed := mediaType == "application/zstd"
	total := response.ContentLength
	segmentLength := response.ContentLength
	if response.StatusCode == http.StatusPartialContent {
		start, end, size, valid := parseUpdateContentRange(response.Header.Get("Content-Range"))
		if offset == 0 || !valid || start != offset || size > selfUpdateMaxWireBytes || (state.Total >= 0 && size != state.Total) || (response.ContentLength >= 0 && response.ContentLength != end-start+1) || compressed != state.Zstd || response.Header.Get("ETag") != state.ETag || response.Request.URL.String() != state.URL {
			return protocolFailure("invalid or changed update range; restarting representation")
		}
		total, segmentLength = size, end-start+1
	} else {
		// A server may ignore Range or fail If-Range. Never append a 200.
		offset = 0
		if err := reset(); err != nil {
			return nil, false, 0, err
		}
		*state = updateDownloadState{Identity: identity, ETag: response.Header.Get("ETag"), URL: response.Request.URL.String(), Zstd: compressed, Total: total}
		if len(state.ETag) > 1024 {
			state.ETag = ""
		}
	}
	if total > selfUpdateMaxWireBytes {
		return nil, false, 0, fmt.Errorf("update representation exceeds %d-byte limit", selfUpdateMaxWireBytes)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(payloadPath, flags, 0o600)
	if err != nil {
		return nil, false, 0, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, false, 0, err
	}
	state.Updated = policy.now()
	if err := writeUpdateJSON(statePath, state); err != nil {
		return nil, false, 0, err
	}
	reader := &updateProgressReader{Reader: response.Body, progress: func() { watchdog.Reset(policy.stall) }}
	readLimit := int64(selfUpdateMaxWireBytes) - offset + 1
	if segmentLength >= 0 {
		readLimit = min(readLimit, segmentLength+1)
	}
	n, copyErr := io.Copy(file, io.LimitReader(reader, readLimit))
	watchdog.Stop()
	if err := file.Sync(); err != nil {
		return nil, false, 0, err
	}
	if n+offset > selfUpdateMaxWireBytes || (segmentLength >= 0 && n > segmentLength) {
		_ = reset()
		return nil, false, 0, fmt.Errorf("update representation exceeds declared size or %d-byte limit", selfUpdateMaxWireBytes)
	}
	if copyErr != nil {
		var pathErr *os.PathError
		return nil, !errors.As(copyErr, &pathErr), 0, fmt.Errorf("download body: %w", copyErr)
	}
	if total >= 0 && n+offset != total {
		return nil, true, 0, fmt.Errorf("truncated update: have %d of %d bytes", n+offset, total)
	}
	state.Total, state.Complete, state.Updated = n+offset, true, policy.now()
	binary, err := verifyUpdatePayload(payloadPath, state.Zstd, algorithm, checksum)
	if err != nil {
		_ = reset()
		return nil, false, 0, err
	}
	if err := writeUpdateJSON(statePath, state); err != nil {
		return nil, false, 0, err
	}
	return binary, false, 0, nil
}

type updateProgressReader struct {
	io.Reader
	progress func()
}

func (r *updateProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.progress()
	}
	return n, err
}

func strongUpdateETag(value string) bool {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, ch := range []byte(value[1 : len(value)-1]) {
		if ch < 0x21 || ch == '"' || ch == 0x7f {
			return false
		}
	}
	return true
}

func parseUpdateContentRange(value string) (start, end, total int64, valid bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return
	}
	var err error
	if start, err = strconv.ParseInt(bounds[0], 10, 64); err != nil {
		return
	}
	if end, err = strconv.ParseInt(bounds[1], 10, 64); err != nil {
		return
	}
	if total, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return
	}
	valid = start >= 0 && end >= start && total > end && total <= selfUpdateMaxWireBytes
	return
}

func updateRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(min(seconds, 301)) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), 5*time.Minute+time.Second)
	}
	return 0
}

func verifyUpdatePayload(path string, compressed bool, algorithm, checksum string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var reader io.Reader = file
	if compressed {
		decoder, err := zstd.NewReader(file, zstd.WithDecoderMaxMemory(selfUpdateMaxBytes), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("open compressed update: %w", err)
		}
		defer decoder.Close()
		reader = decoder
	}
	hasher, err := utils.CreateHash(algorithm)
	if err != nil {
		return nil, err
	}
	binary, err := io.ReadAll(io.TeeReader(io.LimitReader(reader, selfUpdateMaxBytes+1), hasher))
	if err != nil {
		return nil, fmt.Errorf("read update candidate: %w", err)
	}
	if len(binary) > selfUpdateMaxBytes {
		return nil, fmt.Errorf("new binary exceeds %d-byte limit", selfUpdateMaxBytes)
	}
	if !strings.EqualFold(fmt.Sprintf("%x", hasher.Sum(nil)), checksum) {
		return nil, fmt.Errorf("checksum mismatch")
	}
	return binary, nil
}

func readUpdateJSON(path string, value interface{}, limit int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("update state exceeds size limit")
	}
	return json.Unmarshal(data, value)
}

func writeUpdateJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
