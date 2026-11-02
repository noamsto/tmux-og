package linkjoin

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// loadFixture reads testdata/<name>: a "# width=<W>" header, then the capture.
// It fails when a row is wider than W, so a miscounted fixture cannot pass.
func loadFixture(t *testing.T, name string) (capture string, width int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	header, capture, ok := strings.Cut(string(raw), "\n")
	if !ok {
		t.Fatalf("%s: no header line", name)
	}
	width, err = strconv.Atoi(strings.TrimPrefix(header, "# width="))
	if err != nil {
		t.Fatalf("%s: bad header %q: %v", name, header, err)
	}
	for i, row := range strings.Split(capture, "\n") {
		if w := ansi.StringWidth(row); w > width {
			t.Fatalf("%s: row %d is %d cells, wider than %d", name, i+1, w, width)
		}
	}
	return capture, width
}

// assertEdgeRows checks that exactly n rows reach the edge (width-1 or more
// cells once trailing spaces go), i.e. that the fixture breaks where it claims.
func assertEdgeRows(t *testing.T, capture string, width, n int) {
	t.Helper()
	got := 0
	for row := range strings.SplitSeq(capture, "\n") {
		if ansi.StringWidth(strings.TrimRight(ansi.Strip(row), " ")) >= width-1 {
			got++
		}
	}
	if got != n {
		t.Fatalf("fixture has %d rows at the edge of width %d, want %d", got, width, n)
	}
}

func lines(rows ...string) string { return strings.Join(rows, "\n") + "\n" }

func assertLinks(t *testing.T, got, want []Link) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract() =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestExtractRealClaudeCodePath(t *testing.T) {
	capture, width := loadFixture(t, "claude-code-path-62.txt")
	assertLinks(t, Extract(capture, width), []Link{
		{"/tmp/claude-1000/-home-noams-git--worktrees-git-hookyard-feat-264-priors-write-guard-false-positives-on-re/f9266e70-b330-463b-b41f-50721c09f72e/scratchpad/", Path, 3},
		{"declare/printf", Path, 1},
		{"BASH_CMDS/PATH", Path, 1},
	})
}

func TestExtractRealOSC8(t *testing.T) {
	capture, width := loadFixture(t, "claude-code-osc8-91.txt")
	const href = "https://github.com/noamsto/hookyard/pull/267"
	if strings.Contains(ansi.Strip(capture), href) {
		t.Fatalf("fixture shows %s as text; it should only be an OSC 8 href", href)
	}
	assertLinks(t, Extract(capture, width), []Link{{href, URL, 1}})
}

func TestExtractOSC8(t *testing.T) {
	tests := []struct {
		name    string
		capture string
		want    []Link
	}{
		{
			"href duplicating visible text",
			lines("\x1b]8;;https://x.io/a\x07https://x.io/a\x1b]8;;\x07 and /tmp/y"),
			[]Link{{"https://x.io/a", URL, 1}, {"/tmp/y", Path, 1}},
		},
		{
			"ST-terminated href with params",
			lines("see \x1b]8;id=1;https://x.io/b\x1b\\here\x1b]8;;\x1b\\ /tmp/y"),
			[]Link{{"/tmp/y", Path, 1}, {"https://x.io/b", URL, 1}},
		},
		{
			"href rejected: trailing punctuation, foreign scheme",
			lines("\x1b]8;;https://x.io/b.\x07a\x1b]8;;\x07 \x1b]8;;mailto:x@y.io\x07b\x1b]8;;\x07 /tmp/y"),
			[]Link{{"/tmp/y", Path, 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertLinks(t, Extract(tt.capture, 200), tt.want)
		})
	}
}

func TestExtractClaudeCodeURL2Rows(t *testing.T) {
	capture, width := loadFixture(t, "cc-url-2rows-60.txt")
	assertEdgeRows(t, capture, width, 1)
	assertLinks(t, Extract(capture, width), []Link{
		{"https://github.com/noamsto/tmux-og/blob/main/docs/agents/bridge-shipped-state.md", URL, 2},
	})
}

func TestExtractClaudeCodeURL3Rows(t *testing.T) {
	capture, width := loadFixture(t, "cc-url-3rows-40.txt")
	assertEdgeRows(t, capture, width, 2)
	// Both joins are ambiguous: a whole URL meets a '/' row, then `d.` opens
	// a row like a word would.
	assertLinks(t, Extract(capture, width), []Link{
		{"https://github.com/noamsto/tmux-og/blob/main/docs/agents/bridge-daemon.md", URL, 3},
		{"https://github.com/noamsto/tmux-og", URL, 1},
		{"/blob/main/docs/agents/bridge-daemon.m", Path, 1},
	})
}

func TestExtractPiMargin(t *testing.T) {
	capture, width := loadFixture(t, "pi-path-30.txt")
	assertEdgeRows(t, capture, width, 1)
	assertLinks(t, Extract(capture, width), []Link{
		{"/home/noams/git/.worktrees/git/disp/x", Path, 2},
	})
}

// TestExtractPiTrailingPad pins pi's capture shape inline: every row carries
// one trailing pad space (text ends at width-1, the pad fills the last
// column). The trim-trailing-whitespace hook strips that pad from testdata.
func TestExtractPiTrailingPad(t *testing.T) {
	capture := lines(
		" Wrote it to the directory ",
		" /home/noams/git/.worktrees/g ",
		" it/disp/x. done ",
	)
	assertLinks(t, Extract(capture, 30), []Link{
		{"/home/noams/git/.worktrees/git/disp/x", Path, 2},
	})
}

func TestExtractMirrorSeed(t *testing.T) {
	capture, width := loadFixture(t, "mirror-seed-30.txt")
	assertEdgeRows(t, capture, width, 1)
	// A whole URL at the edge meeting a '/' row reads as one link or two.
	assertLinks(t, Extract(capture, width), []Link{
		{"https://example.com/a/very/long/path/to/page.html", URL, 2},
		{"https://example.com/a/very", URL, 1},
		{"/long/path/to/page.html", Path, 1},
	})
}

// edgeCase is a capture whose first row defines the width, so that row
// always reaches the edge.
type edgeCase struct {
	name string
	rows []string
	want []Link
}

func runEdgeCases(t *testing.T, tests []edgeCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertLinks(t, Extract(lines(tt.rows...), ansi.StringWidth(tt.rows[0])), tt.want)
		})
	}
}

func TestExtractNoJoin(t *testing.T) {
	runEdgeCases(t, []edgeCase{
		{"prose reaching the edge", []string{"see /tmp/x then prose", "  continues here"}, []Link{{"/tmp/x", Path, 1}}},
		{"blank row between", []string{"see https://x.io/a", "", "  b/c more"}, []Link{{"b/c", Path, 1}, {"https://x.io/a", URL, 1}}},
		{"next row opens a paren", []string{"see https://x.io/a", "  (b/c) more"}, []Link{{"b/c", Path, 1}, {"https://x.io/a", URL, 1}}},
		{"next row is a dash item", []string{"see https://x.io/a", "  - item"}, []Link{{"https://x.io/a", URL, 1}}},
		{"next row is a numbered item", []string{"see https://x.io/a", "  1. item"}, []Link{{"https://x.io/a", URL, 1}}},
		{"plain word then path", []string{"the data", "  files/x"}, []Link{{"files/x", Path, 1}}},
		{"scheme-prefix word then path", []string{"the logs", "  files/x"}, []Link{{"files/x", Path, 1}}},
		{"no crossing match", []string{"edit a/x.go", "  :12 more"}, []Link{{"a/x.go", Path, 1}}},
	})
}

func TestExtractSchemeBreaks(t *testing.T) {
	runEdgeCases(t, []edgeCase{
		{"inside the scheme", []string{"see htt", "  ps://x.io/a b"}, []Link{{"https://x.io/a", URL, 2}}},
		{"right after the scheme", []string{"https://", "  x.io/a b"}, []Link{{"https://x.io/a", URL, 2}}},
		{"right after git@", []string{"git@", "  github.com:o/r.git b"}, []Link{{"git@github.com:o/r.git", URL, 2}}},
		{"after an opener", []string{"(htt", "  ps://x.io/a) b"}, []Link{{"https://x.io/a", URL, 2}}},
		{"before a port", []string{"https://h", "  :8080/a b"}, []Link{{"https://h:8080/a", URL, 2}}},
	})
}

func TestExtractAmbiguous(t *testing.T) {
	runEdgeCases(t, []edgeCase{
		{"edge punctuation", []string{"see https://x.io/a.", "  Then x"}, []Link{{"https://x.io/a.Then", URL, 2}, {"https://x.io/a", URL, 1}}},
		{"plain word continuation", []string{"in /tmp/x/scratchpad", "  and then"}, []Link{{"/tmp/x/scratchpadand", Path, 2}, {"/tmp/x/scratchpad", Path, 1}}},
		{"bare segment head", []string{"saved to", "  /tmp/x"}, []Link{{"to/tmp/x", Path, 2}, {"/tmp/x", Path, 1}}},
		{"word continuation with trailing punctuation", []string{"see https://x.io/pull/12", "  here. more"}, []Link{{"https://x.io/pull/12here", URL, 2}, {"https://x.io/pull/12", URL, 1}}},
		{"number continuation", []string{"see https://x.io/pull/12", "  3 files changed"}, []Link{{"https://x.io/pull/123", URL, 2}, {"https://x.io/pull/12", URL, 1}}},
		{"contraction continuation", []string{"see https://x.io/pull/12", "  it's merged"}, []Link{{"https://x.io/pull/12it", URL, 2}, {"https://x.io/pull/12", URL, 1}}},
		{"hyphenated continuation", []string{"see https://x.io/pull/12", "  follow-up later"}, []Link{{"https://x.io/pull/12follow-up", URL, 2}, {"https://x.io/pull/12", URL, 1}}},
	})
}

// TestExtractAmbiguityPerJoin pins that only the ambiguous join is broken in
// the fallback reading: the certain join above it stays joined.
func TestExtractAmbiguityPerJoin(t *testing.T) {
	tests := []struct {
		name string
		rows []string
		want []Link
	}{
		{
			"word after a rejoined URL",
			[]string{"see https://x.io/docs/shar", "  ed-state.md/aaaaaaaaaaaa", "  And more"},
			[]Link{{"https://x.io/docs/shared-state.md/aaaaaaaaaaaaAnd", URL, 3}, {"https://x.io/docs/shared-state.md/aaaaaaaaaaaa", URL, 2}},
		},
		{
			"slash row after a rejoined URL",
			[]string{"see https://x.io/docs/sh", "  aredstateabcdefghijklm", "  /tmp/x"},
			[]Link{{"https://x.io/docs/sharedstateabcdefghijklm/tmp/x", URL, 3}, {"https://x.io/docs/sharedstateabcdefghijklm", URL, 2}, {"/tmp/x", Path, 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			width := ansi.StringWidth(tt.rows[0])
			capture := lines(tt.rows...)
			assertEdgeRows(t, capture, width, 2)
			assertLinks(t, Extract(capture, width), tt.want)
		})
	}
}

// blobBudget is TestExtractUnspacedBlobBounded's wall-clock limit;
// race_test.go widens it under the race detector.
var blobBudget = 2 * time.Second

// TestExtractUnspacedBlobBounded pins that rejoining stays near-linear: a
// blob of edge-filling rows used to rescan the whole growing line per join.
func TestExtractUnspacedBlobBounded(t *testing.T) {
	const width, n = 200, 2000
	row := strings.Repeat("aaaa/", width/5)
	rows := make([]string, n)
	for i := range rows {
		rows[i] = row
	}
	start := time.Now()
	links := Extract(lines(rows...), width)
	if d := time.Since(start); d > blobBudget {
		t.Fatalf("Extract took %v on %d rows of width %d", d, n, width)
	}
	if len(links) == 0 {
		t.Fatal("Extract found no links in the blob")
	}
	for _, l := range links {
		if len(l.Text) > maxEdgeToken+width {
			t.Errorf("link of %d bytes joined past the %d-byte cap", len(l.Text), maxEdgeToken)
		}
	}
}

func TestExtractUnambiguousOffersOnlyJoined(t *testing.T) {
	runEdgeCases(t, []edgeCase{
		{"mid-token break", []string{"see https://x.io/docs/sh", "  ared-state.md and more"}, []Link{{"https://x.io/docs/shared-state.md", URL, 2}}},
	})
}

func TestExtractSoftWrappedLongLine(t *testing.T) {
	const width = 20
	head := "see https://x.io/ab"
	long := strings.Repeat("w", 2*width-len(head)-1) + " " + head
	if len(long) != 2*width {
		t.Fatalf("long line is %d cells, want %d", len(long), 2*width)
	}
	assertLinks(t, Extract(lines(long, "  cd.html more"), width), []Link{{"https://x.io/abcd.html", URL, 2}})
}

func TestExtractDedupNewestFirst(t *testing.T) {
	capture := lines("a https://x.io/a", "b /tmp/one", "", "c /tmp/two", "d https://x.io/a")
	assertLinks(t, Extract(capture, 200), []Link{
		{"https://x.io/a", URL, 1},
		{"/tmp/two", Path, 1},
		{"/tmp/one", Path, 1},
	})
}

func TestTrailingVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "trailing.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		in, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed row %q", line)
		}
		got := "-"
		if links := Extract(in, 200); len(links) > 0 {
			got = links[0].Text
		}
		if got != want {
			t.Errorf("Extract(%q) first link = %q, want %q", in, got, want)
		}
	}
}
