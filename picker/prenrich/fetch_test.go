package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	listFields  = "number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest"
	openPRList  = `[{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"isCrossRepository":false}]`
	forkPRList  = `[{"number":9,"title":"fork","url":"f","state":"OPEN","isCrossRepository":true}]`
	testNowUnix = 1_700_000_000
)

func testEnricher(t *testing.T, r runner, o opts) *enricher {
	t.Helper()
	return &enricher{
		r:        r,
		o:        o,
		cacheDir: t.TempDir(),
		srv:      "",
		now:      func() time.Time { return time.Unix(testNowUnix, 0) },
		sentinel: filepath.Join(t.TempDir(), "og-debug.on"),
	}
}

func ghArgs(state string, force bool) []string {
	f := listFields
	if force {
		f += ",statusCheckRollup,headRefOid"
	}
	return []string{"pr", "list", "--head", "feat", "--state", state, "--limit", "100", "--json", f + ",isCrossRepository"}
}

func ghPrefix(state string) string {
	return "gh pr list --head feat --state " + state + " "
}

func readJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v (%s)", path, err, b)
	}
	return v
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFetchOpenAll_SamePRSingleCall(t *testing.T) {
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), openPRList, nil)
	e := testEnricher(t, f, opts{})
	got := e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	cache := filepath.Join(e.cacheDir, "k1.json")
	if got != cache {
		t.Fatalf("path = %q, want %q", got, cache)
	}
	calls := f.byName("gh")
	if len(calls) != 1 {
		t.Fatalf("gh calls = %d, want 1", len(calls))
	}
	if !reflect.DeepEqual(calls[0].args, ghArgs("open", false)) {
		t.Errorf("argv = %v", calls[0].args)
	}
	if calls[0].dir != "/repo" {
		t.Errorf("dir = %q", calls[0].dir)
	}
	want := mustJSON(t, `[{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null}]`)
	if !reflect.DeepEqual(readJSON(t, cache), want) {
		t.Errorf("cache = %v", readJSON(t, cache))
	}
}

func TestFetchOpenAll_FallsBackToAll(t *testing.T) {
	for name, openAnswer := range map[string]string{"empty": `[]`, "fork only": forkPRList} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRunner("gh")
			f.on(ghPrefix("open"), openAnswer, nil)
			f.on(ghPrefix("all"), strings.Replace(openPRList, "OPEN", "MERGED", 1), nil)
			e := testEnricher(t, f, opts{})
			got := e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
			calls := f.byName("gh")
			if len(calls) != 2 {
				t.Fatalf("gh calls = %d, want 2", len(calls))
			}
			if !reflect.DeepEqual(calls[1].args, ghArgs("all", false)) {
				t.Errorf("second argv = %v", calls[1].args)
			}
			arr, _ := readJSON(t, got).([]any)
			if len(arr) != 1 || arr[0].(map[string]any)["state"] != "MERGED" {
				t.Errorf("cache = %v", arr)
			}
		})
	}
}

func TestFetchStatesAll_OnlyAllCall(t *testing.T) {
	f := newFakeRunner("gh")
	f.on(ghPrefix("all"), openPRList, nil)
	e := testEnricher(t, f, opts{})
	e.fetchPRCached("/repo", "feat", "k1", "all", ttlTerminal)
	calls := f.byName("gh")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, ghArgs("all", false)) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestFetchForce_SplitsChecksCache(t *testing.T) {
	rollup := `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]`
	list := `[{"number":7,"state":"OPEN","statusCheckRollup":` + rollup + `,"headRefOid":"abc","isCrossRepository":false}]`
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), list, nil)
	e := testEnricher(t, f, opts{force: true})
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if got := f.byName("gh")[0].args; !reflect.DeepEqual(got, ghArgs("open", true)) {
		t.Errorf("argv = %v", got)
	}
	ident, _ := readJSON(t, filepath.Join(e.cacheDir, "k1.json")).([]any)
	if _, has := ident[0].(map[string]any)["statusCheckRollup"]; has {
		t.Errorf("identity cache kept statusCheckRollup: %v", ident)
	}
	want := mustJSON(t, `[{"statusCheckRollup":`+rollup+`,"headRefOid":"abc"}]`)
	if got := readJSON(t, filepath.Join(e.cacheDir, "k1.checks.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("checks cache = %v", got)
	}
}

func TestFetchForce_NullRollupBecomesEmpty(t *testing.T) {
	list := `[{"number":7,"state":"OPEN","statusCheckRollup":null,"headRefOid":"abc","isCrossRepository":false}]`
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), list, nil)
	e := testEnricher(t, f, opts{force: true})
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	want := mustJSON(t, `[{"statusCheckRollup":[],"headRefOid":"abc"}]`)
	if got := readJSON(t, filepath.Join(e.cacheDir, "k1.checks.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("checks cache = %v", got)
	}
}

func TestFetchGhFailureKeepsCache(t *testing.T) {
	f := newFakeRunner("gh")
	f.on("gh ", "", errors.New("exit status 1"))
	e := testEnricher(t, f, opts{force: true})
	cache := filepath.Join(e.cacheDir, "k1.json")
	old := []byte(`[{"number":1}]`)
	if err := os.WriteFile(cache, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone); got != cache {
		t.Errorf("path = %q", got)
	}
	if b, _ := os.ReadFile(cache); string(b) != string(old) {
		t.Errorf("cache rewritten: %s", b)
	}
}

func TestFetchNoGh(t *testing.T) {
	f := newFakeRunner()
	e := testEnricher(t, f, opts{})
	got := e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if got != filepath.Join(e.cacheDir, "k1.json") {
		t.Errorf("path = %q", got)
	}
	if n := len(f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %d", n)
	}
}

func TestFetchServesFreshCacheFetchesStale(t *testing.T) {
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), openPRList, nil)
	e := testEnricher(t, f, opts{})
	cache := filepath.Join(e.cacheDir, "k1.json")
	if err := os.WriteFile(cache, []byte(`[{"number":7,"state":"OPEN"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	now := e.now()
	if err := os.Chtimes(cache, now, now); err != nil {
		t.Fatal(err)
	}
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if n := len(f.byName("gh")); n != 0 {
		t.Fatalf("fresh cache fetched: %d gh calls", n)
	}
	stale := now.Add(-61 * time.Second)
	if err := os.Chtimes(cache, stale, stale); err != nil {
		t.Fatal(err)
	}
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if n := len(f.byName("gh")); n != 1 {
		t.Fatalf("stale cache gh calls = %d, want 1", n)
	}
}

func TestFetchHeldLockSkipsFetch(t *testing.T) {
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), openPRList, nil)
	e := testEnricher(t, f, opts{})
	lock := filepath.Join(e.cacheDir, "k1.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if n := len(f.byName("gh")); n != 0 {
		t.Errorf("gh calls = %d with lock held", n)
	}
}

func TestFetchReleasesLock(t *testing.T) {
	f := newFakeRunner("gh")
	f.on(ghPrefix("open"), openPRList, nil)
	e := testEnricher(t, f, opts{})
	e.fetchPRCached("/repo", "feat", "k1", "open+all", ttlNone)
	if _, err := os.Stat(filepath.Join(e.cacheDir, "k1.lock")); !os.IsNotExist(err) {
		t.Errorf("lock still present: %v", err)
	}
}
