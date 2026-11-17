package main

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// Regression tests for #1170: a cached check state for an old head must not
// outlive a push, and a rollup holding only settled entries on a head that has
// only just appeared must not read as success.

const (
	headRepo   = "/a/.git"
	headBranch = "feat/x"

	skippedSuccessRollup = `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"},{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]`
	skippedOnlyRollup    = `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"}]`
	failureRollup        = `[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]`
)

// isoAgo is the gh createdAt timestamp for a PR opened ageSec before testNowUnix.
func isoAgo(ageSec int64) string {
	return time.Unix(testNowUnix-ageSec, 0).UTC().Format(time.RFC3339)
}

// prObjHead is prObj with the live head commit and creation time gh reports.
func prObjHead(branch, oid, createdAt string) string {
	return `{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"headRefName":"` + branch + `","headRefOid":"` + oid + `","createdAt":"` + createdAt + `","isCrossRepository":false}`
}

// headPassEnv is one window on headBranch with a fresh checks tick, so a
// checks batch runs only when the production code asks for one.
func headPassEnv(t *testing.T) *passEnv {
	t.Helper()
	p := newPassEnv(t, opts{})
	p.windows(row("$1:@1", "/a", "", headBranch, ""))
	p.repo("/a", headRepo)
	p.setTick(".last-check-tick", testNowUnix)
	return p
}

func (p *passEnv) seedChecks(cached string) {
	p.t.Helper()
	writeCache(p.t, p.e.cacheDir, p.key(headRepo, headBranch)+".checks.json", cached)
}

// seedStamp writes the per-branch head-seen stamp: "<oid>\n<first-seen epoch>".
func (p *passEnv) seedStamp(oid string, firstSeen int64) {
	p.t.Helper()
	writeCache(p.t, p.e.cacheDir, p.key(headRepo, headBranch)+".head-seen", oid+"\n"+strconv.FormatInt(firstSeen, 10)+"\n")
}

func lastOpt(t *testing.T, p *passEnv, opt string) string {
	t.Helper()
	var val string
	found := false
	for _, c := range setOptCalls(p.f) {
		if c.args[len(c.args)-2] == opt {
			val, found = c.args[len(c.args)-1], true
		}
	}
	if !found {
		t.Fatalf("no set-option for %s in %v", opt, p.f.snapshot())
	}
	return val
}

func checksBatchCalls(p *passEnv) int { return len(ghCalls(p.f, checksBatchArgs)) }

func TestHeadStale_PushedHeadRefetchesChecks(t *testing.T) {
	p := headPassEnv(t)
	p.seedChecks("[" + checksObj(headBranch, "A", failureRollup) + "]")
	p.gh(identBatchArgs, "["+prObjHead(headBranch, "B", isoAgo(86400))+"]", nil)
	p.gh(checksBatchArgs, "["+checksObj(headBranch, "B", pendingRollup)+"]", nil)
	p.e.runFullPass(false)

	if n := checksBatchCalls(p); n != 1 {
		t.Errorf("checks batch calls = %d, want 1", n)
	}
	if got := lastOpt(t, p, "@pr_check_state"); got != "pending" {
		t.Errorf("@pr_check_state = %q, want pending (cached failure is for the old head)", got)
	}
}

func TestHeadStale_SameHeadDoesNotRefetch(t *testing.T) {
	p := headPassEnv(t)
	p.seedChecks("[" + checksObj(headBranch, "A", failureRollup) + "]")
	p.gh(identBatchArgs, "["+prObjHead(headBranch, "A", isoAgo(86400))+"]", nil)
	p.gh(checksBatchArgs, "["+checksObj(headBranch, "A", successRollup)+"]", nil)
	p.e.runFullPass(false)

	if n := checksBatchCalls(p); n != 0 {
		t.Errorf("checks batch calls = %d, want 0 for an unchanged head", n)
	}
	if got := lastOpt(t, p, "@pr_check_state"); got != "failure" {
		t.Errorf("@pr_check_state = %q, want failure", got)
	}
}

func TestHeadStale_RefetchFailureReadsPending(t *testing.T) {
	p := headPassEnv(t)
	p.seedChecks("[" + checksObj(headBranch, "A", failureRollup) + "]")
	p.gh(identBatchArgs, "["+prObjHead(headBranch, "B", isoAgo(86400))+"]", nil)
	p.gh(checksBatchArgs, "", errors.New("offline"))
	p.e.runFullPass(false)

	if got := lastOpt(t, p, "@pr_check_state"); got != "pending" {
		t.Errorf("@pr_check_state = %q, want pending", got)
	}
	if got := lastOpt(t, p, "@pr_glyph"); got != testIcons.Pending {
		t.Errorf("@pr_glyph = %q, want the pending glyph %q", got, testIcons.Pending)
	}
}

func TestHeadStale_NewPRSeenLateWithoutChecksHead(t *testing.T) {
	for name, seed := range map[string]string{"no checks cache": "", "empty checks cache": "[]"} {
		t.Run(name, func(t *testing.T) {
			p := headPassEnv(t)
			if seed != "" {
				p.seedChecks(seed)
			}
			p.gh(identBatchArgs, "["+prObjHead(headBranch, "H", isoAgo(1000))+"]", nil)
			p.gh(checksBatchArgs, "", errors.New("offline"))
			p.e.runFullPass(false)

			if n := checksBatchCalls(p); n != 1 {
				t.Errorf("checks batch calls = %d, want 1", n)
			}
			if got := lastOpt(t, p, "@pr_check_state"); got != "pending" {
				t.Errorf("@pr_check_state = %q, want pending", got)
			}
			if got := lastOpt(t, p, "@pr_glyph"); got == testIcons.Success {
				t.Errorf("@pr_glyph = %q, a PR with no verified checks must not show success", got)
			}
		})
	}
}

func TestHeadGrace_SettledRollupOnNewHead(t *testing.T) {
	const head = "H"
	cases := []struct {
		name      string
		createdAt int64 // PR age in seconds
		stamp     string
		stampAge  int64 // seconds since the stamp's first-seen epoch
		rollup    string
		want      string
	}{
		{name: "brand-new PR, no stamp", createdAt: 30, rollup: skippedSuccessRollup, want: "pending"},
		{name: "push, stamp holds another oid", createdAt: 86400, stamp: "old", stampAge: 1000, rollup: skippedSuccessRollup, want: "pending"},
		{name: "same head first seen recently", createdAt: 86400, stamp: head, stampAge: 30, rollup: skippedSuccessRollup, want: "pending"},
		{name: "same head first seen past the grace", createdAt: 30, stamp: head, stampAge: 300, rollup: skippedSuccessRollup, want: "success"},
		{name: "brand-new PR at the grace boundary", createdAt: 120, rollup: skippedSuccessRollup, want: "success"},
		{name: "brand-new PR long past the grace", createdAt: 500, rollup: skippedSuccessRollup, want: "success"},
		{name: "old PR, no stamp", createdAt: 86400, rollup: skippedSuccessRollup, want: "success"},
		{name: "failure inside the grace stays failure", createdAt: 30, rollup: failureRollup, want: "failure"},
		{name: "all skipped, young", createdAt: 30, rollup: skippedOnlyRollup, want: "pending"},
		{name: "all skipped, old", createdAt: 86400, rollup: skippedOnlyRollup, want: "pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := headPassEnv(t)
			p.seedChecks("[" + checksObj(headBranch, head, c.rollup) + "]")
			if c.stamp != "" {
				p.seedStamp(c.stamp, testNowUnix-c.stampAge)
			}
			p.gh(identBatchArgs, "["+prObjHead(headBranch, head, isoAgo(c.createdAt))+"]", nil)
			p.e.runFullPass(false)

			if got := lastOpt(t, p, "@pr_check_state"); got != c.want {
				t.Errorf("@pr_check_state = %q, want %q", got, c.want)
			}
			if c.want == "pending" {
				if got := lastOpt(t, p, "@pr_glyph"); got != testIcons.Pending {
					t.Errorf("@pr_glyph = %q, want the pending glyph %q", got, testIcons.Pending)
				}
			}
			if n := checksBatchCalls(p); n != 0 {
				t.Errorf("checks batch calls = %d, want 0 (head matches the checks cache)", n)
			}
		})
	}
}

func TestHeadGrace_MergedPRIsNeverPending(t *testing.T) {
	p := headPassEnv(t)
	p.seedChecks("[" + checksObj(headBranch, "H", skippedSuccessRollup) + "]")
	p.gh(identBatchArgs, "[]", nil)
	p.gh("pr list --head "+headBranch+" --state all",
		`[{"number":7,"title":"t","url":"u","state":"MERGED","mergeable":"UNKNOWN","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"headRefOid":"H","createdAt":"`+isoAgo(30)+`","isCrossRepository":false}]`, nil)
	p.e.runFullPass(false)

	if got := lastOpt(t, p, "@pr_check_state"); got != "success" {
		t.Errorf("@pr_check_state = %q, want success for a merged PR with a fresh head", got)
	}
	if got := lastOpt(t, p, "@pr_glyph"); got != testIcons.Merged {
		t.Errorf("@pr_glyph = %q, want the merged glyph %q", got, testIcons.Merged)
	}
}

// An open PR outside the top-100 batch has an identity head (per-head lookup)
// but never a checks head; refetching checks for it on every pass would burn
// a query per pass and pin the badge to pending.
func TestHeadStale_PRMissingFromChecksBatchRefetchesOnce(t *testing.T) {
	p := headPassEnv(t)
	p.gh(identBatchArgs, "[]", nil)
	p.gh("pr list --head "+headBranch+" --state all",
		"["+prObjHead(headBranch, "H", isoAgo(86400))+"]", nil)
	p.gh(checksBatchArgs, "["+checksObj("other", "Z", successRollup)+"]", nil)

	p.e.runFullPass(false)
	first := checksBatchCalls(p)
	if first > 1 {
		t.Errorf("first pass checks batch calls = %d, want at most 1", first)
	}

	p.e.runFullPass(false)
	if n := checksBatchCalls(p) - first; n != 0 {
		t.Errorf("second pass checks batch calls = %d, want 0 (at most one refetch per head)", n)
	}
	if got := lastOpt(t, p, "@pr_check_state"); got == "pending" {
		t.Errorf("@pr_check_state stuck at pending after the refetch settled")
	}
}
