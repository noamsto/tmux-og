package remoteopen

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// safeText strips terminal-active bytes from text that came from the remote
// (probe output, daemon log) before it reaches the local terminal. It keeps
// \n and \t so a log tail stays readable. Twin of skipEscape in picker/attach.go,
// which is unimportable from package main.
func safeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// a raw 0x9b is a one-byte CSI on some terminals
		case r == 0x1b:
			i = skipEscape(s, i+1)
			continue
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f, isBidi(r):
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// lineSanitizer passes a child's inherited stdout/stderr to dst through
// safeText one whole line at a time, so an escape sequence split across reads
// is never emitted half-stripped. os/exec writes from a copying goroutine, so
// Write is locked; flush emits the unterminated last line after Wait.
type lineSanitizer struct {
	dst io.Writer
	mu  sync.Mutex
	buf []byte
}

func (w *lineSanitizer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(w.buf[:i+1])
		w.buf = w.buf[i+1:]
		if _, err := io.WriteString(w.dst, safeText(line)); err != nil {
			return len(p), err
		}
	}
}

func (w *lineSanitizer) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	_, _ = io.WriteString(w.dst, safeText(string(w.buf)))
	w.buf = nil
}

func isBidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// skipEscape returns the index just past the escape sequence whose ESC
// precedes i: a CSI through its final byte, a string sequence (OSC, DCS, SOS,
// PM, APC) through BEL or ST, or else the one character after ESC.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		for j := i + 1; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return len(s)
	case ']', 'P', 'X', '^', '_':
		for j := i + 1; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return i + size
}
