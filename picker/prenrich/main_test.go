package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testSelf   = "/store/bin/tmux-pr-enrich"
	testURL    = "https://github.com/o/r/pull/5"
	testTmuxEv = "/tmp/tmux-1000/default,123,0"
)

var baseArgv = []string{
	"--refresh-seconds", "30", "--check-refresh-seconds", "300",
	"--reflow", "/bin/reflow", "--icon-success", "S", "--icon-pending", "P",
}

func argv(extra ...string) opts {
	return parseArgs(append(append([]string(nil), baseArgv...), extra...))
}

type mainEnv struct {
	f   *fakeRunner
	e   *enricher
	dir string // cache dir, deliberately not created
}

func newMainEnv(t *testing.T, o opts, tmuxEnv string, bins ...string) *mainEnv {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	env := map[string]string{
		"OG_ENRICH_CACHE_DIR": dir,
		"OG_DEBUG_SENTINEL":   filepath.Join(root, "debug.on"),
		"TMUX":                tmuxEnv,
	}
	f := newFakeRunner(bins...)
	now := func() time.Time { return time.Unix(testNowUnix, 0) }
	e := newEnricher(o, f, func(k string) string { return env[k] }, now)
	return &mainEnv{f: f, e: e, dir: dir}
}

func (m *mainEnv) dirExists() bool {
	fi, err := os.Stat(m.dir)
	if err != nil {
		return false
	}
	return fi.IsDir()
}

func (m *mainEnv) mkdir(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// stamp creates a cache-dir file whose mtime is ageSec seconds before the enricher's now.
func (m *mainEnv) stamp(t *testing.T, name string, ageSec int64) string {
	t.Helper()
	m.mkdir(t)
	p := filepath.Join(m.dir, name)
	touchAt(p, time.Unix(testNowUnix-ageSec, 0))
	return p
}

func setOptCalls(f *fakeRunner) []call {
	var out []call
	for _, c := range f.byName("tmux") {
		if len(c.args) > 0 && c.args[0] == "set-option" {
			out = append(out, c)
		}
	}
	return out
}

func setOptValue(t *testing.T, f *fakeRunner, opt string) string {
	t.Helper()
	for _, c := range setOptCalls(f) {
		if c.args[len(c.args)-2] == opt {
			return c.args[len(c.args)-1]
		}
	}
	t.Fatalf("no set-option for %s in %v", opt, f.snapshot())
	return ""
}

func ghCallsMatching(f *fakeRunner, prefix string) []call {
	var out []call
	for _, c := range f.byName("gh") {
		if strings.HasPrefix(c.joined(), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestNewEnricher_Defaults(t *testing.T) {
	e := newEnricher(opts{}, newFakeRunner(), func(string) string { return "" }, time.Now)
	if e.cacheDir != "/tmp/og-pr" || e.srv != "" || e.sentinel != "/tmp/og-debug.on" {
		t.Fatalf("got cacheDir=%q srv=%q sentinel=%q", e.cacheDir, e.srv, e.sentinel)
	}
}

func TestNewEnricher_Overrides(t *testing.T) {
	env := map[string]string{
		"OG_ENRICH_CACHE_DIR": "/x/cache",
		"OG_DEBUG_SENTINEL":   "/x/sentinel",
		"TMUX":                testTmuxEv,
	}
	o := opts{mode: "mock"}
	e := newEnricher(o, newFakeRunner(), func(k string) string { return env[k] }, time.Now)
	if e.cacheDir != "/x/cache" || e.sentinel != "/x/sentinel" {
		t.Fatalf("got cacheDir=%q sentinel=%q", e.cacheDir, e.sentinel)
	}
	if want := srvSuffix(testTmuxEv); e.srv != want || want == "" {
		t.Fatalf("srv = %q, want %q", e.srv, want)
	}
	if e.o.mode != "mock" {
		t.Fatalf("opts not stored: %+v", e.o)
	}
}

func TestRun_MockWithoutTargetDoesNothing(t *testing.T) {
	m := newMainEnv(t, argv("--mock-pr-number", "7"), "", "gh")
	m.e.run(testSelf)
	if n := len(m.f.snapshot()); n != 0 {
		t.Fatalf("calls = %v, want none", m.f.snapshot())
	}
}

func TestRun_MockWritesOptionsWithoutGh(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat",
		"--mock-pr-number", "7", "--mock-pr-state", "open", "--mock-check-state", "success",
		"--mock-pr-title", "hello\tworld", "--mock-pr-url", testURL), "", "gh")
	m.e.run(testSelf)

	calls := m.f.snapshot()
	if len(calls) == 0 || calls[0].name != "tmux" || calls[0].args[0] != "display-message" {
		t.Fatalf("first call = %+v, want prev display-message", calls)
	}
	if got := setOptValue(t, m.f, "@pr_number"); got != "7" {
		t.Errorf("@pr_number = %q", got)
	}
	if got := setOptValue(t, m.f, "@pr_title"); got != sanitizeTitle("hello\tworld") {
		t.Errorf("@pr_title = %q, want sanitized", got)
	}
	if got := setOptValue(t, m.f, "@pr_glyph"); got != "S" {
		t.Errorf("@pr_glyph = %q, want S", got)
	}
	if got := setOptValue(t, m.f, "@pr_branch"); got != "feat" {
		t.Errorf("@pr_branch = %q", got)
	}
	if n := len(m.f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %d, want 0", n)
	}
	if m.dirExists() {
		t.Error("mock must not create the cache dir")
	}
}

func TestRun_URLModeMalformedURLNoCalls(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--url", "https://example.com/x"), "", "gh")
	m.e.run(testSelf)
	if n := len(m.f.snapshot()); n != 0 {
		t.Fatalf("calls = %v, want none", m.f.snapshot())
	}
}

func TestRun_URLModeMirrorRefused(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--url", testURL), "", "gh")
	m.f.on("tmux show-options -t @3 -wqv @bridge_win", "1", nil)
	m.e.run(testSelf)
	if n := len(m.f.byName("gh")); n != 0 {
		t.Fatalf("gh calls = %d, want 0", n)
	}
	if len(setOptCalls(m.f)) != 0 || m.dirExists() {
		t.Fatalf("mirror must be left alone (dir exists %v)", m.dirExists())
	}
}

func TestRun_URLModeRefetchesDespiteFreshCache(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--url", testURL), "", "gh")
	m.f.on("tmux show-options -t @3 -wqv @branch", "feat", nil)
	m.f.on("gh pr view "+testURL, `{"number":5,"title":"t","url":"`+testURL+`","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"statusCheckRollup":[]}`, nil)
	m.mkdir(t)
	cache := filepath.Join(m.dir, sha1Key("url|"+testURL)+".json")
	if err := os.WriteFile(cache, []byte(openPRList), 0o644); err != nil {
		t.Fatal(err)
	}
	touchAt(cache, time.Unix(testNowUnix, 0))

	m.e.run(testSelf)

	if got := ghCallsMatching(m.f, "gh pr view "+testURL); len(got) != 1 {
		t.Fatalf("pr view calls = %d, want 1 (force)", len(got))
	}
	if got := setOptValue(t, m.f, "@pr_number"); got != "5" {
		t.Errorf("@pr_number = %q", got)
	}
	if got := setOptValue(t, m.f, "@pr_branch"); got != "feat" {
		t.Errorf("@pr_branch = %q, want @branch value", got)
	}
}

func TestRun_URLModeCreatesCacheDir(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--url", testURL), "")
	m.e.run(testSelf)
	if !m.dirExists() {
		t.Fatal("cache dir not created")
	}
}

func TestRun_URLModeWinsOverBranch(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--url", testURL, "--branch", "other", "--dir", "/d"), "", "gh")
	m.f.on("tmux show-options -t @3 -wqv @branch", "feat", nil)
	m.f.on("gh pr view "+testURL, `{"number":5,"title":"t","url":"`+testURL+`","state":"OPEN","statusCheckRollup":[]}`, nil)
	m.e.run(testSelf)
	for _, c := range m.f.snapshot() {
		if c.name == "git" || strings.HasPrefix(c.joined(), "gh pr list") {
			t.Fatalf("single-target path ran: %s", c.joined())
		}
	}
	if got := setOptValue(t, m.f, "@pr_branch"); got != "feat" {
		t.Errorf("@pr_branch = %q, want @branch value feat", got)
	}
}

func TestRun_SingleTargetMirrorRefused(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), "", "gh")
	m.f.on("tmux show-options -t @3 -wqv @bridge_win", "1", nil)
	m.e.run(testSelf)
	if len(m.f.byName("gh")) != 0 || len(m.f.byName("git")) != 0 || m.dirExists() {
		t.Fatalf("mirror must be refused early: %v", m.f.snapshot())
	}
}

const gitCommonDirPrefix = "git -C /d rev-parse --path-format=absolute --git-common-dir"

func TestRun_SingleTargetGoneRepoMalformedPRURL(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), "", "gh")
	m.f.on(gitCommonDirPrefix, "", errors.New("not a repo"))
	m.f.on("tmux show-options -t @3 -wqv @pr_url", "not-a-url", nil)
	m.e.run(testSelf)
	if len(m.f.byName("gh")) != 0 || len(setOptCalls(m.f)) != 0 {
		t.Fatalf("malformed @pr_url must stop: %v", m.f.snapshot())
	}
}

func TestRun_SingleTargetGoneRepoRefreshesByURLTaggedWithBranch(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), "", "gh")
	m.f.on(gitCommonDirPrefix, "", errors.New("not a repo"))
	m.f.on("tmux show-options -t @3 -wqv @pr_url", testURL, nil)
	m.f.on("gh pr view "+testURL, `{"number":5,"title":"t","url":"`+testURL+`","state":"MERGED","statusCheckRollup":[]}`, nil)
	m.e.run(testSelf)
	if got := ghCallsMatching(m.f, "gh pr view "+testURL); len(got) != 1 {
		t.Fatalf("pr view calls = %d, want 1", len(got))
	}
	if got := setOptValue(t, m.f, "@pr_branch"); got != "feat" {
		t.Errorf("@pr_branch = %q, want feat", got)
	}
	if got := setOptValue(t, m.f, "@pr_state"); got != "merged" {
		t.Errorf("@pr_state = %q", got)
	}
}

func TestRun_SingleTargetResolvableFetchesOpenInDir(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), "", "gh")
	m.f.on(gitCommonDirPrefix, "/r/.git", nil)
	m.f.on("gh pr list --head feat --state open", openPRList, nil)
	m.e.run(testSelf)

	list := ghCallsMatching(m.f, "gh pr list --head feat --state open")
	if len(list) != 1 || list[0].dir != "/d" {
		t.Fatalf("open list calls = %+v, want one in /d", list)
	}
	if got := setOptValue(t, m.f, "@pr_number"); got != "7" {
		t.Errorf("@pr_number = %q", got)
	}
	if got := setOptValue(t, m.f, "@pr_branch"); got != "feat" {
		t.Errorf("@pr_branch = %q", got)
	}
	if !m.dirExists() {
		t.Error("cache dir not created")
	}
	if _, err := os.Stat(filepath.Join(m.dir, sha1Key("/r/.git|feat")+".json")); err != nil {
		t.Errorf("cache not keyed by repo|branch: %v", err)
	}
}

func TestRun_SingleTargetPendingArmsMarker(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), testTmuxEv, "gh")
	m.f.on(gitCommonDirPrefix, "/r/.git", nil)
	key := sha1Key("/r/.git|feat")
	m.mkdir(t)
	cache := filepath.Join(m.dir, key+".json")
	pending, err := os.ReadFile("testdata/rollup-pending.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(openPRList), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := `[{"statusCheckRollup":` + strings.TrimSpace(string(pending)) + `,"headRefOid":"abc123"}]`
	if err := os.WriteFile(checksPath(cache), []byte(checks), 0o644); err != nil {
		t.Fatal(err)
	}
	touchAt(cache, time.Unix(testNowUnix, 0))

	m.e.run(testSelf)

	if len(m.f.byName("gh")) != 0 {
		t.Fatalf("fresh cache must not call gh: %v", m.f.snapshot())
	}
	if got := setOptValue(t, m.f, "@pr_check_state"); got != "pending" {
		t.Fatalf("@pr_check_state = %q, want pending", got)
	}
	marker := filepath.Join(m.dir, sha1Key("/r/.git")+".checks-pending"+m.e.srv)
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not armed: %v", err)
	}
	if got, want := strings.Split(string(b), "\n")[1], sha1Key("feat|abc123"); got != want {
		t.Errorf("fingerprint = %q, want %q", got, want)
	}
}

func TestRun_SingleTargetSettledDoesNotArmMarker(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--branch", "feat", "--dir", "/d"), "", "gh")
	m.f.on(gitCommonDirPrefix, "/r/.git", nil)
	m.f.on("gh pr list --head feat --state open", openPRList, nil)
	m.e.run(testSelf)
	matches, _ := filepath.Glob(filepath.Join(m.dir, "*.checks-pending*"))
	if len(matches) != 0 {
		t.Fatalf("unexpected markers %v", matches)
	}
}

func TestRun_TargetWithoutBranchOrURLFallsToGate(t *testing.T) {
	m := newMainEnv(t, argv("--target", "@3", "--tick"), "")
	m.e.run(testSelf)
	d := m.f.kind("detach")
	if len(d) != 1 || d[0].args[len(d[0].args)-1] != "--tick-run" {
		t.Fatalf("detach = %+v, want one --tick-run", d)
	}
}

func wantDetach(t *testing.T, f *fakeRunner, o opts, flag string) {
	t.Helper()
	d := f.kind("detach")
	if len(d) != 1 {
		t.Fatalf("detach calls = %+v, want exactly one %s", d, flag)
	}
	want := append(append([]string(nil), o.cfgArgs...), flag)
	if d[0].name != testSelf || !reflect.DeepEqual(d[0].args, want) {
		t.Fatalf("detach = %s %v, want %s %v", d[0].name, d[0].args, testSelf, want)
	}
}

func wantNoDetach(t *testing.T, m *mainEnv) {
	t.Helper()
	if d := m.f.kind("detach"); len(d) != 0 {
		t.Fatalf("unexpected detach %+v", d)
	}
}

func TestGate_NoLastTickDetachesFullPass(t *testing.T) {
	o := argv("--tick")
	m := newMainEnv(t, o, testTmuxEv)
	m.e.run(testSelf)
	wantDetach(t, m.f, o, "--tick-run")
	if !m.dirExists() {
		t.Fatal("cache dir not created")
	}
	if fileMtime(filepath.Join(m.dir, ".last-tick"+m.e.srv)) == 0 {
		t.Fatal(".last-tick not touched")
	}
}

func TestGate_StaleTickDetachesFullPass(t *testing.T) {
	o := argv("--tick")
	m := newMainEnv(t, o, testTmuxEv)
	last := m.stamp(t, ".last-tick"+m.e.srv, o.refreshSeconds+1)
	m.e.run(testSelf)
	wantDetach(t, m.f, o, "--tick-run")
	if got := fileMtime(last); got == testNowUnix-o.refreshSeconds-1 {
		t.Fatal(".last-tick not re-touched")
	}
}

func TestGate_FreshTickNoMarkerIsNoop(t *testing.T) {
	o := argv("--tick")
	m := newMainEnv(t, o, testTmuxEv)
	last := m.stamp(t, ".last-tick"+m.e.srv, 1)
	before := fileMtime(last)
	m.e.run(testSelf)
	wantNoDetach(t, m)
	if fileMtime(last) != before {
		t.Fatal(".last-tick must not be touched")
	}
}

func TestGate_PendingMarkerWithFreshPendingTickIsNoop(t *testing.T) {
	o := argv("--tick")
	m := newMainEnv(t, o, testTmuxEv)
	check, _, _ := pendingClocks(o.checkRefreshSeconds)
	m.stamp(t, ".last-tick"+m.e.srv, 1)
	m.stamp(t, "abc.checks-pending"+m.e.srv, 1)
	pt := m.stamp(t, ".last-pending-tick"+m.e.srv, check-1)
	before := fileMtime(pt)
	m.e.run(testSelf)
	wantNoDetach(t, m)
	if fileMtime(pt) != before {
		t.Fatal(".last-pending-tick must not be touched")
	}
}

func TestGate_PendingMarkerStalePendingTickDispatchesPending(t *testing.T) {
	check, _, _ := pendingClocks(300)
	for name, age := range map[string]int64{"stale": check + 1, "absent": -1} {
		t.Run(name, func(t *testing.T) {
			o := argv("--tick")
			m := newMainEnv(t, o, testTmuxEv)
			m.stamp(t, ".last-tick"+m.e.srv, 1)
			m.stamp(t, "abc.checks-pending"+m.e.srv, 1)
			pt := filepath.Join(m.dir, ".last-pending-tick"+m.e.srv)
			if age >= 0 {
				m.stamp(t, ".last-pending-tick"+m.e.srv, age)
			}
			m.e.run(testSelf)
			wantDetach(t, m.f, o, "--tick-run-pending")
			if fileMtime(pt) == 0 || fileMtime(pt) == testNowUnix-age && age >= 0 {
				t.Fatal(".last-pending-tick not touched")
			}
		})
	}
}

func TestGate_OtherServerMarkerDoesNotCount(t *testing.T) {
	o := argv("--tick")
	m := newMainEnv(t, o, testTmuxEv)
	m.stamp(t, ".last-tick"+m.e.srv, 1)
	m.stamp(t, "abc.checks-pending.srv_other", 1)
	m.e.run(testSelf)
	wantNoDetach(t, m)
}

func TestGate_ForceBypassesFreshTickAndStaysOutOfArgv(t *testing.T) {
	o := argv("--tick", "--force")
	m := newMainEnv(t, o, testTmuxEv)
	m.stamp(t, ".last-tick"+m.e.srv, 1)
	m.e.run(testSelf)
	wantDetach(t, m.f, o, "--tick-run")
	d := m.f.kind("detach")
	if len(d) == 0 {
		t.Fatal("no detach call recorded")
	}
	for _, a := range d[0].args {
		if a == "--force" {
			t.Fatal("--force leaked into detached argv")
		}
	}
}

func TestRun_TickRunModesCreateDirAndRunPass(t *testing.T) {
	for _, flag := range []string{"--tick-run", "--tick-run-pending"} {
		t.Run(flag, func(t *testing.T) {
			m := newMainEnv(t, argv(flag), "")
			m.e.run(testSelf)
			if !m.dirExists() {
				t.Fatal("cache dir not created")
			}
			calls := m.f.snapshot()
			if len(calls) != 1 || calls[0].name != "tmux" || calls[0].args[0] != "list-windows" {
				t.Fatalf("calls = %+v, want only list-windows", calls)
			}
		})
	}
}
