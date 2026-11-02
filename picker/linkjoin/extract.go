package linkjoin

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Link is one URL or path offered to the picker.
type Link struct {
	Text string
	Kind Kind
	Rows int // physical rows the link's virtual line spans; 1 = not rejoined
}

var (
	continuationStart = regexp.MustCompile(`^[A-Za-z0-9/_.~%?#&=+@$:,;!-]`)
	listItem          = regexp.MustCompile(`^(\d+[.)]|[-*+•])\s`)
	bareSegment       = regexp.MustCompile(`^[.\w~$@-]+$`)
	openGroupTail     = regexp.MustCompile(`\(` + urlChar + `*$`)
	schemes           = []string{"https://", "http://", "git@", "git://", "ssh://", "ftp://", "file:///"}
)

// maxEdgeToken stops rejoining an unspaced blob: each join rescans the edge
// token, so an unbounded one makes rejoining quadratic in the capture.
const maxEdgeToken = 8192

// row is one captured line: its text without escapes or trailing spaces,
// the OSC 8 hrefs it carried, and the cell width of its last physical row.
type row struct {
	plain string
	hrefs []string
	tail  int
}

// virtual is a run of rows rejoined into one logical line.
type virtual struct {
	text  string
	joins []join
	rows  []row
	// cand is the last join's token+next and candSpans its matches.
	cand      string
	candSpans []span
}

// join is the byte offset in a virtual line's text where a continuation row
// begins; ambiguous when the head alone may also be the whole link.
type join struct {
	at        int
	ambiguous bool
}

// Extract returns the URLs and paths in capture (a `capture-pane -p -J [-e]`
// dump of a pane width cells wide), newest (bottom-most) first.
func Extract(capture string, width int) []Link {
	vls := rejoin(parseRows(capture, width), width)
	var out []Link
	seen := map[string]bool{}
	for i := len(vls) - 1; i >= 0; i-- {
		for _, l := range vls[i].links() {
			if !seen[l.Text] {
				seen[l.Text] = true
				out = append(out, l)
			}
		}
	}
	return out
}

func parseRows(capture string, width int) []row {
	lines := strings.Split(strings.TrimSuffix(capture, "\n"), "\n")
	rows := make([]row, len(lines))
	for i, line := range lines {
		plain := strings.TrimRight(ansi.Strip(line), " ")
		rows[i] = row{plain: plain, hrefs: hrefs(line), tail: tail(plain, width)}
	}
	return rows
}

// tail is the cell width of the last physical row of a possibly -J-joined
// line. A row tmux left one cell short for a wide glyph undercounts it by
// one; links are ASCII, so the one-cell join margin absorbs that.
func tail(plain string, width int) int {
	w := ansi.StringWidth(plain)
	if w == 0 {
		return 0
	}
	if r := w % width; r != 0 {
		return r
	}
	return width
}

// hrefs returns the OSC 8 targets in line that are whole URLs by urlRe.
func hrefs(line string) []string {
	const intro = "\x1b]8;"
	var out []string
	for {
		i := strings.Index(line, intro)
		if i < 0 {
			return out
		}
		line = line[i+len(intro):]
		params := strings.IndexByte(line, ';')
		if params < 0 {
			return out
		}
		line = line[params+1:]
		end, size := oscEnd(line)
		if end < 0 {
			return out
		}
		if uri := line[:end]; isWholeURL(uri) {
			out = append(out, uri)
		}
		line = line[end+size:]
	}
}

// oscEnd finds the OSC terminator, BEL or ESC \, returning its offset and length.
func oscEnd(s string) (end, size int) {
	bel := strings.IndexByte(s, '\a')
	st := strings.Index(s, "\x1b\\")
	if st >= 0 && (bel < 0 || st < bel) {
		return st, 2
	}
	return bel, 1
}

func isWholeURL(s string) bool {
	loc := urlRe.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s) && TrimTrailing(s) == s
}

func rejoin(rows []row, width int) []virtual {
	out := make([]virtual, 0, len(rows))
	for _, r := range rows {
		if n := len(out); n > 0 {
			if next, ok, ambiguous := out[n-1].continues(r, width); ok {
				v := &out[n-1]
				v.joins = append(v.joins, join{len(v.text), ambiguous})
				v.text += next
				v.rows = append(v.rows, r)
				continue
			}
		}
		out = append(out, virtual{text: r.plain, rows: []row{r}})
	}
	return out
}

// continues decides whether r carries on the link at the end of v. Claude
// Code and pi break a token wider than the row mid-token at the edge (pi one
// cell short of it) instead of soft-wrapping, so tmux sees a hard line.
func (v *virtual) continues(r row, width int) (next string, ok, ambiguous bool) {
	if v.rows[len(v.rows)-1].tail < width-1 {
		return "", false, false
	}
	next = strings.TrimLeft(r.plain, " ")
	if !continuationStart.MatchString(next) || listItem.MatchString(next) {
		return "", false, false
	}
	token := edgeToken(v.text)
	if len(token) > maxEdgeToken {
		return "", false, false
	}
	cand := token + next
	spans := matches(cand)
	byMatch, byScheme, bySegment := v.linkHead(token, next, spans)
	if !byMatch && !byScheme && !bySegment {
		return "", false, false
	}
	v.cand, v.candSpans = cand, spans
	// After a link head, a row that is one token running to the edge is itself
	// a mid-token break; any other row may have been word-wrapped after a link.
	// A 3c head stays ambiguous: its other reading is the continuation itself,
	// and a path longer than a row always opens with such a row.
	midToken := !strings.ContainsFunc(next, unicode.IsSpace) && r.tail >= width-1
	return next, true, (byMatch && !midToken) || (bySegment && !byMatch && !byScheme)
}

func edgeToken(text string) string {
	return text[strings.LastIndexFunc(text, unicode.IsSpace)+1:]
}

// linkHead reports by which spec rule (3a match, 3b scheme prefix, 3c bare
// segment before a '/') the edge token heads a link crossing into next.
// spans are the matches of token+next: a crossing match cannot start before
// the token (no pattern spans whitespace), so each join's work is O(token).
func (v *virtual) linkHead(token, next string, spans []span) (byMatch, byScheme, bySegment bool) {
	at := len(token)
	crosses := crossing(spans, at, 0, false)
	tokenSpans := v.tokenSpans(token)
	byMatch = crosses && (endsInMatch(tokenSpans, at) || closesGroup(token, tokenSpans, spans))
	if suffix := schemePrefixSuffix(token); suffix != "" {
		byScheme = crossing(spans, at, at-len(suffix), true)
	}
	bySegment = crosses && next[0] == '/' && bareSegment.MatchString(token)
	return byMatch, byScheme, bySegment
}

// tokenSpans returns matches(token), reusing the last join's token+next when
// that join made its whole row part of token: a long token is not rescanned.
func (v *virtual) tokenSpans(token string) []span {
	if token == v.cand {
		return v.candSpans
	}
	return matches(token)
}

// closesGroup reports whether token ends in an unclosed '(' group, at which
// urlRe cut token's URL match short, and a URL of token+next runs from that
// match's start across the join.
func closesGroup(token string, tokenSpans, spans []span) bool {
	loc := openGroupTail.FindStringIndex(token)
	if loc == nil {
		return false
	}
	for _, t := range tokenSpans {
		if t.kind == URL && t.end == loc[0] {
			return crossing(spans, len(token), t.start, true)
		}
	}
	return false
}

func endsInMatch(spans []span, end int) bool {
	for _, sp := range spans {
		if sp.end == end {
			return true
		}
	}
	return false
}

// schemePrefixSuffix returns the longest suffix of token that is a non-empty
// prefix of an allowed scheme, or "".
func schemePrefixSuffix(token string) string {
	for i := max(0, len(token)-len("file:///")); i < len(token); i++ {
		suffix := token[i:]
		for _, s := range schemes {
			if strings.HasPrefix(s, suffix) {
				return suffix
			}
		}
	}
	return ""
}

// crossing reports whether spans has a match starting at or after from and
// before at that ends after at; urlOnly restricts it to URL matches.
func crossing(spans []span, at, from int, urlOnly bool) bool {
	for _, sp := range spans {
		if sp.start >= from && sp.start < at && sp.end > at && (!urlOnly || sp.kind == URL) {
			return true
		}
	}
	return false
}

// links lists v's links in reading order: the joined readings, then for an
// ambiguous join the readings with only the ambiguous joins broken, then the
// OSC 8 hrefs.
func (v *virtual) links() []Link {
	out := spanLinks(v.text, v.joins)
	if slices.ContainsFunc(v.joins, func(j join) bool { return j.ambiguous }) {
		out = append(out, v.splitLinks()...)
	}
	for _, r := range v.rows {
		for _, h := range r.hrefs {
			out = append(out, Link{Text: h, Kind: URL, Rows: 1})
		}
	}
	return out
}

// splitLinks reads v as broken at each ambiguous join: the links of every
// segment between them, the certain joins inside a segment kept.
func (v *virtual) splitLinks() []Link {
	var out []Link
	var certain []join
	start := 0
	for _, j := range v.joins {
		if !j.ambiguous {
			certain = append(certain, join{at: j.at - start})
			continue
		}
		out = append(out, spanLinks(v.text[start:j.at], certain)...)
		start, certain = j.at, nil
	}
	return append(out, spanLinks(v.text[start:], certain)...)
}

func spanLinks(text string, joins []join) []Link {
	var out []Link
	for _, sp := range matches(text) {
		t := TrimTrailing(text[sp.start:sp.end])
		if t == "" {
			continue
		}
		rows := 1
		for _, j := range joins {
			if sp.start < j.at && j.at < sp.start+len(t) {
				rows++
			}
		}
		out = append(out, Link{Text: t, Kind: sp.kind, Rows: rows})
	}
	return out
}
