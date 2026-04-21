package journalupload

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestEncodeSimpleRecord(t *testing.T) {
	var buf bytes.Buffer
	rec := map[string]string{
		"MESSAGE":       "hello",
		"_SYSTEMD_UNIT": "foo.service",
		"PRIORITY":      "6",
	}
	if err := EncodeRecord(&buf, rec); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"MESSAGE=hello\n", "_SYSTEMD_UNIT=foo.service\n", "PRIORITY=6\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("record must end with blank line: %q", got)
	}
}

func TestEncodeMultilineFieldUsesBinaryForm(t *testing.T) {
	var buf bytes.Buffer
	rec := map[string]string{"MESSAGE": "line1\nline2"}
	if err := EncodeRecord(&buf, rec); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if !bytes.HasPrefix(b, []byte("MESSAGE\n")) {
		t.Fatalf("prefix: %q", b)
	}
	lenBytes := b[len("MESSAGE\n") : len("MESSAGE\n")+8]
	length := binary.LittleEndian.Uint64(lenBytes)
	if length != uint64(len("line1\nline2")) {
		t.Fatalf("length=%d", length)
	}
}

func TestEncodeBatchMultipleRecords(t *testing.T) {
	var buf bytes.Buffer
	recs := []map[string]string{
		{"MESSAGE": "a"},
		{"MESSAGE": "b"},
	}
	for _, r := range recs {
		if err := EncodeRecord(&buf, r); err != nil {
			t.Fatal(err)
		}
	}
	got := buf.String()
	if strings.Count(got, "\n\n") != 2 {
		t.Fatalf("expected two record terminators, got %q", got)
	}
}
