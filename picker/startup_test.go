package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCollectTmuxArgv(t *testing.T) {
	sep := []string{";", "display-message", "-p", startupSep, ";"}

	session := collectTmuxArgv(false, true)
	if session[0] != "show" || !slices.Contains(session, "list-panes") {
		t.Errorf("session argv = %q, want show ... list-panes", session)
	}
	if slices.Contains(session, "list-sessions") {
		t.Error("session mode reads window rows and session activity it never uses")
	}

	window := collectTmuxArgv(true, true)
	for _, cmd := range []string{"show", "list-panes", "list-sessions"} {
		if !slices.Contains(window, cmd) {
			t.Errorf("window argv lacks %s: %q", cmd, window)
		}
	}
	if got := strings.Count(strings.Join(window, " "), strings.Join(sep, " ")); got != 3 {
		t.Errorf("window argv has %d separators, want 3", got)
	}
	// The separators must sit between the commands, in this order.
	want := []string{"show", "list-panes", "list-panes", "list-sessions"}
	var got []string
	for _, a := range window {
		if slices.Contains(want, a) && (len(got) == 0 || got[len(got)-1] != a || a == "list-panes") {
			got = append(got, a)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("command order = %v, want %v", got, want)
	}
}

func TestParseTmuxDataSession(t *testing.T) {
	out := "@catppuccin_flavor mocha\n@picker_layout list\n" + startupSep + "\n%1|s1|0|/p|1|||fish|1|||\n%2|s2|0|/q|2|||vim|2|||\n"
	d, ok := parseTmuxData(out, false, true)
	if !ok {
		t.Fatal("parseTmuxData declined a well-formed session read")
	}
	if d.opts["@catppuccin_flavor"] != "mocha" || d.opts["@picker_layout"] != "list" {
		t.Errorf("opts = %v", d.opts)
	}
	if len(d.snap) != 2 || !strings.HasPrefix(d.snap[1], "%2|s2") {
		t.Errorf("snap = %q", d.snap)
	}
	if d.windowRows != nil || d.activity != nil {
		t.Error("session mode filled window-only fields")
	}
}

func TestParseTmuxDataWindow(t *testing.T) {
	out := "@a b\n" + startupSep + "\n%1|s1|0|/p|1|||fish|1|||\n" + startupSep + "\nwin-row-1\nwin-row-2\n" + startupSep + "\ns1|10\ns2|20\n"
	d, ok := parseTmuxData(out, true, true)
	if !ok {
		t.Fatal("parseTmuxData declined a well-formed window read")
	}
	if !slices.Equal(d.windowRows, []string{"win-row-1", "win-row-2"}) {
		t.Errorf("windowRows = %q", d.windowRows)
	}
	if d.activity["s1"] != 10 || d.activity["s2"] != 20 {
		t.Errorf("activity = %v", d.activity)
	}
}

func TestParseTmuxDataRejectsWrongShape(t *testing.T) {
	// A missing or extra separator means the outputs cannot be attributed;
	// the caller falls back to the separate reads.
	if _, ok := parseTmuxData("only options\n", false, true); ok {
		t.Error("accepted a read with no separator")
	}
	if _, ok := parseTmuxData("a\n"+startupSep+"\nb\n"+startupSep+"\nc\n", false, true); ok {
		t.Error("accepted an extra separator")
	}
	if _, ok := parseTmuxData("a\n"+startupSep+"\nb\n", true, true); ok {
		t.Error("window mode accepted a session-shaped read")
	}
}

func TestParseTmuxDataSeparatorInsideAValueIsNotASeparator(t *testing.T) {
	out := "@note before " + startupSep + " after\n" + startupSep + "\n%1|s1|0|/p|1|||fish|1|||\n"
	d, ok := parseTmuxData(out, false, true)
	if !ok {
		t.Fatal("parseTmuxData declined")
	}
	if d.opts["@note"] != "before "+startupSep+" after" {
		t.Errorf("@note = %q", d.opts["@note"])
	}
}

func TestCollectTmuxArgvWithoutOptions(t *testing.T) {
	// The 1s refresh chains the pane reads but not show -g.
	if a := collectTmuxArgv(false, false); a[0] != "list-panes" || slices.Contains(a, "show") {
		t.Errorf("session refresh argv = %q, want a bare list-panes", a)
	}
	w := collectTmuxArgv(true, false)
	if w[0] != "list-panes" || slices.Contains(w, "show") || !slices.Contains(w, "list-sessions") {
		t.Errorf("window refresh argv = %q, want list-panes ... list-sessions without show", w)
	}
	if got := strings.Count(strings.Join(w, " "), startupSep); got != 2 {
		t.Errorf("window refresh argv has %d separators, want 2", got)
	}
}

func TestParseTmuxDataWithoutOptions(t *testing.T) {
	out := "%1|s1|0|/p|1|||fish|1|||\n" + startupSep + "\nwin-row\n" + startupSep + "\ns1|10\n"
	d, ok := parseTmuxData(out, true, false)
	if !ok {
		t.Fatal("declined a well-formed refresh read")
	}
	if d.opts != nil || len(d.snap) != 1 || !slices.Equal(d.windowRows, []string{"win-row"}) || d.activity["s1"] != 10 {
		t.Errorf("parsed %+v", d)
	}
	if _, ok := parseTmuxData("%1|s1|0|/p|1|||fish|1|||\n", false, false); !ok {
		t.Error("declined a session refresh read (snapshot only)")
	}
}

// stubBin writes an executable shell script named name into dir.
func stubBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDumpFirstFrameChainsTmuxReads runs the headless first-frame path against
// a stub tmux that answers the chained read. It pins the fork budget the
// picker's open latency depends on: one tmux call in session mode, and one in
// window mode, where the missing branches come from HEAD rather than git.
func TestDumpFirstFrameChainsTmuxReads(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	work := t.TempDir()
	writeFile(t, filepath.Join(work, "repo/.git/HEAD"), "ref: refs/heads/feat/874-fast\n")
	writeFile(t, filepath.Join(work, "repo/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(work, "repo/.git/refs/.keep"), "")
	plain := filepath.Join(work, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	paneRow := func(id, sess string) string {
		return strings.Join([]string{id, sess, "0", plain, "100", "", "fish", "0", "", "", ""}, "|")
	}
	winRow := func(sess, path string) string {
		return windowPaneRow(sess, "0", "name", "0", "fish", "1", "", path)
	}

	stubBin(t, bin, "tmux", `echo "$*" >> `+calls+`
case "$*" in
*list-sessions*) act=1 ;; *) act=0 ;;
esac
echo "@catppuccin_flavor mocha"
echo "`+startupSep+`"
echo "`+paneRow("%1", "alpha")+`"
echo "`+paneRow("%2", "beta")+`"
if [ "$act" = 1 ]; then
echo "`+startupSep+`"
echo "`+winRow("alpha", filepath.Join(work, "repo"))+`"
echo "`+winRow("beta", plain)+`"
echo "`+startupSep+`"
echo "alpha|200"
echo "beta|100"
fi
`)
	stubBin(t, bin, "git", `echo git >> `+calls+`
`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("CLAUDE_STATUS_DIR", t.TempDir())
	t.Setenv("OG_PICKER_DUMP_SIZE", "100x30")
	clearDiscoveryVars(t)

	for _, tc := range []struct {
		name   string
		window bool
		want   []string
	}{
		{"session", false, []string{"alpha", "beta"}},
		{"window", true, []string{"alpha", "feat/874-fast", "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(calls)
			var out bytes.Buffer
			if err := dumpFirstFrame(&out, tc.window, false, false); err != nil {
				t.Fatal(err)
			}
			frame := out.String()
			at := 0
			for _, want := range tc.want {
				i := strings.Index(frame[at:], want)
				if i < 0 {
					t.Fatalf("frame lacks %q after offset %d:\n%s", want, at, frame)
				}
				at += i + len(want)
			}
			b, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != 1 || strings.HasPrefix(lines[0], "git") {
				t.Errorf("first paint ran %d commands, want exactly one tmux call:\n%s", len(lines), b)
			}
		})
	}
}
