package linkjoin

import (
	"regexp"
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
	plainWord         = regexp.MustCompile(`^[A-Za-z]+$`)
	schemes           = []string{"https://", "http://", "git@", "git://", "ssh://", "ftp://", "file:///"}
)

// row is one captured line: its text without escapes or trailing spaces,
// the OSC 8 hrefs it carried, and the cell width of its last physical row.
type row struct {
	plain string
	hrefs []string
	tail  int
}

// virtual is a run of rows rejoined into one logical line.
type virtual struct {
	text      string
	joins     []int // byte offsets in text where a continuation row begins
	ambiguous bool
	rows      []row
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
				v.joins = append(v.joins, len(v.text))
				v.text += next
				v.ambiguous = v.ambiguous || ambiguous
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
	byMatch, byScheme, bySegment := linkHead(v.text, token, next)
	if !byMatch && !byScheme && !bySegment {
		return "", false, false
	}
	return next, true, endsInPunct(token) || startsWithWord(next) || (bySegment && !byMatch && !byScheme)
}

func edgeToken(text string) string {
	return text[strings.LastIndexFunc(text, unicode.IsSpace)+1:]
}

// linkHead reports by which spec rule (3a match, 3b scheme prefix, 3c bare
// segment before a '/') the edge token heads a link crossing into next.
func linkHead(text, token, next string) (byMatch, byScheme, bySegment bool) {
	cand := text + next
	byMatch = endsInMatch(token) && crossing(cand, len(text), 0, false)
	if suffix := schemePrefixSuffix(token); suffix != "" {
		byScheme = crossing(cand, len(text), len(text)-len(suffix), true)
	}
	bySegment = bareSegment.MatchString(token) && next[0] == '/' && crossing(cand, len(text), 0, false)
	return byMatch, byScheme, bySegment
}

func endsInMatch(token string) bool {
	for _, sp := range matches(token) {
		if sp.end == len(token) {
			return true
		}
	}
	return false
}

// schemePrefixSuffix returns the longest suffix of token that is a non-empty
// prefix of an allowed scheme, or "".
func schemePrefixSuffix(token string) string {
	for i := range len(token) {
		suffix := token[i:]
		for _, s := range schemes {
			if strings.HasPrefix(s, suffix) {
				return suffix
			}
		}
	}
	return ""
}

// crossing reports whether cand has a match starting at or after from and
// before join that ends after join; urlOnly restricts it to URL matches.
func crossing(cand string, join, from int, urlOnly bool) bool {
	for _, sp := range matches(cand) {
		if sp.start >= from && sp.start < join && sp.end > join && (!urlOnly || sp.kind == URL) {
			return true
		}
	}
	return false
}

func endsInPunct(token string) bool {
	return token != "" && strings.IndexByte(".,;:!?", token[len(token)-1]) >= 0
}

// startsWithWord reports whether next opens with a plain word, which a
// word-wrapped row does as often as a mid-token continuation does.
func startsWithWord(next string) bool {
	first, _, _ := strings.Cut(next, " ")
	return plainWord.MatchString(first)
}

// links lists v's links in reading order: the joined readings, then for an
// ambiguous join each row's own, then the OSC 8 hrefs.
func (v *virtual) links() []Link {
	out := spanLinks(v.text, v.joins)
	if v.ambiguous {
		for _, r := range v.rows {
			out = append(out, spanLinks(r.plain, nil)...)
		}
	}
	for _, r := range v.rows {
		for _, h := range r.hrefs {
			out = append(out, Link{Text: h, Kind: URL, Rows: 1})
		}
	}
	return out
}

func spanLinks(text string, joins []int) []Link {
	var out []Link
	for _, sp := range matches(text) {
		t := TrimTrailing(text[sp.start:sp.end])
		if t == "" {
			continue
		}
		rows := 1
		for _, j := range joins {
			if sp.start < j && j < sp.start+len(t) {
				rows++
			}
		}
		out = append(out, Link{Text: t, Kind: sp.kind, Rows: rows})
	}
	return out
}
