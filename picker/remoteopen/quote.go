package remoteopen

import (
	"fmt"
	"strings"
)

// UsageError is a refusal the launcher prints verbatim and exits 1 on.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return "og-remote-open: " + e.Msg }

// bashQuote reproduces bash's `printf %q` under LC_ALL=C; it is only used to
// render a rejected value in an error message, but the text is pinned by tests.
func bashQuote(s string) string {
	if s == "" {
		return "''"
	}
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return bashAnsiCQuote(s)
		}
	}
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case strings.IndexByte(" !\"$&'()*,;<>?[\\]^`{|}", c) >= 0:
			b.WriteByte('\\')
		case c == '#' && i == 0:
			b.WriteByte('\\')
		case c == '~' && (i == 0 || s[i-1] == ':' || s[i-1] == '='):
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func bashAnsiCQuote(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := range len(s) {
		switch c := s[i]; c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\E`)
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// shellQuote single-quotes s for any POSIX-ish remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuotable reports whether s survives every remote shell dialect the
// launcher feeds it to; a backslash does not (fish and csh read it inside
// single quotes).
func shellQuotable(s string) bool {
	return !strings.Contains(s, `\`)
}

// validRemotePath matches ^/[A-Za-z0-9._/@+:-]*$.
func validRemotePath(s string) bool {
	if s == "" || s[0] != '/' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("._/@+:-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func requireSessionName(s string) error {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return &UsageError{"session name contains a control character, which no tmux session name can: " + bashQuote(s) + " — pass an explicit session name instead"}
		}
	}
	if !shellQuotable(s) {
		return &UsageError{"session name contains a backslash, which no remote shell dialect can quote safely: " + s + " — pass an explicit session name instead"}
	}
	return nil
}

// posixDirname follows coreutils dirname.
func posixDirname(s string) string {
	if s == "" {
		return "."
	}
	t := strings.TrimRight(s, "/")
	if t == "" {
		return "/"
	}
	i := strings.LastIndexByte(t, '/')
	if i < 0 {
		return "."
	}
	d := strings.TrimRight(t[:i], "/")
	if d == "" {
		return "/"
	}
	return d
}
