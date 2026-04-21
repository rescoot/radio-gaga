//go:build linux && cgo

package journalupload

import (
	"strconv"
	"time"

	"github.com/coreos/go-systemd/v22/sdjournal"
)

// sdReader adapts *sdjournal.Journal to journalReader.
type sdReader struct {
	j *sdjournal.Journal
}

func NewSdJournalReader() (journalReader, error) {
	j, err := sdjournal.NewJournal()
	if err != nil {
		return nil, err
	}
	return &sdReader{j: j}, nil
}

func (r *sdReader) SeekCursor(c string) error { return r.j.SeekCursor(c) }

func (r *sdReader) TestCursor(c string) (bool, error) {
	if err := r.j.TestCursor(c); err != nil {
		return false, nil
	}
	return true, nil
}

func (r *sdReader) SeekHead() error { return r.j.SeekHead() }

func (r *sdReader) AddBootMatch(bootID string) error {
	if bootID == "" {
		return nil
	}
	return r.j.AddMatch("_BOOT_ID=" + bootID)
}

func (r *sdReader) Next() (bool, error) {
	n, err := r.j.Next()
	return n > 0, err
}

func (r *sdReader) Entry() (map[string]string, string, error) {
	e, err := r.j.GetEntry()
	if err != nil {
		return nil, "", err
	}
	fields := make(map[string]string, len(e.Fields)+2)
	for k, v := range e.Fields {
		fields[k] = v
	}
	if e.Cursor != "" {
		fields["__CURSOR"] = e.Cursor
	}
	fields["__REALTIME_TIMESTAMP"] = strconv.FormatUint(e.RealtimeTimestamp, 10)
	fields["__MONOTONIC_TIMESTAMP"] = strconv.FormatUint(e.MonotonicTimestamp, 10)
	return fields, e.Cursor, nil
}

func (r *sdReader) Wait(d time.Duration) { r.j.Wait(d) }

func (r *sdReader) Close() error { return r.j.Close() }
