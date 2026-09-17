package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestParseListKeysRows(t *testing.T) {
	t.Run("multiple tables with notes and command fallback", func(t *testing.T) {
		out := strings.Join([]string{
			`prefix|C-a |1|Pick session|s`,
			`prefix|C-a ||kill-window|x`,
			`root||1|Pick window|M-w`,
			`copy-mode-vi||1|Copy selection|Enter`,
		}, "\n")
		rows := parseListKeysRows(out)
		if len(rows) != 4 {
			t.Fatalf("len(rows) = %d, want 4", len(rows))
		}

		if rows[0].table != "prefix" || rows[0].keyString != "s" || rows[0].note != "Pick session" {
			t.Errorf("rows[0] = %+v, want table=prefix keyString=s note=%q", rows[0], "Pick session")
		}
		// prefix table: display key prepends the prefix chord.
		if rows[0].key != "C-a s" {
			t.Errorf("rows[0].key = %q, want %q", rows[0].key, "C-a s")
		}

		if !rows[0].described {
			t.Errorf("rows[0].described = false, want true for a -N bind")
		}

		// Row with no -N note falls back to key_command, and says so.
		if rows[1].note != "kill-window" {
			t.Errorf("rows[1].note = %q, want fallback to command %q", rows[1].note, "kill-window")
		}
		if rows[1].described {
			t.Errorf("rows[1].described = true, want false for a bind with no -N")
		}

		// Non-prefix tables render the bare key_string, no chord prepended.
		if rows[2].table != "root" || rows[2].key != "M-w" {
			t.Errorf("rows[2] = %+v, want table=root key=M-w", rows[2])
		}
		if rows[3].table != "copy-mode-vi" || rows[3].key != "Enter" {
			t.Errorf("rows[3] = %+v, want table=copy-mode-vi key=Enter", rows[3])
		}
	})

	t.Run("pipe inside note does not shift the row", func(t *testing.T) {
		// The "prefix Y ... | wl-copy" case CLAUDE.md flags: the note field's
		// own pipe must not be mistaken for a field separator. whichKeyListFormat
		// strips pipes from the note on the tmux side, but parseListKeysRows must
		// still not choke if one slips through, since it uses SplitN(5) with
		// key_string trailing.
		line := `prefix|C-a ||copy-pipe-and-cancel -T vi-copy  wl-copy|Y`
		rows := parseListKeysRows(line)
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1", len(rows))
		}
		if rows[0].keyString != "Y" {
			t.Errorf("keyString = %q, want %q (must not shift on note's internal content)", rows[0].keyString, "Y")
		}
		if rows[0].note != "copy-pipe-and-cancel -T vi-copy  wl-copy" {
			t.Errorf("note = %q, unexpected shift", rows[0].note)
		}
	})

	t.Run("malformed line is dropped without panicking", func(t *testing.T) {
		out := strings.Join([]string{
			`prefix|C-a |1|Pick session|s`,
			`this-line-has-too-few-fields`,
			`root||1|M-w`, // only 4 fields
			`onlytwo|fields`,
		}, "\n")
		rows := parseListKeysRows(out)
		// Only the first, well-formed row should survive.
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1 (malformed lines dropped): %+v", len(rows), rows)
		}
		if rows[0].keyString != "s" {
			t.Errorf("surviving row keyString = %q, want %q", rows[0].keyString, "s")
		}
	})

	t.Run("empty key_string drops the row", func(t *testing.T) {
		rows := parseListKeysRows(`prefix|C-a |1|some note|`)
		if len(rows) != 0 {
			t.Errorf("len(rows) = %d, want 0 for empty key_string", len(rows))
		}
	})

	t.Run("blank input returns nil", func(t *testing.T) {
		if rows := parseListKeysRows("  \n  "); rows != nil {
			t.Errorf("rows = %+v, want nil for blank input", rows)
		}
	})
}

func TestGroupWhichKeyRows(t *testing.T) {
	rows := []whichKeyRow{
		{table: "copy-mode-vi", keyString: "b", key: "b", note: "back"},
		{table: "root", keyString: "M-l", key: "M-l", note: "nav right"},
		{table: "prefix", keyString: "x", key: "C-a x", note: "kill-window"},
		{table: "another-table", keyString: "z", key: "z", note: "zzz"},
		{table: "root", keyString: "M-h", key: "M-h", note: "nav left"},
		{table: "prefix", keyString: "s", key: "C-a s", note: "pick session"},
	}

	groups := groupWhichKeyRows(rows)

	wantOrder := []string{"prefix", "root", "another-table", "copy-mode-vi"}
	if len(groups) != len(wantOrder) {
		t.Fatalf("len(groups) = %d, want %d: %+v", len(groups), len(wantOrder), groups)
	}
	for i, table := range wantOrder {
		if groups[i].table != table {
			t.Errorf("groups[%d].table = %q, want %q (order: prefix, root, then alphabetical)", i, groups[i].table, table)
		}
	}

	// Within-group ordering: sorted by key.
	prefixGroup := groups[0]
	if len(prefixGroup.rows) != 2 || prefixGroup.rows[0].key != "C-a s" || prefixGroup.rows[1].key != "C-a x" {
		t.Errorf("prefix group rows = %+v, want sorted by key (C-a s, C-a x)", prefixGroup.rows)
	}

	rootGroup := groups[1]
	if len(rootGroup.rows) != 2 || rootGroup.rows[0].key != "M-h" || rootGroup.rows[1].key != "M-l" {
		t.Errorf("root group rows = %+v, want sorted by key (M-h, M-l)", rootGroup.rows)
	}
}

func TestWhichKeyTableRank(t *testing.T) {
	cases := map[string]int{
		"prefix":        0,
		"root":          1,
		"copy-mode-vi":  2,
		"anything-else": 2,
	}
	for table, want := range cases {
		if got := whichKeyTableRank(table); got != want {
			t.Errorf("whichKeyTableRank(%q) = %d, want %d", table, got, want)
		}
	}
}

func TestRebuildVisibleFiltering(t *testing.T) {
	rows := []whichKeyRow{
		{table: "prefix", keyString: "s", key: "C-a s", note: "Pick session"},
		{table: "prefix", keyString: "w", key: "C-a w", note: "Pick window"},
		{table: "root", keyString: "M-l", key: "M-l", note: "Move right"},
	}
	m := newWhichKeyModel(rows, map[string]string{}, "mocha", "%0")

	t.Run("query matches key+note via fuzzyScore", func(t *testing.T) {
		m := m
		m.query = "session"
		m = m.rebuildVisible()

		var gotRows []whichKeyRow
		for _, item := range m.visible {
			if !item.isHeader {
				gotRows = append(gotRows, item.row)
			}
		}
		if len(gotRows) != 1 || gotRows[0].note != "Pick session" {
			t.Errorf("visible rows = %+v, want exactly the Pick session row", gotRows)
		}
	})

	t.Run("query with no match yields no visible rows", func(t *testing.T) {
		m := m
		m.query = "zzz-nonexistent-zzz"
		m = m.rebuildVisible()

		for _, item := range m.visible {
			if !item.isHeader {
				t.Errorf("visible = %+v, want no rows for a non-matching query", m.visible)
				break
			}
		}
	})

	t.Run("empty query keeps every row, grouped with headers", func(t *testing.T) {
		m := m
		m.query = ""
		m = m.rebuildVisible()

		var rowCount, headerCount int
		for _, item := range m.visible {
			if item.isHeader {
				headerCount++
			} else {
				rowCount++
			}
		}
		if rowCount != len(rows) {
			t.Errorf("rowCount = %d, want %d", rowCount, len(rows))
		}
		if headerCount != 2 { // prefix, root
			t.Errorf("headerCount = %d, want 2", headerCount)
		}
	})
}

func TestStripBindKeyPrefix(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    string
		wantErr bool
	}{
		{
			name: "note and no flags otherwise",
			line: `bind-key -T prefix -N 'Pick session' s run-shell -b '/nix/store/x/bin/tmux-session-picker'`,
			want: `run-shell -b '/nix/store/x/bin/tmux-session-picker'`,
		},
		{
			name: "repeatable flag -r",
			line: `bind-key -T prefix -r -N 'Resize pane left' H resize-pane -L 1`,
			want: `resize-pane -L 1`,
		},
		{
			name: "both -r and -N",
			line: `bind-key -T root -r -N 'Move window selection down' M-J run-shell 'tmux-window-nav down'`,
			want: `run-shell 'tmux-window-nav down'`,
		},
		{
			name: "neither -r nor -N",
			line: `bind-key -T prefix x kill-window`,
			want: `kill-window`,
		},
		{
			name: "escaped semicolon key token",
			line: `bind-key -T copy-mode-vi \; send -X search-again`,
			want: `send -X search-again`,
		},
		{
			name:    "malformed: not a bind-key line",
			line:    `set-option -g status on`,
			wantErr: true,
		},
		{
			name:    "malformed: no command after key",
			line:    `bind-key -T prefix -N 'note' s`,
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := stripBindKeyPrefix(c.line)
			if c.wantErr {
				if err == nil {
					t.Fatalf("stripBindKeyPrefix(%q) = %q, nil, want a non-nil error", c.line, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stripBindKeyPrefix(%q) unexpected error: %v", c.line, err)
			}
			if got != c.want {
				t.Errorf("stripBindKeyPrefix(%q) = %q, want %q", c.line, got, c.want)
			}
		})
	}
}

func TestNextBindToken(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		from     int
		wantTok  string
		wantMore bool
	}{
		{name: "simple word", line: "bind-key s pick-session", from: 0, wantTok: "bind-key", wantMore: true},
		{name: "quoted note with spaces", line: `-N 'Pick session' s`, from: 3, wantTok: "Pick session", wantMore: true},
		{name: "escaped semicolon", line: `\; send -X search-again`, from: 0, wantTok: ";", wantMore: true},
		{name: "double-quoted token", line: `-F "a and b" x`, from: 3, wantTok: "a and b", wantMore: true},
		{name: "end of string returns false", line: "bind-key", from: 8, wantTok: "", wantMore: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, _, ok := nextBindToken(c.line, c.from)
			if ok != c.wantMore {
				t.Fatalf("nextBindToken(%q, %d) ok = %v, want %v", c.line, c.from, ok, c.wantMore)
			}
			if ok && tok != c.wantTok {
				t.Errorf("nextBindToken(%q, %d) = %q, want %q", c.line, c.from, tok, c.wantTok)
			}
		})
	}

	t.Run("advances position past the token", func(t *testing.T) {
		line := "bind-key s pick-session"
		tok, pos, ok := nextBindToken(line, 0)
		if !ok || tok != "bind-key" {
			t.Fatalf("first token = %q, %v, want bind-key, true", tok, ok)
		}
		tok2, _, ok2 := nextBindToken(line, pos)
		if !ok2 || tok2 != "s" {
			t.Errorf("second token = %q, %v, want s, true", tok2, ok2)
		}
	})
}

func TestRebuildVisibleRanking(t *testing.T) {
	// The #689 shape: two described binds and one whose note is the raw
	// key_command a /nix/store path lives in — the command subsequence-matches
	// "float" (…f…l…o…a…t…) with nothing on screen to explain why.
	rows := []whichKeyRow{
		{table: "prefix", keyString: "*", key: "C-a *", note: "New floating pane", described: true},
		{table: "prefix", keyString: "@", key: "C-a @", note: "Toggle pane between floating and tiled", described: true},
		{table: "fingers", keyString: "f", key: "f", note: `run-shell -b "/nix/store/abc-fingers/bin/tmux-fingers start"`},
	}
	m := newWhichKeyModel(rows, map[string]string{}, "mocha", "%0")
	m.query = "float"
	m = m.rebuildVisible()

	if len(m.visible) != 3 {
		t.Fatalf("len(visible) = %d, want 3 flat rows: %+v", len(m.visible), m.visible)
	}
	for i, item := range m.visible {
		if item.isHeader {
			t.Fatalf("visible[%d] is a header — a filtered list is flat, not grouped", i)
		}
	}
	if m.visible[0].row.keyString != "*" {
		t.Errorf("visible[0] = %q, want the best-scoring described bind (*)", m.visible[0].row.keyString)
	}
	if m.visible[2].row.described || m.visible[2].row.keyString != "f" {
		t.Errorf("visible[2] = %+v, want the undescribed command row ranked last", m.visible[2].row)
	}
}

func TestRebuildVisibleUnfilteredStaysGrouped(t *testing.T) {
	rows := []whichKeyRow{
		{table: "prefix", keyString: "s", key: "C-a s", note: "Pick session", described: true},
		{table: "root", keyString: "M-l", key: "M-l", note: "Move right", described: true},
	}
	m := newWhichKeyModel(rows, map[string]string{}, "mocha", "%0")

	if len(m.visible) != 4 {
		t.Fatalf("len(visible) = %d, want 2 headers + 2 rows: %+v", len(m.visible), m.visible)
	}
	if !m.visible[0].isHeader || m.visible[0].table != "prefix" {
		t.Errorf("visible[0] = %+v, want the prefix header", m.visible[0])
	}
	if !m.visible[2].isHeader || m.visible[2].table != "root" {
		t.Errorf("visible[2] = %+v, want the root header", m.visible[2])
	}
}

func TestWhichKeyTableGloss(t *testing.T) {
	cases := []struct {
		table, prefixKey, want string
	}{
		{"prefix", "`", "after `"},
		{"prefix", "", "after the prefix key"},
		{"root", "`", "no prefix"},
		{"copy-mode-vi", "`", "in copy mode"},
		{"some-plugin-table", "`", ""},
	}
	for _, c := range cases {
		if got := whichKeyTableGloss(c.table, c.prefixKey); got != c.want {
			t.Errorf("whichKeyTableGloss(%q, %q) = %q, want %q", c.table, c.prefixKey, got, c.want)
		}
	}
}

func TestWhichKeyQueryAcceptsSpace(t *testing.T) {
	rows := []whichKeyRow{
		{table: "prefix", keyString: "c", key: "C-a c", note: "New window", described: true},
		{table: "prefix", keyString: "N", key: "C-a N", note: "Create new session", described: true},
	}
	m := newWhichKeyModel(rows, map[string]string{}, "mocha", "%0")

	for _, code := range []rune{'n', 'e', 'w', tea.KeySpace, 'w'} {
		next, _ := m.handleKey(tea.KeyPressMsg{Code: code})
		m = next.(whichKeyModel)
	}

	if m.query != "new w" {
		t.Fatalf("query = %q, want %q", m.query, "new w")
	}
	if len(m.visible) != 1 || m.visible[0].row.keyString != "c" {
		t.Errorf("visible = %+v, want only the New window bind", m.visible)
	}
}
