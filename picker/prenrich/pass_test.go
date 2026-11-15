package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

const (
	identBatchArgs  = "pr list --state open --limit 100 --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName,isCrossRepository"
	checksBatchArgs = "pr list --state open --limit 100 --json headRefName,statusCheckRollup,headRefOid,isCrossRepository"
	headFields      = "number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,isCrossRepository"
	urlViewArgs     = "pr view https://github.com/o/r/pull/5 --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,statusCheckRollup"
	rereadFormat    = "#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_check_progress}|#{@pr_glyph}"
	prURL5          = "https://github.com/o/r/pull/5"
	successRollup   = `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]`
)

// passEnv is a fake runner plus an enricher over a temp cache, with the
// tmux/git plumbing every pass test needs.
type passEnv struct {
	t *testing.T
	f *fakeRunner
	e *enricher
}

func newPassEnv(t *testing.T, o opts) *passEnv {
	t.Helper()
	if o.icons == (enrichstate.Icons{}) {
		o.icons = testIcons
	}
	if o.checkRefreshSeconds == 0 {
		o.checkRefreshSeconds = 300
	}
	f := newFakeRunner("gh")
	return &passEnv{t: t, f: f, e: testEnricher(t, f, o)}
}

func (p *passEnv) windows(rows ...string) {
	p.f.on("tmux list-windows -a -F "+listWindowsFormat, strings.Join(rows, "\n"), nil)
}

func (p *passEnv) repo(dir, common string) {
	p.f.on("git -C "+dir+" rev-parse --path-format=absolute --git-common-dir", common, nil)
}

func (p *passEnv) gone(dir string) {
	p.f.on("git -C "+dir+" rev-parse --path-format=absolute --git-common-dir", "", fmt.Errorf("not a git repository"))
}

func (p *passEnv) gh(args, out string, err error) { p.f.on("gh "+args, out, err) }

func (p *passEnv) cache(name string) string { return filepath.Join(p.e.cacheDir, name) }

func (p *passEnv) key(repo, br string) string { return sha1Key(repo + "|" + br) }

func (p *passEnv) marker(repo string) string {
	return p.cache(sha1Key(repo) + ".checks-pending" + p.e.srv)
}

func (p *passEnv) tick(name string) string { return p.cache(name + p.e.srv) }

func (p *passEnv) writeMarker(repo string, first int64, fp string, mtime int64) {
	p.t.Helper()
	path := p.marker(repo)
	if err := os.WriteFile(path, []byte(strconv.FormatInt(first, 10)+"\n"+fp+"\n"), 0o644); err != nil {
		p.t.Fatal(err)
	}
	touchAt(path, time.Unix(mtime, 0))
}

func (p *passEnv) setTick(name string, mtime int64) {
	p.t.Helper()
	touchAt(p.tick(name), time.Unix(mtime, 0))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func row(fields ...string) string { return strings.Join(fields, "|") }

// ghCalls returns the gh calls whose "args..." start with prefix.
func ghCalls(f *fakeRunner, prefix string) []call {
	var out []call
	for _, c := range f.byName("gh") {
		if strings.HasPrefix(strings.Join(c.args, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func prObj(head string) string {
	return `{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"headRefName":"` + head + `","isCrossRepository":false}`
}

func checksObj(head, oid, rollup string) string {
	return `{"headRefName":"` + head + `","statusCheckRollup":` + rollup + `,"headRefOid":"` + oid + `","isCrossRepository":false}`
}

func glyphWrites(f *fakeRunner, target string) []string {
	var out []string
	for _, c := range f.byName("tmux") {
		if len(c.args) == 6 && reflect.DeepEqual(c.args[:5], []string{"set-option", "-t", target, "-w", "@pr_glyph"}) {
			out = append(out, c.args[5])
		}
	}
	return out
}

func (p *passEnv) reread(target, out string) {
	p.f.on("tmux display-message -t "+target+" -p "+rereadFormat, out, nil)
}

// --- parsing ---

func TestParseWindowRows(t *testing.T) {
	if got := parseWindowRows(""); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}

	short := parseWindowRows("$1:@1|/repo||feat/x|\n$1:@2|/repo||feat/y||https://u")
	if len(short) != 2 {
		t.Fatalf("rows = %v", short)
	}
	want := windowRow{target: "$1:@1", worktree: "/repo", branch: "feat/x"}
	if short[0] != want {
		t.Errorf("5-field row = %+v, want %+v", short[0], want)
	}
	if short[1].prURL != "https://u" || short[1].bridgeWin != "" || short[1].prGlyph != "" {
		t.Errorf("6-field row = %+v", short[1])
	}

	if got := parseWindowRows("$1:@1|a|b|c\n\nx|y"); len(got) != 0 {
		t.Errorf("short rows kept: %v", got)
	}

	full := parseWindowRows("$1:@1|/w|/g|br|1|url|7|open|success|mergeable|1|2/3|glyph")
	wantFull := windowRow{"$1:@1", "/w", "/g", "br", "1", "url", "7", "open", "success", "mergeable", "1", "2/3", "glyph"}
	if len(full) != 1 || full[0] != wantFull {
		t.Errorf("13-field row = %+v", full)
	}
}

func TestListWindowsFormat(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows()
	p.e.runFullPass(false)
	calls := p.f.byName("tmux")
	if len(calls) == 0 || !reflect.DeepEqual(calls[0].args, []string{"list-windows", "-a", "-F", listWindowsFormat}) {
		t.Fatalf("first tmux call = %v", calls)
	}
	n := 0
	for _, c := range calls {
		if len(c.args) > 0 && c.args[0] == "list-windows" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("list-windows calls = %d, want 1", n)
	}
}

// --- grouping and skipping ---

func TestPass_GroupsByCommonDirAndRetriesGitRoot(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(
		row("$1:@1", "/a/wt1", "/a", "feat/x", ""),
		row("$1:@2", "/a/wt2", "/a", "feat/y", ""),
		row("$1:@3", "/stale", "/b", "feat/z", ""),
	)
	p.repo("/a/wt1", "/a/.git")
	p.repo("/a/wt2", "/a/.git")
	p.gone("/stale")
	p.repo("/b", "/b/.git")
	p.gh(identBatchArgs, "["+prObj("feat/x")+","+prObj("feat/y")+"]", nil)
	p.e.runFullPass(false)

	batches := ghCalls(p.f, identBatchArgs)
	if len(batches) != 2 {
		t.Fatalf("identity batches = %v", batches)
	}
	dirs := []string{batches[0].dir, batches[1].dir}
	slices.Sort(dirs)
	if dirs[0] != "/a/wt2" || dirs[1] != "/b" {
		t.Errorf("batch dirs = %v, want [/a/wt2 /b]", dirs)
	}
	for _, c := range batches {
		if !reflect.DeepEqual(c.args, strings.Fields(identBatchArgs)) {
			t.Errorf("argv = %v", c.args)
		}
	}
	if !exists(p.cache(p.key("/a/.git", "feat/x")+".json")) || !exists(p.cache(p.key("/a/.git", "feat/y")+".json")) {
		t.Error("batch hits not cached under the common-dir key")
	}
}

func TestPass_SkipsEmptyBranchAndMirror(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(
		row("$1:@1", "/skip1", "", "", ""),
		row("$1:@2", "/skip2", "", "feat/m", "1"),
	)
	p.e.runFullPass(false)
	if n := len(p.f.byName("gh")) + len(p.f.byName("git")); n != 0 {
		t.Errorf("gh/git calls = %v", p.f.snapshot())
	}
}

// --- identity batch outcomes ---

func TestPass_BatchHitWritesOneElementCache(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", "feat/x", ""))
	p.repo("/a", "/a/.git")
	p.gh(identBatchArgs, "["+prObj("feat/x")+"]", nil)
	p.e.runFullPass(false)

	got := readJSON(t, p.cache(p.key("/a/.git", "feat/x")+".json"))
	want := mustJSON(t, `[{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"headRefName":"feat/x"}]`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cache = %v", got)
	}
	if n := len(ghCalls(p.f, "pr list --head")); n != 0 {
		t.Errorf("per-head lookups = %d, want 0", n)
	}
	if v, ok := optionValue(p.f.snapshot(), "@pr_number"); !ok || v != "7" {
		t.Errorf("@pr_number = %q (%v)", v, ok)
	}
}

func TestPass_BatchMissLooksUpStateAll(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", "feat/x", ""), row("$1:@2", "/a", "", "feat/y", ""))
	p.repo("/a", "/a/.git")
	p.gh(identBatchArgs, "["+prObj("feat/x")+"]", nil)
	p.gh("pr list --head feat/y --state all --limit 100 --json "+headFields, `[{"number":9,"state":"MERGED","isCrossRepository":false}]`, nil)
	p.e.runFullPass(false)

	if n := len(ghCalls(p.f, "pr list --head feat/y --state all")); n != 1 {
		t.Errorf("state-all lookups = %d, want 1", n)
	}
	if n := len(ghCalls(p.f, "pr list --head feat/y --state open")); n != 0 {
		t.Errorf("batch-ok miss ran an open lookup (%d)", n)
	}
	if n := len(ghCalls(p.f, "pr list --head feat/x")); n != 0 {
		t.Errorf("batch hit ran a per-head lookup (%d)", n)
	}
	arr, _ := readJSON(t, p.cache(p.key("/a/.git", "feat/y")+".json")).([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["state"] != "MERGED" {
		t.Errorf("cache = %v", arr)
	}
}

func TestPass_BatchErrorFallsBackToOpenThenAll(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", "feat/x", ""))
	p.repo("/a", "/a/.git")
	p.gh(identBatchArgs, "", fmt.Errorf("offline"))
	p.gh("pr list --head feat/x --state open", `[]`, nil)
	p.gh("pr list --head feat/x --state all", `[{"number":9,"state":"CLOSED","isCrossRepository":false}]`, nil)
	p.e.runFullPass(false)

	if len(ghCalls(p.f, "pr list --head feat/x --state open")) != 1 || len(ghCalls(p.f, "pr list --head feat/x --state all")) != 1 {
		t.Errorf("gh calls = %v", p.f.byName("gh"))
	}
	arr, _ := readJSON(t, p.cache(p.key("/a/.git", "feat/x")+".json")).([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["state"] != "CLOSED" {
		t.Errorf("cache = %v", arr)
	}
}

// --- gone checkout ---

func TestPass_GoneCheckoutRefreshesByURL(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/gone", "", "feat/z", "", prURL5))
	p.gone("/gone")
	p.gh(urlViewArgs, `{"number":5,"title":"t","url":"`+prURL5+`","state":"MERGED","statusCheckRollup":`+successRollup+`}`, nil)
	p.e.runFullPass(false)

	calls := ghCalls(p.f, urlViewArgs)
	if len(calls) != 1 {
		t.Fatalf("gh = %v", p.f.byName("gh"))
	}
	k := sha1Key("url|" + prURL5)
	arr, _ := readJSON(t, p.cache(k+".json")).([]any)
	if len(arr) != 1 {
		t.Fatalf("cache = %v", arr)
	}
	if _, has := arr[0].(map[string]any)["statusCheckRollup"]; has {
		t.Errorf("identity cache kept the rollup: %v", arr)
	}
	if !exists(p.cache(k + ".checks.json")) {
		t.Error("no checks cache")
	}
	if v, ok := optionValue(p.f.snapshot(), "@pr_number"); !ok || v != "5" {
		t.Errorf("@pr_number = %q (%v)", v, ok)
	}
}

func TestPass_GoneCheckoutURLFailureLogsAndKeepsCache(t *testing.T) {
	p := newPassEnv(t, opts{libLog: "/x/lib-log"})
	if err := os.WriteFile(p.e.sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p.windows(row("$1:@1", "/gone", "", "feat/z", "", prURL5))
	p.gone("/gone")
	p.gh(urlViewArgs, "", fmt.Errorf("boom"))
	p.e.runFullPass(false)

	k := sha1Key("url|" + prURL5)
	if exists(p.cache(k+".json")) || exists(p.cache(k+".checks.json")) {
		t.Error("failed url refresh touched the cache")
	}
	want := []string{"-c", `source "$1"; shift; log_event "$@"`, "_", "/x/lib-log", "enrich", "event", "pr_url_fail", "url", prURL5}
	found := false
	for _, c := range p.f.kind("run") {
		if c.name == "bash" && reflect.DeepEqual(c.args, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("no pr_url_fail log run in %v", p.f.kind("run"))
	}
	if n := len(setOpts(p.f.snapshot())); n != 0 {
		t.Errorf("set-option calls = %d, want 0", n)
	}
}

func TestPass_MalformedURLGoneCheckoutSkipped(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/gone", "", "feat/z", "", "https://example.com/x"))
	p.gone("/gone")
	p.e.runFullPass(false)
	if n := len(p.f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %v", p.f.byName("gh"))
	}
}

// --- 30-entry cap and stale-marker sweep ---

func TestPass_CapAt30UniqueBranches(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.e.srv = ".srv_a"
	var rows []string
	var objs []string
	for i := range 31 {
		br := fmt.Sprintf("br%02d", i)
		rows = append(rows, row(fmt.Sprintf("$1:@%d", i+1), "/a", "", br, ""))
		objs = append(objs, prObj(br))
	}
	p.windows(rows...)
	p.repo("/a", "/a/.git")
	p.gh(identBatchArgs, "["+strings.Join(objs, ",")+"]", nil)
	p.writeMarker("/gone-repo", testNowUnix, "fp", testNowUnix)
	p.e.runFullPass(false)

	written := 0
	for i := range 31 {
		if exists(p.cache(p.key("/a/.git", fmt.Sprintf("br%02d", i)) + ".json")) {
			written++
			if i == 30 {
				t.Error("31st branch was processed")
			}
		}
	}
	if written != 30 {
		t.Errorf("branch caches = %d, want 30", written)
	}
	applied := 0
	for _, c := range setOpts(p.f.snapshot()) {
		if c[4] == "@pr_number" {
			applied++
		}
	}
	if applied != 30 {
		t.Errorf("applied windows = %d, want 30", applied)
	}
	if !exists(p.marker("/gone-repo")) {
		t.Error("truncated pass swept a marker")
	}
}

func TestPass_StaleMarkerSweep(t *testing.T) {
	for name, rows := range map[string][]string{
		"other repo live": {row("$1:@1", "/a", "", "feat/x", "")},
		"no windows":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPassEnv(t, opts{})
			p.e.srv = ".srv_a"
			p.windows(rows...)
			p.repo("/a", "/a/.git")
			p.gh(identBatchArgs, "["+prObj("feat/x")+"]", nil)
			p.writeMarker("/gone-repo", testNowUnix, "fp", testNowUnix)
			otherSrv := p.cache(sha1Key("/gone-repo") + ".checks-pending.srv_b")
			if err := os.WriteFile(otherSrv, []byte("1\nfp\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p.e.runFullPass(false)
			if exists(p.marker("/gone-repo")) {
				t.Error("stale marker of this srv survived")
			}
			if !exists(otherSrv) {
				t.Error("another srv's marker was removed")
			}
		})
	}
}

// --- checks batch ---

func TestPass_ChecksBatchGate(t *testing.T) {
	setup := func(t *testing.T) *passEnv {
		p := newPassEnv(t, opts{})
		p.windows(row("$1:@1", "/a", "", "feat/x", ""))
		p.repo("/a", "/a/.git")
		p.gh(identBatchArgs, "["+prObj("feat/x")+"]", nil)
		p.gh(checksBatchArgs, "["+checksObj("feat/x", "aaaa", successRollup)+"]", nil)
		return p
	}
	checksCalls := func(p *passEnv) []call { return ghCalls(p.f, checksBatchArgs) }

	t.Run("absent tick runs and stamps", func(t *testing.T) {
		p := setup(t)
		p.e.runFullPass(false)
		calls := checksCalls(p)
		if len(calls) != 1 || calls[0].dir != "/a" {
			t.Fatalf("checks calls = %v", calls)
		}
		if !exists(p.tick(".last-check-tick")) {
			t.Error("tick not stamped")
		}
		if !exists(p.cache(p.key("/a/.git", "feat/x") + ".checks.json")) {
			t.Error("no checks cache")
		}
	})
	t.Run("stale tick runs", func(t *testing.T) {
		p := setup(t)
		p.setTick(".last-check-tick", testNowUnix-1000)
		p.e.runFullPass(false)
		if len(checksCalls(p)) != 1 {
			t.Errorf("checks calls = %v", checksCalls(p))
		}
	})
	t.Run("fresh tick without marker skips", func(t *testing.T) {
		p := setup(t)
		p.setTick(".last-check-tick", testNowUnix)
		p.e.runFullPass(false)
		if n := len(checksCalls(p)); n != 0 {
			t.Errorf("checks calls = %d, want 0", n)
		}
	})
	t.Run("fresh tick with due marker runs", func(t *testing.T) {
		p := setup(t)
		p.setTick(".last-check-tick", testNowUnix)
		p.writeMarker("/a/.git", testNowUnix-100, "fp", testNowUnix-100)
		p.e.runFullPass(false)
		if n := len(checksCalls(p)); n != 1 {
			t.Errorf("checks calls = %d, want 1", n)
		}
	})
}

// --- pending marker ---

func TestPendingFingerprint(t *testing.T) {
	got := pendingFingerprint([]string{"b|2", "a|1", "b|2"})
	if want := sha1Key("a|1\nb|2"); got != want {
		t.Errorf("fingerprint = %q, want %q", got, want)
	}
}

func TestArmPendingMarker(t *testing.T) {
	p := newPassEnv(t, opts{})
	repo := "/a/.git"
	p.setTick(".last-pending-tick", testNowUnix)

	p.e.armPendingMarker(repo, "fp1")
	want := strconv.Itoa(testNowUnix) + "\nfp1\n"
	if b, _ := os.ReadFile(p.marker(repo)); string(b) != want {
		t.Errorf("new marker = %q, want %q", b, want)
	}
	if exists(p.tick(".last-pending-tick")) {
		t.Error("new marker kept .last-pending-tick")
	}

	p.writeMarker(repo, 100, "fp1", testNowUnix)
	p.setTick(".last-pending-tick", testNowUnix)
	p.e.armPendingMarker(repo, "fp1")
	if b, _ := os.ReadFile(p.marker(repo)); string(b) != "100\nfp1\n" {
		t.Errorf("same fingerprint rewrote the marker: %q", b)
	}
	if !exists(p.tick(".last-pending-tick")) {
		t.Error("same fingerprint dropped .last-pending-tick")
	}

	p.e.armPendingMarker(repo, "fp2")
	if b, _ := os.ReadFile(p.marker(repo)); string(b) != strconv.Itoa(testNowUnix)+"\nfp2\n" {
		t.Errorf("changed fingerprint = %q", b)
	}
	if exists(p.tick(".last-pending-tick")) {
		t.Error("changed fingerprint kept .last-pending-tick")
	}
}

func pendingPassEnv(t *testing.T, rollup string) *passEnv {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", "feat/x", ""))
	p.repo("/a", "/a/.git")
	p.gh(identBatchArgs, "["+prObj("feat/x")+"]", nil)
	p.gh(checksBatchArgs, "["+checksObj("feat/x", "aaaa", rollup)+"]", nil)
	return p
}

func TestPass_PendingArmsMarkerThenSettles(t *testing.T) {
	p := pendingPassEnv(t, pendingRollup)
	p.setTick(".last-pending-tick", testNowUnix)
	p.e.runFullPass(false)

	want := strconv.Itoa(testNowUnix) + "\n" + pendingFingerprint([]string{"feat/x|aaaa"}) + "\n"
	if b, _ := os.ReadFile(p.marker("/a/.git")); string(b) != want {
		t.Fatalf("marker = %q, want %q", b, want)
	}
	if exists(p.tick(".last-pending-tick")) {
		t.Error(".last-pending-tick survived a newly armed marker")
	}

	// Same pending set later: the episode's first-seen epoch is kept.
	p.e.now = func() time.Time { return time.Unix(testNowUnix+100, 0) }
	_ = os.Remove(p.tick(".last-check-tick"))
	p.e.runFullPass(false)
	b, _ := os.ReadFile(p.marker("/a/.git"))
	if first, _, _ := strings.Cut(string(b), "\n"); first != strconv.Itoa(testNowUnix) {
		t.Errorf("episode restarted: %q", b)
	}

	// Settled: the marker goes.
	p.f.on("gh "+checksBatchArgs, "["+checksObj("feat/x", "aaaa", successRollup)+"]", nil)
	_ = os.Remove(p.tick(".last-check-tick"))
	p.e.runFullPass(false)
	if exists(p.marker("/a/.git")) {
		t.Error("marker survived a settled rollup")
	}
}

// --- pending-only pass ---

func TestPass_PendingOnlyRunsOnlyDueRepos(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(
		row("$1:@1", "/a", "", "fa", ""),
		row("$1:@2", "/b", "", "fb", ""),
		row("$1:@3", "/gone", "", "fc", "", prURL5),
	)
	p.repo("/a", "/a/.git")
	p.repo("/b", "/b/.git")
	p.gone("/gone")
	p.gh(checksBatchArgs, "["+checksObj("fa", "aaaa", pendingRollup)+"]", nil)
	p.writeMarker("/a/.git", testNowUnix-100, "fp", testNowUnix-100)
	p.e.runFullPass(true)

	calls := ghCalls(p.f, "pr ")
	if len(calls) != 1 || calls[0].dir != "/a" || strings.Join(calls[0].args, " ") != checksBatchArgs {
		t.Fatalf("gh calls = %v", calls)
	}
	if n := len(ghCalls(p.f, identBatchArgs)) + len(ghCalls(p.f, "pr view")); n != 0 {
		t.Errorf("pending-only ran identity work (%d)", n)
	}
}

func TestPass_PendingOnlyDoesNotRunUndueRepo(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", "fa", ""))
	p.repo("/a", "/a/.git")
	p.writeMarker("/a/.git", testNowUnix-100, "fp", testNowUnix)
	p.e.runFullPass(true)
	if n := len(p.f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %v", p.f.byName("gh"))
	}
	if exists(p.tick(".last-pending-tick")) {
		t.Error("a marker inside the window still got a push-ahead")
	}
}

func TestPass_PendingOnlyPushesTickAheadWhenAllMarkersExpired(t *testing.T) {
	p := newPassEnv(t, opts{checkRefreshSeconds: 300})
	p.windows(row("$1:@1", "/a", "", "fa", ""))
	p.repo("/a", "/a/.git")
	p.writeMarker("/a/.git", testNowUnix-2000, "fp", testNowUnix-2000)
	p.e.runFullPass(true)
	if n := len(p.f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %v", p.f.byName("gh"))
	}
	if got := fileMtime(p.tick(".last-pending-tick")); got != testNowUnix+300 {
		t.Errorf(".last-pending-tick mtime = %d, want %d", got, testNowUnix+300)
	}
}

// --- glyph sync ---

func TestGlyphSync_WritesStaleGlyphAfterReread(t *testing.T) {
	p := newPassEnv(t, opts{})
	want := enrichstate.Glyph(testIcons, "open", "success", "", "", "")
	if want == "" {
		t.Fatal("test glyph is empty")
	}
	p.reread("T1", "7|open|success||||")
	p.e.glyphSync([]windowRow{{target: "T1", prNumber: "7", prState: "open", prCheck: "success"}})

	if got := glyphWrites(p.f, "T1"); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("glyph writes = %q, want [%q]", got, want)
	}
	var order []string
	for _, c := range p.f.byName("tmux") {
		switch c.args[0] {
		case "display-message":
			order = append(order, "read")
		case "set-option":
			order = append(order, "write")
		}
	}
	if !reflect.DeepEqual(order, []string{"read", "write"}) {
		t.Errorf("order = %v", order)
	}
	for _, c := range p.f.byName("tmux") {
		if c.args[0] == "display-message" && !reflect.DeepEqual(c.args, []string{"display-message", "-t", "T1", "-p", rereadFormat}) {
			t.Errorf("re-read argv = %v", c.args)
		}
	}
}

func TestGlyphSync_RereadWins(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.reread("T1", "7|merged|success||||old")
	p.e.glyphSync([]windowRow{{target: "T1", prNumber: "7", prState: "open", prCheck: "success", prGlyph: "old"}})
	want := enrichstate.Glyph(testIcons, "merged", "success", "", "", "")
	if want == enrichstate.Glyph(testIcons, "open", "success", "", "", "") {
		t.Fatal("test glyphs do not differ")
	}
	if got := glyphWrites(p.f, "T1"); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("glyph writes = %q, want [%q]", got, want)
	}
}

func TestGlyphSync_NoWrites(t *testing.T) {
	p := newPassEnv(t, opts{})
	cur := enrichstate.Glyph(testIcons, "open", "success", "", "", "")
	p.e.glyphSync([]windowRow{
		{target: "T1", prNumber: "7", prState: "open", prCheck: "success", prGlyph: cur},
		{target: "T2", bridgeWin: "1", prNumber: "7", prState: "open", prCheck: "success"},
		{target: "T3"},
		{target: "T4", prNumber: "none"},
	})
	if n := len(setOpts(p.f.snapshot())); n != 0 {
		t.Errorf("set-option calls = %d, want 0", n)
	}
}

func TestGlyphSync_NoPRWantsEmptyGlyph(t *testing.T) {
	p := newPassEnv(t, opts{})
	p.reread("T1", "none||||||X")
	p.e.glyphSync([]windowRow{{target: "T1", prNumber: "none", prGlyph: "X"}})
	if got := glyphWrites(p.f, "T1"); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("glyph writes = %q, want [\"\"]", got)
	}
}

func TestGlyphSync_OneReflowPerSession(t *testing.T) {
	p := newPassEnv(t, opts{reflow: "/bin/reflow"})
	for _, tgt := range []string{"$1:@1", "$1:@2", "$2:@3"} {
		p.reread(tgt, "7|open|success||||")
	}
	p.f.on("tmux display-message -t $1:@1 -p #{session_name}", "s1", nil)
	p.f.on("tmux display-message -t $1:@2 -p #{session_name}", "s1", nil)
	p.f.on("tmux display-message -t $2:@3 -p #{session_name}", "s2", nil)
	stale := func(tgt string) windowRow {
		return windowRow{target: tgt, prNumber: "7", prState: "open", prCheck: "success"}
	}
	p.e.glyphSync([]windowRow{stale("$1:@1"), stale("$1:@2"), stale("$2:@3")})

	var got []string
	for _, c := range p.f.kind("detach") {
		if c.name != "/bin/reflow" || len(c.args) != 2 || c.args[1] != "--force" {
			t.Errorf("detach = %v", c)
		}
		got = append(got, c.args[0])
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Errorf("reflowed sessions = %v, want [s1 s2]", got)
	}
}

func TestPass_GlyphSyncCoversSkippedWindows(t *testing.T) {
	for _, pendingOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("pendingOnly=%v", pendingOnly), func(t *testing.T) {
			p := newPassEnv(t, opts{})
			stalePR := []string{"7", "open", "success", "", "", "", ""}
			mk := func(fields ...string) string { return row(append(fields, stalePR...)...) }
			p.windows(
				mk("$1:@1", "/x", "", "", "", ""),
				mk("$1:@2", "/gone", "", "feat", "", ""),
				mk("$1:@3", "/m", "", "feat", "1", ""),
			)
			p.gone("/gone")
			want := enrichstate.Glyph(testIcons, "open", "success", "", "", "")
			p.reread("$1:@1", "7|open|success||||")
			p.reread("$1:@2", "7|open|success||||")
			p.e.runFullPass(pendingOnly)

			for _, tgt := range []string{"$1:@1", "$1:@2"} {
				if got := glyphWrites(p.f, tgt); !reflect.DeepEqual(got, []string{want}) {
					t.Errorf("%s glyph writes = %q, want [%q]", tgt, got, want)
				}
			}
			if got := glyphWrites(p.f, "$1:@3"); len(got) != 0 {
				t.Errorf("mirror glyph writes = %q", got)
			}
		})
	}
}

func TestPass_GlyphSyncCoversWindowsPastCap(t *testing.T) {
	p := newPassEnv(t, opts{})
	var rows []string
	for i := range 31 {
		fields := []string{fmt.Sprintf("$1:@%d", i+1), "/a", "", fmt.Sprintf("br%02d", i), ""}
		if i == 30 {
			fields = append(fields, "", "7", "open", "success", "", "", "", "")
		}
		rows = append(rows, row(fields...))
	}
	p.windows(rows...)
	p.repo("/a", "/a/.git")
	p.reread("$1:@31", "7|open|success||||")
	p.e.runFullPass(false)

	want := enrichstate.Glyph(testIcons, "open", "success", "", "", "")
	if got := glyphWrites(p.f, "$1:@31"); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("capped window glyph writes = %q, want [%q]", got, want)
	}
}
