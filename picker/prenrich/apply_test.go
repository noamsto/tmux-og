package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

const prevFormat = "#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_review}|#{@pr_auto_merge}|#{@pr_check_progress}|#{@pr_glyph}"

var testIcons = enrichstate.Icons{Pending: "P", Success: "S", Failure: "F", Merged: "M", Closed: "X", Conflict: "C", Draft: "D"}

func writeCache(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func setOpts(calls []call) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c.args) > 0 && c.args[0] == "set-option" {
			out = append(out, c.args)
		}
	}
	return out
}

func TestApplyCache_MissingFile(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	if got := e.applyCache("T", filepath.Join(e.cacheDir, "nope.json"), "br"); got != "" {
		t.Errorf("applied = %q", got)
	}
	if n := len(f.snapshot()); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
}

func TestApplyCache_NoPR(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", `[]`)
	e.applyCache("T", p, "br")
	want := [][]string{
		{"set-option", "-t", "T", "-w", "@pr_number", "none"},
		{"set-option", "-t", "T", "-w", "@pr_title", ""},
		{"set-option", "-t", "T", "-w", "@pr_state", ""},
		{"set-option", "-t", "T", "-w", "@pr_check_state", ""},
		{"set-option", "-t", "T", "-w", "@pr_url", ""},
		{"set-option", "-t", "T", "-w", "@pr_mergeable", ""},
		{"set-option", "-t", "T", "-w", "@pr_draft", ""},
		{"set-option", "-t", "T", "-w", "@pr_review", ""},
		{"set-option", "-t", "T", "-w", "@pr_auto_merge", ""},
		{"set-option", "-t", "T", "-w", "@pr_check_progress", ""},
		{"set-option", "-t", "T", "-w", "@pr_glyph", ""},
		{"set-option", "-t", "T", "-w", "@pr_branch", "br"},
	}
	if got := setOpts(f.snapshot()); !reflect.DeepEqual(got, want) {
		t.Errorf("set-options =\n%v\nwant\n%v", got, want)
	}
}

const fullPR = `[{"number":7,"title":"Fix #1 'it' | now","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":true,"reviewDecision":"APPROVED","autoMergeRequest":{"enabledAt":"x"}}]`

const pendingRollup = `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]`

func TestApplyCache_FullPR(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", fullPR)
	writeCache(t, e.cacheDir, "k.checks.json", `[{"statusCheckRollup":`+pendingRollup+`,"headRefOid":"h"}]`)
	if got := e.applyCache("T", p, "br"); got != "pending" {
		t.Errorf("applied = %q", got)
	}
	glyph := enrichstate.Glyph(testIcons, "open", "pending", "mergeable", "1", "1/2")
	if glyph != "D "+enrichstate.PieSlices[3] {
		t.Fatalf("test glyph = %q", glyph)
	}
	var tmux []call
	for _, c := range f.snapshot() {
		if c.name == "tmux" {
			tmux = append(tmux, c)
		}
	}
	want := [][]string{
		{"display-message", "-t", "T", "-p", prevFormat},
		{"set-option", "-t", "T", "-w", "@pr_number", "7"},
		{"set-option", "-t", "T", "-w", "@pr_title", "Fix 1 it   now"},
		{"set-option", "-t", "T", "-w", "@pr_state", "open"},
		{"set-option", "-t", "T", "-w", "@pr_check_state", "pending"},
		{"set-option", "-t", "T", "-w", "@pr_url", "u"},
		{"set-option", "-t", "T", "-w", "@pr_mergeable", "mergeable"},
		{"set-option", "-t", "T", "-w", "@pr_draft", "1"},
		{"set-option", "-t", "T", "-w", "@pr_review", "approved"},
		{"set-option", "-t", "T", "-w", "@pr_auto_merge", "1"},
		{"set-option", "-t", "T", "-w", "@pr_check_progress", "1/2"},
		{"set-option", "-t", "T", "-w", "@pr_glyph", glyph},
		{"set-option", "-t", "T", "-w", "@pr_branch", "br"},
	}
	if len(tmux) < len(want) {
		t.Fatalf("tmux calls = %d, want >= %d", len(tmux), len(want))
	}
	for i, w := range want {
		if !reflect.DeepEqual(tmux[i].args, w) {
			t.Errorf("tmux call %d = %v, want %v", i, tmux[i].args, w)
		}
		if tmux[i].kind != "run" && tmux[i].kind != "output" {
			t.Errorf("tmux call %d kind = %s", i, tmux[i].kind)
		}
	}
	if tmux[0].kind != "output" {
		t.Errorf("display-message must be an output call, got %s", tmux[0].kind)
	}
}

func optionValue(calls []call, option string) (string, bool) {
	for _, c := range calls {
		if len(c.args) >= 6 && c.args[0] == "set-option" && c.args[4] == option {
			return c.args[5], true
		}
	}
	return "", false
}

func TestApplyCache_LegacyInlineRollup(t *testing.T) {
	inline := `[{"number":7,"title":"t","url":"u","state":"OPEN","statusCheckRollup":` + pendingRollup + `}]`
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", inline)
	if got := e.applyCache("T", p, "br"); got != "pending" {
		t.Errorf("applied = %q, want pending", got)
	}
}

func TestApplyCache_ChecksCacheBeatsInline(t *testing.T) {
	inline := `[{"number":7,"title":"t","url":"u","state":"OPEN","statusCheckRollup":` + pendingRollup + `}]`
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", inline)
	writeCache(t, e.cacheDir, "k.checks.json", `[{"statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]}]`)
	if got := e.applyCache("T", p, "br"); got != "failure" {
		t.Errorf("applied = %q, want failure", got)
	}
}

func TestApplyCache_FalsyFlagsAndIntegerNumber(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", `[{"number":12345,"title":"t","url":"u","state":"OPEN","isDraft":false,"autoMergeRequest":null}]`)
	e.applyCache("T", p, "br")
	calls := f.snapshot()
	for opt, want := range map[string]string{"@pr_number": "12345", "@pr_draft": "", "@pr_auto_merge": "", "@pr_review": ""} {
		if got, ok := optionValue(calls, opt); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", opt, got, ok, want)
		}
	}
}

func TestApplyCache_MalformedIdentity(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons})
	p := writeCache(t, e.cacheDir, "k.json", `{not json`)
	if got := e.applyCache("T", p, "br"); got != "" {
		t.Errorf("applied = %q", got)
	}
	if n := len(setOpts(f.snapshot())); n != 0 {
		t.Errorf("set-option calls = %d, want 0", n)
	}
}

func values(state, check string) prValues {
	return prValues{number: "12", title: "T", state: state, check: check, url: "u", mergeable: "mergeable", branch: "br"}
}

func prevOf(v prValues, glyph string) string {
	return strings.Join([]string{v.number, v.state, v.check, v.mergeable, v.draft, v.review, v.autoMerge, v.progress, glyph}, "|")
}

func glyphOf(e *enricher, v prValues) string {
	return enrichstate.Glyph(e.o.icons, v.state, v.check, v.mergeable, v.draft, v.progress)
}

func TestWritePROptions_PrevFormat(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons, reflow: "/bin/reflow"})
	e.writePROptions("T", values("open", "pending"))
	calls := f.byName("tmux")
	if len(calls) == 0 || !reflect.DeepEqual(calls[0].args, []string{"display-message", "-t", "T", "-p", prevFormat}) {
		t.Fatalf("first tmux call = %v", calls)
	}
}

func TestWritePROptions_ReflowOnlyOnChange(t *testing.T) {
	v := values("open", "pending")

	same := newFakeRunner()
	e := testEnricher(t, same, opts{icons: testIcons, reflow: "/bin/reflow"})
	same.on("tmux display-message -t T -p "+prevFormat, prevOf(v, glyphOf(e, v)), nil)
	e.writePROptions("T", v)
	if d := same.kind("detach"); len(d) != 0 {
		t.Errorf("unchanged write detached: %v", d)
	}

	diff := newFakeRunner()
	e = testEnricher(t, diff, opts{icons: testIcons, reflow: "/bin/reflow"})
	diff.on("tmux display-message -t T -p "+prevFormat, prevOf(v, "stale glyph"), nil)
	diff.on("tmux display-message -t T -p #{session_name}", "sess", nil)
	e.writePROptions("T", v)
	d := diff.kind("detach")
	if len(d) != 1 || d[0].name != "/bin/reflow" || !reflect.DeepEqual(d[0].args, []string{"sess", "--force"}) {
		t.Fatalf("detach = %v", d)
	}
	var sawSession bool
	for _, c := range diff.byName("tmux") {
		if reflect.DeepEqual(c.args, []string{"display-message", "-t", "T", "-p", "#{session_name}"}) {
			sawSession = true
		}
	}
	if !sawSession {
		t.Error("no session_name lookup")
	}
}

func notifyArgs(level, title string) []string {
	return []string{"emit", "--source", "pr", "--level", level, "--window", "T", "--title", title, "--body", "#12 T"}
}

func TestNotify(t *testing.T) {
	cases := []struct {
		name        string
		notifyBin   string
		prevState   string
		prevCheck   string
		state       string
		check       string
		level, head string // empty level = silent
	}{
		{"merged", "/bin/notify", "open", "success", "merged", "success", "info", "PR merged"},
		{"merged beats check flip", "/bin/notify", "open", "success", "merged", "failure", "info", "PR merged"},
		{"checks failed", "/bin/notify", "open", "success", "open", "failure", "error", "checks failed"},
		{"checks passed", "/bin/notify", "open", "pending", "open", "success", "info", "checks passed"},
		{"empty prior state and check", "/bin/notify", "", "", "merged", "failure", "", ""},
		{"to pending", "/bin/notify", "open", "success", "open", "pending", "", ""},
		{"closed", "/bin/notify", "open", "success", "closed", "success", "", ""},
		{"unsubstituted placeholder", "@notify@", "open", "success", "merged", "success", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRunner()
			e := testEnricher(t, f, opts{icons: testIcons, reflow: "/bin/reflow", notify: c.notifyBin})
			f.on("tmux display-message -t T -p "+prevFormat, "12|"+c.prevState+"|"+c.prevCheck+"|mergeable|||||", nil)
			e.writePROptions("T", values(c.state, c.check))
			var nc []call
			for _, d := range f.kind("detach") {
				if strings.HasSuffix(d.name, "notify@") || d.name == c.notifyBin {
					nc = append(nc, d)
				}
			}
			if c.level == "" {
				if len(nc) != 0 {
					t.Fatalf("notified: %v", nc)
				}
				return
			}
			if len(nc) != 1 || nc[0].name != c.notifyBin || !reflect.DeepEqual(nc[0].args, notifyArgs(c.level, c.head)) {
				t.Fatalf("notify = %v, want args %v", nc, notifyArgs(c.level, c.head))
			}
		})
	}
}

func armSentinel(t *testing.T, e *enricher) {
	t.Helper()
	if err := os.WriteFile(e.sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLogEvent_SentinelAbsent(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{libLog: "/x/lib-log"})
	e.logEvent("event", "pr")
	if n := len(f.snapshot()); n != 0 {
		t.Errorf("calls = %d", n)
	}
}

func TestLogEvent_Sourced(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{libLog: "/x/lib-log"})
	armSentinel(t, e)
	e.logEvent("event", "pr", "target", "T")
	runs := f.kind("run")
	if len(runs) != 1 {
		t.Fatalf("runs = %v", f.snapshot())
	}
	want := []string{"-c", `source "$1"; shift; log_event "$@"`, "_", "/x/lib-log", "enrich", "event", "pr", "target", "T"}
	if runs[0].name != "bash" || !reflect.DeepEqual(runs[0].args, want) {
		t.Errorf("run = %s %v", runs[0].name, runs[0].args)
	}
}

func TestLogEvent_UnsubstitutedLibLog(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{libLog: "@lib_log@"})
	armSentinel(t, e)
	e.logEvent("event", "pr")
	if n := len(f.snapshot()); n != 0 {
		t.Errorf("calls = %d", n)
	}
}

func TestWritePROptions_LogsEvent(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{icons: testIcons, libLog: "/x/lib-log", reflow: "/bin/reflow"})
	armSentinel(t, e)
	v := values("open", "pending")
	v.draft, v.review, v.autoMerge, v.progress = "1", "approved", "1", "1/2"
	e.writePROptions("T", v)
	runs := f.kind("run")
	var logged []string
	for _, c := range runs {
		if c.name == "bash" {
			logged = c.args
		}
	}
	want := []string{"-c", `source "$1"; shift; log_event "$@"`, "_", "/x/lib-log", "enrich", "event", "pr",
		"target", "T", "number", "12", "state", "open", "check", "pending", "mergeable", "mergeable",
		"draft", "1", "review", "approved", "auto_merge", "1", "progress", "1/2"}
	if !reflect.DeepEqual(logged, want) {
		t.Errorf("log argv =\n%v\nwant\n%v", logged, want)
	}
}
