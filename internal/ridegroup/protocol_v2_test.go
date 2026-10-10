package ridegroup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestV2FixtureCorpus(t *testing.T) {
	files, err := os.ReadDir("testdata/v2")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		count++
		data, err := os.ReadFile(filepath.Join("testdata/v2", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		_, parseErr := ParseV2Message(data)
		wantInvalid := strings.HasPrefix(file.Name(), "invalid-")
		if wantInvalid && parseErr == nil {
			t.Errorf("%s parsed, want rejection", file.Name())
		}
		if !wantInvalid && parseErr != nil {
			t.Errorf("%s rejected: %v", file.Name(), parseErr)
		}
	}
	if count != 60 {
		t.Fatalf("v2 fixture count = %d, want 60", count)
	}
}

func TestV2FixtureChecksumsAndV1BytePreservation(t *testing.T) {
	verifyFixtureManifest(t, "testdata/v2/SHA256SUMS", "testdata/v2", 60)
	v1Manifest := "testdata/v2/V1-SHA256SUMS"
	data, err := os.ReadFile(v1Manifest)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 24 {
		t.Fatalf("v1 fixture manifest entries = %d, want 24", len(lines))
	}
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			t.Fatalf("invalid v1 manifest line %q", line)
		}
		name := strings.TrimPrefix(parts[1], "*")
		current, err := os.ReadFile(filepath.Join("testdata/v1", name))
		if err != nil {
			t.Fatalf("read v1 fixture %s: %v", name, err)
		}
		sum := sha256.Sum256(current)
		if hex.EncodeToString(sum[:]) != parts[0] {
			t.Errorf("v1 fixture %s checksum differs", name)
		}
	}
}

func TestEncodeControlV2UsesCanonicalStringsAndExactBoundaries(t *testing.T) {
	canonicalInput, err := os.ReadFile("testdata/v2/control-canonical-strings.json")
	if err != nil {
		t.Fatal(err)
	}
	control, err := ParseControlV2(canonicalInput)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeControlV2(control)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, canonicalInput) {
		t.Fatalf("canonical control differs\n got: %s\nwant: %s", encoded, canonicalInput)
	}

	maxEmptyBytes, err := os.ReadFile("testdata/v2/control-grant-max-empty.json")
	if err != nil {
		t.Fatal(err)
	}
	maxEmpty, err := ParseControlV2(maxEmptyBytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxEmpty.Peers {
		maxEmpty.Peers[i].Label = "AAAAAAAAAA"
	}
	fitted, err := FitControlLabelsV2(maxEmpty)
	if err != nil {
		t.Fatal(err)
	}
	fittedBytes, err := EncodeControlV2(fitted)
	if err != nil {
		t.Fatal(err)
	}
	maxFitBytes, err := os.ReadFile("testdata/v2/control-grant-max-label-fit.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fittedBytes, maxFitBytes) {
		t.Fatalf("maximal fitted control differs\n got: %s\nwant: %s", fittedBytes, maxFitBytes)
	}
	if len(fittedBytes) != 1024 {
		t.Fatalf("maximal control bytes = %d, want 1024", len(fittedBytes))
	}
}

func TestV2CanonicalUnicodeAndScalarValidation(t *testing.T) {
	input := []byte(`"<>& 😀\\u2028"`)
	encoded, err := CanonicalV2String(stringValue(t, input))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`"<>& 😀\\u2028"`)
	if !bytes.Equal(encoded, want) {
		t.Fatalf("canonical string = %q, want %q", encoded, want)
	}
	if validJSONScalars([]byte(`"\ud800"`)) {
		t.Fatal("unpaired escaped surrogate accepted")
	}
	if !validJSONScalars([]byte(`"\ud83d\ude03"`)) {
		t.Fatal("valid escaped surrogate pair rejected")
	}
}

func stringValue(t *testing.T, data []byte) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestLocalStopV2AttemptCorrelationIsBoundedAndExact(t *testing.T) {
	data, err := os.ReadFile("testdata/v2/local-stop-result-committed.json")
	if err != nil {
		t.Fatal(err)
	}
	result, err := ParseLocalStopResultV2(data)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 10, 5, 30, 0, 0, time.UTC)
	attempt := LocalStopAttemptV2{Adapter: result.Adapter, OperationID: result.OperationID, JournalRevision: result.JournalRevision, Challenge: result.Challenge, ConnectionNonce: result.ConnectionNonce, StartedAt: started}
	if !MatchesLocalStopAttemptV2(result, attempt, result.ConnectionNonce, started.Add(10*time.Second)) {
		t.Fatal("exact ten-second response did not match")
	}
	for name, mutate := range map[string]func(*LocalStopResultV2){
		"operation":  func(v *LocalStopResultV2) { v.OperationID = "00000000-0000-4000-8000-000000000099" },
		"revision":   func(v *LocalStopResultV2) { v.JournalRevision++ },
		"challenge":  func(v *LocalStopResultV2) { v.Challenge = "00000000-0000-4000-8000-000000000099" },
		"connection": func(v *LocalStopResultV2) { v.ConnectionNonce = "00000000-0000-4000-8000-000000000099" },
	} {
		bad := result
		mutate(&bad)
		if MatchesLocalStopAttemptV2(bad, attempt, result.ConnectionNonce, started.Add(time.Second)) {
			t.Errorf("%s mismatch matched", name)
		}
	}
	if MatchesLocalStopAttemptV2(result, attempt, result.ConnectionNonce, started.Add(10*time.Second+time.Nanosecond)) {
		t.Fatal("expired response matched")
	}
	if MatchesLocalStopAttemptV2(result, attempt, "00000000-0000-4000-8000-000000000099", started.Add(time.Second)) {
		t.Fatal("response from another current connection matched")
	}
}

func verifyFixtureManifest(t *testing.T, manifest, dir string, expected int) {
	t.Helper()
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != expected {
		t.Fatalf("%s entries = %d, want %d", manifest, len(lines), expected)
	}
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			t.Fatalf("invalid manifest line %q", line)
		}
		name := strings.TrimPrefix(parts[1], "*")
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != parts[0] {
			t.Errorf("fixture %s checksum differs", name)
		}
	}
}
