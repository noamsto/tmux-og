package daemon

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseWindowLabels(t *testing.T) {
	// The format is the third leg the constant has to hold together, beside the
	// parser's SplitN and oneRow's fixture — a field added to one and not the
	// others is a shifted row stamping wrong values under the existing names.
	// Every position boundary is "}|#{"; a plain '|' count would also find the
	// ones inside each wrapper's [|] bracket expression.
	if n := strings.Count(windowLabelFormat, "}|#{") + 1; n != windowLabelFields {
		t.Fatalf("windowLabelFormat carries %d fields, want %d", n, windowLabelFields)
	}

	// One full row, spelled out to document the wire shape.
	full := "@1|nova|#89b4fa|123|open|success|mergeable| PR #123|" +
		"github|460|https://github.com/o/r/issues/460|https://github.com/o/r/pull/123|1|" +
		"feat/460-card|/home/noams/wt/card|Card reads bridge state|Ship the card|" +
		"approved|1|3/8|GH #460| ship it"
	// The remote's #{s/[|]/ /:…} has already run over positions 8-16, so a title
	// that held a '|' arrives carrying a space and shifts nothing. Only the
	// trailing @window_label_rest_long is unwrapped, where a '|' lands in itself.
	piped := "@2|orbit|#[fg=red]|none|OPEN|success|unknown|||||||||a  piped title|||||| a #[fg=red]title | with a pipe"
	if n := strings.Count(full, "|") + 1; n != windowLabelFields {
		t.Fatalf("full fixture has %d fields, want %d", n, windowLabelFields)
	}
	// piped carries an extra '|' inside its trailing field on purpose, so it is
	// counted the way the parser counts: everything past the last separator is
	// field 18.
	if n := len(strings.SplitN(piped, "|", windowLabelFields)); n != windowLabelFields {
		t.Fatalf("piped fixture splits into %d fields, want %d", n, windowLabelFields)
	}

	body := strings.Join([]string{
		full,
		piped,
		"@3|zephyr", // trailing empty fields may not survive the trip
		"",          // blank line
	}, "\n")

	got := parseWindowLabels(body)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(got), got)
	}

	want0 := labelRow{
		id: "@1", crewName: "nova", crewColor: "#89b4fa", prNumber: "123",
		prState: "open", prCheck: "success", prMergeable: "mergeable",
		// The leading space of @window_pr_plain is load-bearing for reflow's
		// pr_colw padding, so nothing trims it.
		prPlain:       " PR #123",
		issueProvider: "github", issueID: "460",
		issueURL: "https://github.com/o/r/issues/460",
		prURL:    "https://github.com/o/r/pull/123",
		prDraft:  "1", branch: "feat/460-card", dir: "/home/noams/wt/card",
		issueTitle: "Card reads bridge state", prTitle: "Ship the card",
		prReview: "approved", prAutoMerge: "1", prProgress: "3/8",
		labelID: "GH #460", labelRest: " ship it",
	}
	if got[0] != want0 {
		t.Errorf("row 0 = %+v, want %+v", got[0], want0)
	}

	// The unwrapped field is last, so a '|' inside it shifts nothing: the enum
	// fields either side still read correctly, and a title that already lost its
	// pipes remotely arrives whole.
	want1 := labelRow{
		id: "@2", crewName: "orbit", prCheck: "success", prMergeable: "unknown",
		issueTitle: "a  piped title",
		labelRest:  " a title  with a pipe",
	}
	if got[1] != want1 {
		t.Errorf("row 1 = %+v, want %+v", got[1], want1)
	}

	if (got[2] != labelRow{id: "@3", crewName: "zephyr"}) {
		t.Errorf("short row = %+v", got[2])
	}
}

// oneRow parses a single-window body whose field i holds v.
func oneRow(t *testing.T, i int, v string) labelRow {
	t.Helper()
	fields := make([]string, windowLabelFields)
	fields[0] = "@1"
	fields[i] = v
	rows := parseWindowLabels(strings.Join(fields, "|"))
	if len(rows) != 1 {
		t.Fatalf("parseWindowLabels(%q) = %d rows, want 1", v, len(rows))
	}
	return rows[0]
}

func TestWindowLabelValidation(t *testing.T) {
	colors := []struct{ in, want string }{
		{"#89b4fa", "#89b4fa"},
		{"#89B4FA", "#89B4FA"}, // ansiFg takes either case; lowercase-only would drop it to the fallback
		{"colour42", "colour42"},
		{"colour256", ""},
		{"red", "red"},
		{"#[fg=red]", ""},
		{"#89b4f", ""},
	}
	for _, c := range colors {
		if got := oneRow(t, 2, c.in).crewColor; got != c.want {
			t.Errorf("crewColor(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	numbers := []struct{ in, want string }{{"123", "123"}, {"none", ""}, {"12a", ""}}
	for _, n := range numbers {
		if got := oneRow(t, 3, n.in).prNumber; got != n.want {
			t.Errorf("prNumber(%q) = %q, want %q", n.in, got, n.want)
		}
	}

	states := []struct{ in, want string }{{"open", "open"}, {"OPEN", ""}, {"open pr", ""}, {"", ""}}
	for _, s := range states {
		if got := oneRow(t, 4, s.in).prState; got != s.want {
			t.Errorf("prState(%q) = %q, want %q", s.in, got, s.want)
		}
	}

	// Every identity field: the validator, a DROP rather than a cut at the cap,
	// and a DROP rather than a strip for #[…] markup. Every markup case below is
	// chosen to PASS its own validator once stripWindowName has deleted the
	// markup, so each one is only caught by the before/after compare — a cut URL
	// opens the wrong page, and so does a de-markup'd one.
	identity := []struct {
		name  string
		field int
		cases []struct{ in, want string }
	}{
		{"issueProvider", 8, []struct{ in, want string }{
			{"github", "github"}, {"linear", "linear"},
			{"GitHub", ""}, {"git hub", ""},
			{"git#[x]hub", ""}, // strips to a valid "github"
			{strings.Repeat("a", providerMaxRunes), strings.Repeat("a", providerMaxRunes)},
			{strings.Repeat("a", providerMaxRunes+1), ""},
		}},
		{"issueID", 9, []struct{ in, want string }{
			{"LZT-123", "LZT-123"}, {"460", "460"}, {"a_b-C9", "a_b-C9"},
			{"has space", ""}, {"has/slash", ""},
			{"LZT#[x]-123", ""}, // strips to a valid "LZT-123"
			{strings.Repeat("a", issueIDMaxRunes+1), ""},
		}},
		{"issueURL", 10, []struct{ in, want string }{
			{"https://linear.app/x/issue/LZT-1/a", "https://linear.app/x/issue/LZT-1/a"},
			{"http://x.test/a", "http://x.test/a"},
			{"ftp://x.test/a", ""}, {"/local/path", ""}, {"https://x.test/a b", ""},
			// Strips to "https://x.test/ac" — a different, still-valid URL.
			{"https://x.test/a#[b]c", ""},
			{"https://x.test/a\x01c", ""}, // and a control byte, same route
			{"https://x.test/" + strings.Repeat("a", urlMaxRunes-14), ""},
		}},
		{"prURL", 11, []struct{ in, want string }{
			{"https://github.com/o/r/pull/1", "https://github.com/o/r/pull/1"},
			{"github.com/o/r/pull/1", ""},
			{"https://github.com/o/r#[x]/pull/1", ""},
		}},
		{"prDraft", 12, []struct{ in, want string }{
			{"1", "1"}, {"0", ""}, {"", ""}, {"11", ""}, {"true", ""},
			{"1#[x]", ""}, // strips to a valid "1"
		}},
		{"branch", 13, []struct{ in, want string }{
			{"feat/598-card", "feat/598-card"},
			{"has space", ""}, {"", ""},
			{"feat#[x]/598-card", ""}, // strips to a valid "feat/598-card"
			{strings.Repeat("a", branchMaxRunes), strings.Repeat("a", branchMaxRunes)},
			{strings.Repeat("a", branchMaxRunes+1), ""},
		}},
		{"dir", 14, []struct{ in, want string }{
			{"/home/noams/wt/card", "/home/noams/wt/card"}, {"/", "/"},
			{"relative/path", ""}, {"~/wt", ""},
			// A legal worktree path containing a space is rejected: an absent
			// dir line is correct-but-incomplete, an unquoted path is not.
			{"/home/my wt", ""},
			{"/home#[x]/wt", ""}, // strips to a valid "/home/wt"
			{"/" + strings.Repeat("a", dirMaxRunes), ""},
		}},
	}
	for _, f := range identity {
		for _, c := range f.cases {
			got := rowField(t, oneRow(t, f.field, c.in), f.field)
			if got != c.want {
				t.Errorf("%s(%q) = %q, want %q", f.name, c.in, got, c.want)
			}
		}
	}

	// Caps count runes, so one pathological remote value cannot dominate a column.
	if got := []rune(oneRow(t, 1, strings.Repeat("é", 200)).crewName); len(got) != crewNameMaxRunes {
		t.Errorf("crewName capped to %d runes, want %d", len(got), crewNameMaxRunes)
	}
	// The titles and the label segments are display text, so they strip markup
	// and truncate where the identity fields above drop whole. Dropping a title
	// for holding markup would regress what already ships.
	for _, f := range []int{15, 16, 20, 21} {
		if got := rowField(t, oneRow(t, f, "a #[fg=red]title"), f); got != "a title" {
			t.Errorf("field %d: markup value = %q, want it stripped and kept", f, got)
		}
	}
	for f, limit := range map[int]int{15: labelTitleMaxRunes, 16: labelTitleMaxRunes, 20: labelTextMaxRunes, 21: labelTitleMaxRunes} {
		got := []rune(rowField(t, oneRow(t, f, strings.Repeat("x", 300)), f))
		if len(got) != limit {
			t.Errorf("field %d capped to %d runes, want %d", f, len(got), limit)
		}
	}

	// LocalTmux execs without a shell, so tmux's own args_parse would read this
	// as a flag. The titles have no validator to fall back on, so this drop and
	// the ';' one below are their only guard.
	for _, f := range []int{15, 16, 20, 21} {
		if got := rowField(t, oneRow(t, f, "-n oops"), f); got != "" {
			t.Errorf("field %d: flag-shaped value = %q, want it dropped", f, got)
		}
	}

	// A lone ';' is the separator apply joins its per-window sequence with:
	// tmux fails the whole batch on it and drops every later option in the
	// sequence. One *inside* a value is not a separator and is kept.
	for _, f := range []int{1, 7, 15, 16, 20, 21} {
		if got := rowField(t, oneRow(t, f, ";"), f); got != "" {
			t.Errorf("field %d: lone ';' = %q, want it dropped", f, got)
		}
	}
	if got := oneRow(t, 21, " a;b").labelRest; got != " a;b" {
		t.Errorf("labelRest(%q) = %q, want it kept", " a;b", got)
	}
}

// rowField reads the field oneRow placed at index i. Exhaustive on purpose, and
// fatal on an unmapped index: a default arm here is how a field that MOVES turns
// every index-keyed assertion above into a vacuous pass against an empty field.
func rowField(t *testing.T, r labelRow, i int) string {
	t.Helper()
	switch i {
	case 1:
		return r.crewName
	case 2:
		return r.crewColor
	case 3:
		return r.prNumber
	case 4:
		return r.prState
	case 5:
		return r.prCheck
	case 6:
		return r.prMergeable
	case 7:
		return r.prPlain
	case 8:
		return r.issueProvider
	case 9:
		return r.issueID
	case 10:
		return r.issueURL
	case 11:
		return r.prURL
	case 12:
		return r.prDraft
	case 13:
		return r.branch
	case 14:
		return r.dir
	case 15:
		return r.issueTitle
	case 16:
		return r.prTitle
	case 17:
		return r.prReview
	case 18:
		return r.prAutoMerge
	case 19:
		return r.prProgress
	case 20:
		return r.labelID
	case 21:
		return r.labelRest
	}
	t.Fatalf("rowField: index %d is not mapped to a labelRow field", i)
	return ""
}

func TestLabelShipperApply(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	s := newLabelShipper()
	var calls [][]string
	cfg := mirrorCfg(&calls)

	row := labelRow{id: "@1", crewName: "nova", crewColor: "#89b4fa", labelID: "GH #460", labelRest: " ship it"}
	if !s.apply(cfg, reg, []labelRow{row, {id: "@9", crewName: "ghost"}}) {
		t.Fatal("first pass should report a change")
	}
	if len(calls) != 1 {
		t.Fatalf("first pass = %d tmux calls, want one argv sequence: %v", len(calls), calls)
	}
	first := strings.Join(calls[0], " ")
	if n := strings.Count(first, "set-option"); n != len(bridgeLabelOptions) {
		t.Errorf("first pass wrote %d options, want all %d: %q", n, len(bridgeLabelOptions), first)
	}
	if n := strings.Count(first, " ; "); n != len(bridgeLabelOptions)-1 {
		t.Errorf("commands are not one ';'-joined sequence: %q", first)
	}
	if !strings.Contains(first, "set-option -w -t @101 @bridge_crew_name nova") {
		t.Errorf("carried value not stamped: %q", first)
	}
	// An absent remote value unsets rather than stamping "".
	if !strings.Contains(first, "set-option -w -t @101 -u @bridge_pr_number") {
		t.Errorf("empty value not unset: %q", first)
	}
	if strings.Contains(first, "ghost") {
		t.Errorf("a row with no mirror window was stamped: %q", first)
	}

	if s.apply(cfg, reg, []labelRow{row}) {
		t.Error("an unchanged row reported a change")
	}
	if len(calls) != 1 {
		t.Errorf("an unchanged row issued tmux calls: %v", calls[1:])
	}

	row.crewName = "orbit"
	s.apply(cfg, reg, []labelRow{row})
	want := []string{"set-option", "-w", "-t", "@101", "@bridge_crew_name", "orbit"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
		t.Errorf("changed field wrote %v, want only %v", calls[1:], want)
	}

	row.crewColor = ""
	s.apply(cfg, reg, []labelRow{row})
	want = []string{"set-option", "-w", "-t", "@101", "-u", "@bridge_crew_color"}
	if len(calls) != 3 || !reflect.DeepEqual(calls[2], want) {
		t.Errorf("emptied field wrote %v, want only %v", calls[2:], want)
	}

	// Forgetting is keyed on the registry, not on absence from the reply.
	reg.remove("@1")
	s.apply(cfg, reg, nil)
	if len(s.written) != 0 {
		t.Errorf("written = %v, want the departed window forgotten", s.written)
	}
}

// Teardown ends in kill-session, so clear is near-vacuous in production and
// this is the only place it is provable.
func TestLabelShipperClear(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	s := newLabelShipper()
	var calls [][]string
	cfg := mirrorCfg(&calls)
	s.apply(cfg, reg, []labelRow{{id: "@1", crewName: "nova"}})

	calls = nil
	s.clear(cfg, reg)
	if len(calls) != 1 {
		t.Fatalf("clear = %d tmux calls, want one argv sequence: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	for _, o := range bridgeLabelOptions {
		if !strings.Contains(got, "-t @101 -u "+o.opt) {
			t.Errorf("clear left %s set: %q", o.opt, got)
		}
	}
	if len(s.written) != 0 {
		t.Errorf("written = %v, want empty", s.written)
	}
}

// retireMirror rebuilds a dead mirror through closeWindow + reconcileWindows,
// which re-adds the SAME remote id against a fresh local window. The row is
// unchanged across that, so only the local target tells the shipper it must
// stamp again — otherwise the replacement window renders bare forever.
func TestLabelShipperRestampsRebuiltMirror(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	s := newLabelShipper()
	var calls [][]string
	cfg := mirrorCfg(&calls)

	row := labelRow{id: "@1", crewName: "nova", labelID: "GH #460"}
	s.apply(cfg, reg, []labelRow{row})

	reg.remove("@1")
	reg.add("@1", "@102")
	calls = nil
	if !s.apply(cfg, reg, []labelRow{row}) {
		t.Fatal("a rebuilt mirror reported no change")
	}
	if len(calls) != 1 {
		t.Fatalf("rebuilt mirror = %d tmux calls, want one argv sequence: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	if n := strings.Count(got, "set-option"); n != len(bridgeLabelOptions) {
		t.Errorf("rebuilt mirror wrote %d options, want all %d: %q", n, len(bridgeLabelOptions), got)
	}
	if strings.Contains(got, "@101") {
		t.Errorf("stamped the dead local window: %q", got)
	}
	if !strings.Contains(got, "set-option -w -t @102 @bridge_crew_name nova") {
		t.Errorf("replacement window not stamped: %q", got)
	}
}

func TestWindowLabelReviewAutoMergeProgress(t *testing.T) {
	reviews := []struct{ in, want string }{
		{"approved", "approved"}, {"changes_requested", "changes_requested"},
		{"review_required", "review_required"}, {"APPROVED", ""}, {"maybe", ""}, {"", ""},
	}
	for _, c := range reviews {
		if got := oneRow(t, 17, c.in).prReview; got != c.want {
			t.Errorf("prReview(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	autos := []struct{ in, want string }{{"1", "1"}, {"true", ""}, {"", ""}}
	for _, c := range autos {
		if got := oneRow(t, 18, c.in).prAutoMerge; got != c.want {
			t.Errorf("prAutoMerge(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	progress := []struct{ in, want string }{
		{"3/8", "3/8"}, {"0/1", "0/1"}, {"3/", ""}, {"a/b", ""}, {"3 / 8", ""}, {"-3/8", ""},
	}
	for _, c := range progress {
		if got := oneRow(t, 19, c.in).prProgress; got != c.want {
			t.Errorf("prProgress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
