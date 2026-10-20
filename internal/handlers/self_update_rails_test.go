package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type railsUpdateFixture struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Checksum  string `json:"checksum"`
	Size      int    `json:"size"`
	ChangeURL string `json:"change_url"`
}

// Sunshine's opt-in RadioGagaDownloadContractTest starts the real Rails route
// under Puma with committed isolated fixtures and passes this loopback manifest.
func TestSelfUpdateRailsContract(t *testing.T) {
	manifest := os.Getenv("RADIO_GAGA_RAILS_CONTRACT")
	if manifest == "" {
		t.Skip("run Sunshine's RadioGagaDownloadContractTest with RADIO_GAGA_CHECKOUT")
	}
	var fixtures []railsUpdateFixture
	if err := json.Unmarshal([]byte(manifest), &fixtures); err != nil || len(fixtures) != 5 {
		t.Fatalf("invalid Rails fixture manifest: %v", err)
	}
	for _, fixture := range fixtures {
		assertRailsLoopbackURL(t, fixture.URL)
		if fixture.ChangeURL != "" {
			assertRailsLoopbackURL(t, fixture.ChangeURL)
		}
		t.Run(fixture.Name, func(t *testing.T) { testRailsInterruptedDownload(t, fixture) })
		if fixture.Name == "raw" || fixture.Name == "zstd" {
			t.Run(fixture.Name+"_416", func(t *testing.T) { testRailsCompletePartial(t, fixture) })
		}
	}
}

func assertRailsLoopbackURL(t *testing.T, value string) {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" || !net.ParseIP(parsed.Hostname()).IsLoopback() {
		t.Fatalf("Rails acceptance fixtures must use an explicit loopback HTTP port: %q", value)
	}
}

type railsDownloadExchange struct {
	Status                          int
	Length                          int64
	Range, IfRange, ETag, MediaType string
	ContentRange, FinalURL          string
	Bytes                           int64
}

type railsContractTransport struct {
	base           http.RoundTripper
	interrupt      bool
	wrongValidator bool
	exchanges      []*railsDownloadExchange
}

func (transport *railsContractTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.interrupt && len(transport.exchanges) == 0 {
		request = request.Clone(request.Context())
		request.Close = true
		request.Header.Set("X-Radio-Gaga-Contract-Interrupt", "half")
	}
	if transport.wrongValidator && request.Header.Get("Range") != "" {
		request = request.Clone(request.Context())
		request.Header.Set("If-Range", `"deliberately-wrong"`)
	}
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	exchange := &railsDownloadExchange{
		Status: response.StatusCode, Length: response.ContentLength,
		Range: request.Header.Get("Range"), IfRange: request.Header.Get("If-Range"),
		ETag: response.Header.Get("ETag"), MediaType: response.Header.Get("Content-Type"),
		ContentRange: response.Header.Get("Content-Range"), FinalURL: request.URL.String(),
	}
	transport.exchanges = append(transport.exchanges, exchange)
	response.Body = &railsContractBody{ReadCloser: response.Body, exchange: exchange}
	return response, nil
}

type railsContractBody struct {
	io.ReadCloser
	exchange *railsDownloadExchange
}

func (body *railsContractBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	body.exchange.Bytes += int64(n)
	return n, err
}

func railsContractClient(t *testing.T, interrupt, wrongValidator bool) (*http.Client, *railsContractTransport) {
	t.Helper()
	client := updateHTTPClient()
	transport := &railsContractTransport{base: client.Transport, interrupt: interrupt, wrongValidator: wrongValidator}
	t.Cleanup(client.CloseIdleConnections)
	return &http.Client{Transport: transport}, transport
}

func testRailsInterruptedDownload(t *testing.T, fixture railsUpdateFixture) {
	dir := t.TempDir()
	policy := fastUpdatePolicy()
	policy.budget = 30 * time.Second
	policy.attempts = 1
	client, interrupted := railsContractClient(t, true, false)
	_, err := downloadSelfUpdateBinary(context.Background(), client, dir, fixture.URL, "sha256", fixture.Checksum, policy)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(interrupted.exchanges) != 1 {
		t.Fatalf("actual Rails TCP body was not truncated: %v responses=%+v", err, interrupted.exchanges)
	}
	first := interrupted.exchanges[0]
	if first.Status != http.StatusOK || !strongUpdateETag(first.ETag) || first.FinalURL != fixture.URL || first.Length < 2 {
		t.Fatalf("Rails initial response lacks a stable wire identity: %+v", first)
	}
	partial, err := os.ReadFile(filepath.Join(dir, "payload"))
	if err != nil || int64(len(partial)) != first.Length/2 || first.Bytes != int64(len(partial)) {
		t.Fatalf("interrupted wire bytes not preserved: %v partial=%d exchange=%+v", err, len(partial), first)
	}
	if fixture.ChangeURL != "" {
		request, err := http.NewRequest(http.MethodPost, fixture.ChangeURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("isolated Rails fixture mutation failed: HTTP %d", response.StatusCode)
		}
	}
	// A new downloader/client instance consumes only persisted partial state.
	client, resumed := railsContractClient(t, false, fixture.Name == "wrong_validator")
	policy.attempts = 3
	binary, err := downloadSelfUpdateBinary(context.Background(), client, dir, fixture.URL, "sha256", fixture.Checksum, policy)
	if err != nil || len(binary) != fixture.Size || len(resumed.exchanges) != 1 {
		t.Fatalf("actual Go/Rails resumption failed: %v size=%d responses=%+v", err, len(binary), resumed.exchanges)
	}
	second := resumed.exchanges[0]
	if second.Range != fmt.Sprintf("bytes=%d-", len(partial)) || second.FinalURL != fixture.URL {
		t.Fatalf("persisted partial was not requested at the stable Rails route: %+v", second)
	}
	if fixture.Name == "raw" || fixture.Name == "zstd" {
		if second.Status != http.StatusPartialContent || second.IfRange != first.ETag || second.ETag != first.ETag || second.Length != first.Length-int64(len(partial)) || first.Bytes+second.Bytes != first.Length {
			t.Fatalf("Rails did not transfer exactly the remaining wire bytes: first=%+v second=%+v", first, second)
		}
		if fixture.Name == "zstd" && !strings.HasPrefix(second.MediaType, "application/zstd") {
			t.Fatalf("compressed wire representation lost: %+v", second)
		}
	} else {
		if second.Status != http.StatusOK || second.ContentRange != "" {
			t.Fatalf("changed/mismatched validator must send full 200, not append: %+v", second)
		}
		if fixture.ChangeURL != "" && first.ETag == second.ETag {
			t.Fatalf("changed wire object reused its validator: first=%+v second=%+v", first, second)
		}
		if fixture.Name == "changed_representation" && !strings.HasPrefix(second.MediaType, "application/zstd") {
			t.Fatalf("actual raw-to-zstd change was not exercised: %+v", second)
		}
	}
	_, err = downloadSelfUpdateBinary(context.Background(), client, dir, fixture.URL, "sha256", fixture.Checksum, policy)
	if err != nil || len(resumed.exchanges) != 1 {
		t.Fatalf("verified candidate downloaded again: %v responses=%d", err, len(resumed.exchanges))
	}
	t.Logf("actual Rails %s: HTTP %d -> %d, interrupted=%d wire bytes, subsequent=%d, final checksum verified", fixture.Name, first.Status, second.Status, first.Bytes, second.Bytes)
}

func testRailsCompletePartial(t *testing.T, fixture railsUpdateFixture) {
	client, _ := railsContractClient(t, false, false)
	request, err := http.NewRequest(http.MethodGet, fixture.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", selfUpdateAccept)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("read actual Rails seed representation: %v HTTP %d", err, response.StatusCode)
	}
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "payload"), wire, 0o600)
	state := updateDownloadState{
		Identity: updateIdentity(fixture.URL, "sha256", fixture.Checksum), URL: fixture.URL,
		ETag: response.Header.Get("ETag"), Zstd: strings.HasPrefix(response.Header.Get("Content-Type"), "application/zstd"),
		Total: int64(len(wire)), Complete: false, Updated: time.Now(),
	}
	if err := writeUpdateJSON(filepath.Join(dir, "download.json"), state); err != nil {
		t.Fatal(err)
	}
	client, transport := railsContractClient(t, false, false)
	binary, err := downloadSelfUpdateBinary(context.Background(), client, dir, fixture.URL, "sha256", fixture.Checksum, fastUpdatePolicy())
	if err != nil || len(binary) != fixture.Size || len(transport.exchanges) != 1 || transport.exchanges[0].Status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("actual Rails 416 completion failed: %v responses=%+v", err, transport.exchanges)
	}
	t.Logf("actual Rails %s: HTTP 416 verified %d cached wire bytes without retransferring the payload", fixture.Name, len(wire))
}
