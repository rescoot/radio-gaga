package journalupload

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const journalctlBinSizeCap = 1 << 30 // 1 GiB; sanity cap on a binary field

// journalctlReader implements journalReader by streaming
// `journalctl --output=export --follow` and parsing the export format.
// No cgo, no glibc pin: works on every systemd host.
type journalctlReader struct {
	cursor  string
	useHead bool
	bootID  string

	startMu sync.Mutex
	started bool
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser

	recordCh chan record
	cancel   context.CancelFunc

	current record
}

type record struct {
	fields map[string]string
	cursor string
	err    error
}

// NewJournalctlReader returns a journalReader backed by `journalctl`.
// The subprocess is started lazily on the first Next() so the caller can
// configure cursor / boot match first.
func NewJournalctlReader() (journalReader, error) {
	return &journalctlReader{
		recordCh: make(chan record, 64),
	}, nil
}

func (r *journalctlReader) SeekCursor(c string) error {
	if r.started {
		return errors.New("journalctl: SeekCursor after start")
	}
	r.cursor = c
	r.useHead = false
	return nil
}

// TestCursor checks whether journalctl accepts the cursor. We use a quick
// non-streaming invocation; non-zero exit means the cursor is unknown.
func (r *journalctlReader) TestCursor(c string) (bool, error) {
	cmd := exec.Command("journalctl", "--after-cursor="+c, "-n", "0", "--no-pager", "--quiet")
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

func (r *journalctlReader) SeekHead() error {
	if r.started {
		return errors.New("journalctl: SeekHead after start")
	}
	r.cursor = ""
	r.useHead = true
	return nil
}

func (r *journalctlReader) AddBootMatch(bootID string) error {
	if r.started {
		return errors.New("journalctl: AddBootMatch after start")
	}
	r.bootID = bootID
	return nil
}

func (r *journalctlReader) Next() (bool, error) {
	if err := r.ensureStarted(); err != nil {
		return false, err
	}
	select {
	case rec, ok := <-r.recordCh:
		if !ok {
			return false, io.EOF
		}
		if rec.err != nil {
			return false, rec.err
		}
		r.current = rec
		return true, nil
	default:
		return false, nil
	}
}

func (r *journalctlReader) Entry() (map[string]string, string, error) {
	if r.current.fields == nil {
		return nil, "", errors.New("journalctl: no current entry")
	}
	return r.current.fields, r.current.cursor, nil
}

func (r *journalctlReader) Wait(d time.Duration) { time.Sleep(d) }

func (r *journalctlReader) Close() error {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_ = r.cmd.Wait()
	}
	return nil
}

func (r *journalctlReader) ensureStarted() error {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.started {
		return nil
	}
	args := []string{"--output=export", "--follow", "--no-pager"}
	if r.cursor != "" {
		args = append(args, "--after-cursor="+r.cursor)
	}
	if r.bootID != "" {
		args = append(args, "--boot="+r.bootID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.cmd = exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr, err := r.cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := r.cmd.Start(); err != nil {
		cancel()
		return err
	}
	r.stdout = stdout
	r.stderr = stderr
	go r.drain(bufio.NewReaderSize(stdout, 64<<10))
	go r.discardStderr()
	r.started = true
	return nil
}

func (r *journalctlReader) drain(br *bufio.Reader) {
	defer close(r.recordCh)
	for {
		fields, cursor, err := decodeExportRecord(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			r.recordCh <- record{err: err}
			return
		}
		r.recordCh <- record{fields: fields, cursor: cursor}
	}
}

func (r *journalctlReader) discardStderr() {
	if r.stderr == nil {
		return
	}
	_, _ = io.Copy(io.Discard, r.stderr)
}

// decodeExportRecord reads one journal-export record from br. Returns the
// parsed fields and the entry's __CURSOR (if present). Returns io.EOF when
// the stream ends cleanly between records.
//
// Format spec: https://systemd.io/JOURNAL_EXPORT_FORMATS/
//   - KEY=VALUE\n  for plain text
//   - KEY\n<u64 LE length><raw bytes>\n  for binary
//   - blank line terminates the record
func decodeExportRecord(br *bufio.Reader) (map[string]string, string, error) {
	fields := map[string]string{}
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 && len(fields) == 0 {
					return nil, "", io.EOF
				}
				return nil, "", io.ErrUnexpectedEOF
			}
			return nil, "", err
		}
		l := len(line) - 1
		line = line[:l]
		if l == 0 {
			return fields, fields["__CURSOR"], nil
		}
		eq := indexByte(line, '=')
		if eq >= 0 {
			fields[string(line[:eq])] = string(line[eq+1:])
			continue
		}
		key := string(line)
		var lenBuf [8]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return nil, "", fmt.Errorf("binary len for %q: %w", key, err)
		}
		n := binary.LittleEndian.Uint64(lenBuf[:])
		if n > journalctlBinSizeCap {
			return nil, "", fmt.Errorf("binary field %q oversize: %d", key, n)
		}
		val := make([]byte, n)
		if _, err := io.ReadFull(br, val); err != nil {
			return nil, "", fmt.Errorf("binary value for %q: %w", key, err)
		}
		if _, err := br.ReadByte(); err != nil {
			return nil, "", fmt.Errorf("binary terminator for %q: %w", key, err)
		}
		fields[key] = string(val)
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
