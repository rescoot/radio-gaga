package journalupload

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	sessionFile = "journal-upload.session.json"
	cursorFile  = "journal-upload.cursor"
)

type Session struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// State manages persistent files describing whether streaming is active
// and where we left off.
type State struct {
	Dir string
}

func (s *State) sessionPath() string { return filepath.Join(s.Dir, sessionFile) }
func (s *State) cursorPath() string  { return filepath.Join(s.Dir, cursorFile) }

func (s *State) ensureDir() error {
	return os.MkdirAll(s.Dir, 0o755)
}

func (s *State) WriteSession(sess Session) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return writeAtomic(s.sessionPath(), data, 0o600)
}

func (s *State) ReadSession() (Session, bool, error) {
	f, err := os.Open(s.sessionPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}
	defer f.Close()
	var sess Session
	if err := json.NewDecoder(f).Decode(&sess); err != nil && err != io.EOF {
		return Session{}, false, err
	}
	return sess, true, nil
}

func (s *State) WriteCursor(cursor string) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	return writeAtomic(s.cursorPath(), []byte(cursor), 0o600)
}

func (s *State) ReadCursor() (string, bool, error) {
	data, err := os.ReadFile(s.cursorPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

func (s *State) Clear() error {
	var firstErr error
	for _, p := range []string{s.sessionPath(), s.cursorPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
