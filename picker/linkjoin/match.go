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

// The patterns port #1067's @fingers-pattern-url/path (its replacements for
// tmux-fingers' url and path built-ins), so the picker and fingers select the
// same text. RE2 has no lookbehind: their trailing (?<![…]) is TrimTrailing.
// \w is ASCII-only in RE2, so a path stops before the ':' of path:line:col.
var (
	urlRe  = regexp.MustCompile(`(?:https?://|git@|git://|ssh://|ftp://|file:///)(?:` + urlChar + `|\(` + urlChar + `*\))+`)
	pathRe = regexp.MustCompile(`(?:[.\w\-~$@]+)?(?:/[.\w\-@]+)+/?`)
)

// urlChar adds control bytes to #1067's exclusions, so no escape byte from a
// capture can reach the opener inside a URL.
const urlChar = `[^\s()"'` + "`" + `\x00-\x1f\x7f]`

// span is the greedy, untrimmed byte range of one match in a line.
type span struct {
	start, end int
	kind       Kind
}

// matches returns every URL match in s plus the path matches that do not
// overlap one, ordered by start. A URL's own path part is not offered again.
func matches(s string) []span {
	var urls []span
	// Every scheme holds "://" or "git@"; skipping urlRe, by far the slower
	// pattern, when neither occurs keeps a long path-only line cheap.
	if strings.Contains(s, "://") || strings.Contains(s, "git@") {
		for _, m := range urlRe.FindAllStringIndex(s, -1) {
			urls = append(urls, span{m[0], m[1], URL})
		}
	}
	out := urls
	for _, m := range pathRe.FindAllStringIndex(s, -1) {
		if !overlapsAny(urls, m[0], m[1]) {
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

// TrimTrailing repeatedly strips a final rune the lookbehind of #1067's
// patterns rejects, which is what PCRE backtracking off it amounts to. No
// bracket balancing: urlRe admits only a balanced (…) and pathRe holds none
// of these but '.'. The vectors are testdata/trailing.tsv.
func TrimTrailing(s string) string {
	return strings.TrimRight(s, ".,;:!?>]}")
}

// Openable reports whether the picker may hand s to the platform opener.
func Openable(s string) bool {
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "file:///")
}
