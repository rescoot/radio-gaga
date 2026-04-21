package journalupload

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// EncodeRecord writes one record in systemd journal-export format.
// Fields whose value contains a newline or non-printable control byte are
// written in the binary-length-prefixed form. A blank line terminates the
// record.
// Spec: https://systemd.io/JOURNAL_EXPORT_FORMATS/
func EncodeRecord(w io.Writer, fields map[string]string) error {
	for k, v := range fields {
		if needsBinary(v) {
			if _, err := fmt.Fprintf(w, "%s\n", k); err != nil {
				return err
			}
			var lenBuf [8]byte
			binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(v)))
			if _, err := w.Write(lenBuf[:]); err != nil {
				return err
			}
			if _, err := io.WriteString(w, v); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "%s=%s\n", k, v); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func needsBinary(s string) bool {
	if strings.ContainsAny(s, "\n\x00") {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\t' {
			return true
		}
	}
	return false
}
