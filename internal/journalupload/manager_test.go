package journalupload

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeReader struct {
	mu      sync.Mutex
	entries []map[string]string
	cursor  string
	idx     int
}

func (f *fakeReader) SeekCursor(c string) error         { return nil }
func (f *fakeReader) TestCursor(c string) (bool, error) { return c == f.cursor, nil }
func (f *fakeReader) SeekHead() error                   { f.idx = 0; return nil }
func (f *fakeReader) AddBootMatch(bootID string) error  { return nil }
func (f *fakeReader) Next() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.entries) {
		return false, nil
	}
	f.idx++
	return true, nil
}
func (f *fakeReader) Entry() (map[string]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.entries[f.idx-1]
	return r, r["__CURSOR"], nil
}
func (f *fakeReader) Wait(d time.Duration) {}
func (f *fakeReader) Close() error         { return nil }

func newFakeReader(entries []map[string]string) *fakeReader {
	return &fakeReader{entries: entries}
}

func TestManagerStreamsAndUploadsBatch(t *testing.T) {
	var bodies [][]byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	state := &State{Dir: t.TempDir()}
	_ = state.WriteSession(Session{URL: srv.URL, Username: "1", Password: "p"})

	reader := newFakeReader([]map[string]string{
		{"__CURSOR": "s=1;i=1", "MESSAGE": "hello"},
	})

	m := &Manager{
		State:       state,
		NewReader:   func() (journalReader, error) { return reader, nil },
		BatchSizeKB: 1,
		FlushEvery:  50 * time.Millisecond,
		Client:      srv.Client(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no upload happened")
	}
	if !bytes.Contains(bodies[0], []byte("MESSAGE=hello")) {
		t.Fatalf("body missing MESSAGE: %q", bodies[0])
	}

	got, _, _ := state.ReadCursor()
	if got != "s=1;i=1" {
		t.Fatalf("cursor=%q", got)
	}
}

func TestManagerEmitsWarningOnCursorMiss(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	state := &State{Dir: t.TempDir()}
	_ = state.WriteSession(Session{URL: srv.URL, Username: "1", Password: "p"})
	_ = state.WriteCursor("stale-cursor-not-in-journal")

	reader := newFakeReader([]map[string]string{
		{"__CURSOR": "s=1;i=1", "MESSAGE": "real log"},
	})
	reader.cursor = "something-else" // so TestCursor returns false

	m := &Manager{
		State:      state,
		NewReader:  func() (journalReader, error) { return reader, nil },
		FlushEvery: 50 * time.Millisecond,
		Client:     srv.Client(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	if !bytes.Contains(body, []byte("cursor unresolvable")) {
		t.Fatalf("warning not emitted: %q", body)
	}
}

func TestManagerStopsOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	state := &State{Dir: t.TempDir()}
	_ = state.WriteSession(Session{URL: srv.URL, Username: "1", Password: "p"})

	reader := newFakeReader([]map[string]string{
		{"__CURSOR": "s=1;i=1", "MESSAGE": "x"},
	})

	m := &Manager{
		State:      state,
		NewReader:  func() (journalReader, error) { return reader, nil },
		FlushEvery: 50 * time.Millisecond,
		Client:     srv.Client(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	if _, ok, _ := state.ReadSession(); ok {
		t.Error("session file should have been cleared after 401")
	}
}
