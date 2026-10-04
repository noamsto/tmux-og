package main

import (
	"strings"
	"testing"
)

func TestTruncateCells(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"under", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"over", "hello world", 8, "hello w…"},
		{"tiny", "abcdef", 2, "a…"},
	}
	for _, c := range cases {
		if got := truncateCells(c.s, c.max); got != c.want {
			t.Errorf("%s: truncateCells(%q,%d) = %q, want %q", c.name, c.s, c.max, got, c.want)
		}
		if w := iconCellWidth(truncateCells(c.s, c.max)); w > c.max {
			t.Errorf("%s: result width %d exceeds max %d", c.name, w, c.max)
		}
	}
}

func TestRenderWindowItemsEnriched(t *testing.T) {
	windows := []windowData{
		// Window 1 carries a self-reported issue id (non-empty icon column);
		// window 2 has none. Both icon and label columns must be padded for the
		// identity column to align.
		{session: "proj", index: 1, name: "x", branch: "feat/eng-1-a",
			agent:   agentCounts{issues: []string{"AAA-1"}},
			labelID: "L ENG-1", labelRest: " short title",
			prPlain: "  #10", prState: "open", prCheck: "success", prMergeable: "mergeable"},
		{session: "proj", index: 2, name: "a-much-longer-window-name", active: true, branch: "feat/eng-2-b",
			labelID: "L ENG-2", labelRest: " other",
			prPlain: "  #20", prState: "open", prCheck: "failure", prMergeable: "mergeable"},
	}
	items := renderWindowItems(windows, map[string]string{}, nil, "dark", 0, false)

	var rows []listItem
	for _, it := range items {
		if !it.isHeader && !it.isColumnHeader {
			rows = append(rows, it)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 window rows, got %d", len(rows))
	}

	d0, d1 := stripANSI(rows[0].display), stripANSI(rows[1].display)
	for _, want := range []string{"L ENG-1", "short title", "#10"} {
		if !strings.Contains(d0, want) {
			t.Errorf("row0 missing %q in %q", want, d0)
		}
	}
	if !strings.Contains(d1, "#20") {
		t.Errorf("row1 missing PR badge in %q", d1)
	}

	// Identity column starts at the same visible offset despite very different
	// window-name lengths — the alignment guarantee.
	o0, o1 := strings.Index(d0, "L ENG-1"), strings.Index(d1, "L ENG-2")
	if o0 < 0 || o1 < 0 {
		t.Fatalf("identity not found: %q / %q", d0, d1)
	}
	if c0, c1 := visibleWidth(d0[:o0]), visibleWidth(d1[:o1]); c0 != c1 {
		t.Errorf("identity misaligned: row0 col %d, row1 col %d\n%q\n%q", c0, c1, d0, d1)
	}

	// PR badge tinted by check state.
	if !strings.Contains(rows[0].display, ansiFg("#a6e3a1")) {
		t.Error("passing PR badge not green")
	}
	if !strings.Contains(rows[1].display, ansiFg("#f38ba8")) {
		t.Error("failing PR badge not red")
	}

	// Searchable by issue id and PR number.
	if !strings.Contains(rows[0].searchText, "ENG-1") || !strings.Contains(rows[0].searchText, "#10") {
		t.Errorf("row0 not searchable by id/pr: %q", rows[0].searchText)
	}
}

func TestBranchEchoesName(t *testing.T) {
	cases := []struct {
		branch, name string
		want         bool
	}{
		{"feat/5-window-picker-enrich", "feat-5-window-picker-enrich", true}, // worktree dir
		{"mono", "mono", true},             // exact
		{"feat/login", "feat-login", true}, // slash normalized
		{"feat/login", "mono", false},      // unrelated
		{"feat/a/b", "feat-a-b", true},     // multiple slashes
	}
	for _, c := range cases {
		if got := branchEchoesName(c.branch, c.name); got != c.want {
			t.Errorf("branchEchoesName(%q,%q) = %v, want %v", c.branch, c.name, got, c.want)
		}
	}
}

func TestColorPRBadge(t *testing.T) {
	c := prColors{success: "<s>", failure: "<f>", pending: "<p>", merged: "<m>", closed: "<c>", required: "<q>", underline: "<u>", reset: "<r>"}
	cases := []struct {
		name, prPlain, state, check, mergeable, review, autoMerge string
		want                                                      string
	}{
		{"no pr", "", "open", "success", "mergeable", "", "", ""},
		{"conflict wins over success", " G #1", "open", "success", "conflicting", "", "", "<f>G <r><f>#1<r>"},
		{"failing checks", " G #2", "open", "failure", "mergeable", "", "", "<f>G <r><f>#2<r>"},
		{"pending checks", " G #3", "open", "pending", "mergeable", "", "", "<p>G <r><p>#3<r>"},
		{"merged", " G #4", "merged", "success", "mergeable", "approved", "1", "<m>G <r><m>#4<r>"},
		{"merged wins over leftover failure", " G #7", "merged", "failure", "unknown", "", "", "<m>G <r><m>#7<r>"},
		{"closed", " G #8", "closed", "success", "mergeable", "", "", "<c>G <r><c>#8<r>"},
		{"clean success", " G #5", "open", "success", "mergeable", "", "", "<s>G <r><s>#5<r>"},
		{"approved tints the number", " G #9", "open", "pending", "mergeable", "approved", "", "<p>G <r><s>#9<r>"},
		{"changes requested", " G #10", "open", "success", "mergeable", "changes_requested", "", "<s>G <r><f>#10<r>"},
		{"review required", " G #11", "open", "success", "mergeable", "review_required", "", "<s>G <r><q>#11<r>"},
		{"auto-merge underlines", " G #12", "open", "success", "mergeable", "approved", "1", "<s>G <r><s><u>#12<r>"},
		{"draft prefix stays in the glyph half", " D G #13", "open", "success", "mergeable", "", "", "<s>D G <r><s>#13<r>"},
		{"no # keeps one colour", " weird", "open", "success", "mergeable", "approved", "", "<s>weird<r>"},
	}
	for _, c2 := range cases {
		if got := colorPRBadge(c2.prPlain, c2.state, c2.check, c2.mergeable, c2.review, c2.autoMerge, c); got != c2.want {
			t.Errorf("%s: got %q, want %q", c2.name, got, c2.want)
		}
	}
}
