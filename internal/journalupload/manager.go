package journalupload

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// journalReader is a narrow interface around sd_journal used so tests can
// substitute a fake without linking against libsystemd.
type journalReader interface {
	SeekCursor(string) error
	TestCursor(string) (bool, error)
	SeekHead() error
	AddBootMatch(bootID string) error
	Next() (bool, error)
	Entry() (map[string]string, string, error)
	Wait(time.Duration)
	Close() error
}

type Manager struct {
	State       *State
	NewReader   func() (journalReader, error)
	BootIDFn    func() string
	BatchSizeKB int
	FlushEvery  time.Duration
	Client      *http.Client
}

func (m *Manager) Run(ctx context.Context) {
	sess, ok, err := m.State.ReadSession()
	if err != nil || !ok {
		log.Printf("journalupload: no session found, exiting")
		return
	}

	if m.BatchSizeKB == 0 {
		m.BatchSizeKB = 256
	}
	if m.FlushEvery == 0 {
		m.FlushEvery = 1 * time.Second
	}
	if m.BootIDFn == nil {
		m.BootIDFn = readCurrentBootID
	}

	reader, err := m.NewReader()
	if err != nil {
		log.Printf("journalupload: NewReader: %v", err)
		return
	}
	defer reader.Close()

	emitWarning := false

	stored, hasCursor, _ := m.State.ReadCursor()
	if hasCursor {
		if err := reader.SeekCursor(stored); err == nil {
			if ok, _ := reader.TestCursor(stored); !ok {
				emitWarning = true
			}
		} else {
			emitWarning = true
		}
	}
	if emitWarning || !hasCursor {
		_ = reader.AddBootMatch(m.BootIDFn())
		_ = reader.SeekHead()
	}

	uploader := &Uploader{
		URL:      sess.URL,
		Username: sess.Username,
		Password: sess.Password,
		Client:   m.Client,
	}

	var buf bytes.Buffer
	if emitWarning {
		writeSynthetic(&buf, "radio-gaga: journal cursor unresolvable, resuming from current boot head")
	}
	var lastCursor string
	batchStarted := time.Now()

	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		err := uploader.Upload(ctx, buf.Bytes())
		if err != nil {
			if errors.Is(err, ErrSessionInvalid) {
				_ = m.State.Clear()
				return err
			}
			return err
		}
		if lastCursor != "" {
			_ = m.State.WriteCursor(lastCursor)
		}
		buf.Reset()
		batchStarted = time.Now()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = flush()
			return
		default:
		}

		ok, err := reader.Next()
		if err != nil {
			log.Printf("journalupload: Next: %v", err)
			return
		}
		if !ok {
			reader.Wait(500 * time.Millisecond)
			if time.Since(batchStarted) >= m.FlushEvery {
				if err := flush(); err != nil && errors.Is(err, ErrSessionInvalid) {
					return
				}
			}
			continue
		}

		fields, cursor, err := reader.Entry()
		if err != nil {
			log.Printf("journalupload: Entry: %v", err)
			continue
		}
		if cursor != "" {
			lastCursor = cursor
		}
		if err := EncodeRecord(&buf, fields); err != nil {
			log.Printf("journalupload: encode: %v", err)
			continue
		}

		if buf.Len() >= m.BatchSizeKB*1024 || time.Since(batchStarted) >= m.FlushEvery {
			if err := flush(); err != nil && errors.Is(err, ErrSessionInvalid) {
				return
			}
		}
	}
}

func writeSynthetic(w *bytes.Buffer, msg string) {
	_ = EncodeRecord(w, map[string]string{
		"__REALTIME_TIMESTAMP": strconv.FormatInt(time.Now().UnixMicro(), 10),
		"PRIORITY":             "4",
		"_SYSTEMD_UNIT":        "radio-gaga.service",
		"MESSAGE":              msg,
	})
}

func readCurrentBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	s := string(bytes.ReplaceAll(bytes.TrimSpace(b), []byte("-"), nil))
	return s
}
