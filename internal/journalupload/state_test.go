package journalupload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndReadSession(t *testing.T) {
	dir := t.TempDir()
	s := &State{Dir: dir}
	in := Session{URL: "https://example.test/a", Username: "42", Password: "pw"}
	if err := s.WriteSession(in); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	out, ok, err := s.ReadSession()
	if err != nil || !ok {
		t.Fatalf("ReadSession ok=%v err=%v", ok, err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestReadSessionAbsent(t *testing.T) {
	s := &State{Dir: t.TempDir()}
	_, ok, err := s.ReadSession()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false on missing session file")
	}
}

func TestClearRemovesBoth(t *testing.T) {
	dir := t.TempDir()
	s := &State{Dir: dir}
	_ = s.WriteSession(Session{URL: "x", Username: "1", Password: "p"})
	_ = s.WriteCursor("s=abc;i=1")

	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	for _, name := range []string{"journal-upload.session.json", "journal-upload.cursor"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone: err=%v", name, err)
		}
	}
}

func TestCursorRoundtrip(t *testing.T) {
	s := &State{Dir: t.TempDir()}
	if err := s.WriteCursor("s=abc;i=42"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ReadCursor()
	if err != nil || !ok {
		t.Fatalf("ReadCursor ok=%v err=%v", ok, err)
	}
	if got != "s=abc;i=42" {
		t.Fatalf("got %q", got)
	}
}
