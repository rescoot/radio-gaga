package journalupload

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUploadSuccess(t *testing.T) {
	var gotBody []byte
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := &Uploader{
		URL:      srv.URL,
		Username: "7",
		Password: "secret",
		Client:   srv.Client(),
	}
	body := []byte("MESSAGE=hi\n\n")
	err := u.Upload(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body mismatch")
	}
	if gotUser != "7" || gotPass != "secret" {
		t.Fatalf("basic auth mismatch: %q:%q", gotUser, gotPass)
	}
}

func TestUpload401ReturnsErrSessionInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	u := &Uploader{URL: srv.URL, Client: srv.Client()}
	err := u.Upload(context.Background(), []byte{})
	if err != ErrSessionInvalid {
		t.Fatalf("want ErrSessionInvalid, got %v", err)
	}
}

func TestUpload5xxRetriesThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := &Uploader{
		URL:     srv.URL,
		Client:  srv.Client(),
		Backoff: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
	}
	err := u.Upload(context.Background(), []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}
