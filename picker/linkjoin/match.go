// Package linkjoin extracts URLs and file paths from a tmux pane capture,
// rejoining ones a terminal hard-wrapped across rows (#1068).
package linkjoin

import (
	"regexp"
	"sort"
	"strings"
)

// Kind says what a link is, which decides whether the picker may open it.
type Kind int

const (
	URL Kind = iota
	Path
)

func (k Kind) String() string {
	if k == URL {
		return "url"
	}
	return "path"
}

// The patterns mirror tmux-fingers' built-ins so the picker and fingers
// select the same text (#1067). \w is ASCII-only in RE2, so a path stops
// before the ':' of path:line:col.
var (
	urlRe  = regexp.MustCompile(`(?:https?://|git@|git://|ssh://|ftp://|file:///)[^\s\x00-\x1f\x7f"'<>` + "`" + `]+`)
	pathRe = regexp.MustCompile(`(?:[.\w\-~$@]+)?(?:/[.\w\-@]+)+/?`)
)

// span is the greedy, untrimmed byte range of one match in a line.
type span struct {
	start, end int
	kind       Kind
}

// matches returns every URL match in s plus the path matches that do not
// overlap one, ordered by start. A URL's own path part is not offered again.
func matches(s string) []span {
	var out []span
	for _, m := range urlRe.FindAllStringIndex(s, -1) {
		out = append(out, span{m[0], m[1], URL})
	}
	urls := len(out)
	for _, m := range pathRe.FindAllStringIndex(s, -1) {
		if !overlapsAny(out[:urls], m[0], m[1]) {
			out = append(out, span{m[0], m[1], Path})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func overlapsAny(spans []span, start, end int) bool {
	for _, sp := range spans {
		if start < sp.end && sp.start < end {
			return true
		}
	}
	return false
}

// TrimTrailing strips the sentence punctuation and unbalanced closers a
// greedy match picks up. A closer survives when its opener is inside the
// match (so Foo_(bar) stays whole); a quote is stripped when its count is
// odd. The shared vectors are testdata/trailing.tsv.
func TrimTrailing(s string) string {
	for s != "" {
		last := s[len(s)-1]
		switch {
		case strings.IndexByte(".,;:!?", last) >= 0:
		case last == ')' && unbalanced(s, '(', ')'):
		case last == ']' && unbalanced(s, '[', ']'):
		case last == '}' && unbalanced(s, '{', '}'):
		case last == '>' && unbalanced(s, '<', '>'):
		case (last == '\'' || last == '"' || last == '`') && strings.Count(s, string(last))%2 == 1:
		default:
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

func unbalanced(s string, open, closer byte) bool {
	return strings.Count(s, string(closer)) > strings.Count(s, string(open))
}

// Openable reports whether the picker may hand s to the platform opener.
func Openable(s string) bool {
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "file:///")
}
