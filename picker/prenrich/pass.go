package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// listWindowsFormat is the full pass's single window read: the six fields the
// grouping needs, then the seven PR options the glyph sync recomputes from.
const listWindowsFormat = "#{session_id}:#{window_id}|#{@worktree}|#{@git_root}|#{@branch}|#{@bridge_win}|#{s/[|]/ /:@pr_url}" +
	"|#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_check_progress}|#{@pr_glyph}"

const glyphRereadFormat = "#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_check_progress}|#{@pr_glyph}"

const (
	maxPassEntries     = 30
	pendingMaxSeconds  = 1800
	pendingDueSlack    = 5
	identityBatchField = prFields + ",headRefName,isCrossRepository"
	checksBatchFields  = "headRefName,statusCheckRollup,headRefOid,isCrossRepository"
)

type windowRow struct {
	target, worktree, gitRoot, branch, bridgeWin, prURL string
	prNumber, prState, prCheck, prMergeable, prDraft    string
	prProgress, prGlyph                                 string
}

// parseWindowRows keeps rows of at least five fields; missing trailing fields
// read empty, and the last field absorbs any extra '|'.
func parseWindowRows(out string) []windowRow {
	var rows []windowRow
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.SplitN(line, "|", 13)
		if len(f) < 5 {
			continue
		}
		f = append(f, make([]string, 13-len(f))...)
		rows = append(rows, windowRow{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9], f[10], f[11], f[12]})
	}
	return rows
}

type groupWindow struct{ target, branch string }

type repoGroup struct {
	dir      string
	branches []string
	windows  []groupWindow
}

func (e *enricher) pendingMarker(repoID string) string {
	return filepath.Join(e.cacheDir, sha1Key(repoID)+".checks-pending"+e.srv)
}

func (e *enricher) gitCommonDir(d string) string {
	if d == "" {
		return ""
	}
	out, err := e.r.output("", "git", "-C", d, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return out
}

// runFullPass enriches every window carrying a @branch, one concurrent group
// per repo (git common dir) and per gone-checkout PR url. pendingOnly is the
// checks-only pass: just the repos whose pending marker is due.
func (e *enricher) runFullPass(pendingOnly bool) {
	now := e.now()
	refreshChecks := false
	checkTick := filepath.Join(e.cacheDir, ".last-check-tick"+e.srv)
	if !pendingOnly {
		_, err := os.Stat(checkTick)
		if err != nil || now.Unix()-fileMtime(checkTick) >= e.o.checkRefreshSeconds {
			refreshChecks = true
			touchAt(checkTick, now)
		}
	}
	out, _ := e.r.output("", "tmux", "list-windows", "-a", "-F", listWindowsFormat)
	rows := parseWindowRows(out)
	// Windows the filters below skip still render their leftover @pr_* options.
	e.glyphSync(rows)

	groups := map[string]*repoGroup{}
	urlWindows := map[string][]groupWindow{}
	seen := map[string]bool{}
	total, truncated := 0, false
	for _, w := range rows {
		if w.branch == "" || w.bridgeWin == "1" {
			continue
		}
		d := w.worktree
		if d == "" {
			d = w.gitRoot
		}
		key := e.gitCommonDir(d)
		// Stale @worktree (dir removed under the window): retry @git_root.
		if key == "" && w.gitRoot != "" && w.gitRoot != d {
			d = w.gitRoot
			key = e.gitCommonDir(d)
		}
		gw := groupWindow{w.target, w.branch}
		if key == "" {
			// Checkout gone: last-known options would show the PR open forever,
			// but a well-formed @pr_url names its own repo.
			if !prURLWellformed(w.prURL) {
				continue
			}
			sk := "url|" + w.prURL
			if !seen[sk] {
				if total >= maxPassEntries {
					truncated = true
					continue
				}
				seen[sk] = true
				total++
			}
			urlWindows[w.prURL] = append(urlWindows[w.prURL], gw)
			continue
		}
		g := groups[key]
		if g == nil {
			g = &repoGroup{}
			groups[key] = g
		}
		g.windows = append(g.windows, gw)
		sk := key + "|" + w.branch
		if seen[sk] {
			continue
		}
		if total >= maxPassEntries {
			truncated = true
			continue
		}
		seen[sk] = true
		g.dir = d
		g.branches = append(g.branches, w.branch)
		total++
	}
	markers := map[string]string{}
	live := map[string]bool{}
	for key, g := range groups {
		if len(g.branches) == 0 {
			continue
		}
		m := e.pendingMarker(key)
		markers[key] = m
		live[m] = true
	}
	// Only enrichRepoGroup clears a marker, and a repo that lost its last
	// window never reaches it again. A capped pass may have skipped a live
	// repo, so it removes nothing.
	if !truncated {
		e.sweepMarkers(live)
	}
	if total == 0 {
		return
	}

	pendingCheck, pendingSlow, pendingFast := pendingClocks(e.o.checkRefreshSeconds)
	fast := false
	var wg sync.WaitGroup
	for key, m := range markers {
		due := false
		b, err := os.ReadFile(m) //nolint:gosec // marker path is the poller's own cache dir plus a sha1 key
		present := err == nil
		if present {
			age := now.Unix() - markerFirstSeen(b)
			if age < pendingMaxSeconds {
				fast = true
				interval := pendingCheck
				if age >= pendingFast {
					interval = pendingSlow
				}
				due = now.Unix()-fileMtime(m) >= interval-pendingDueSlack
			}
		}
		if pendingOnly && !due {
			continue
		}
		// Stamp the refresh's start, not its end: the gate measures from its
		// own dispatch, and a concurrent pass must not see this repo as due.
		if present && (refreshChecks || due) {
			touchAt(m, now)
		}
		g := groups[key]
		wg.Go(func() {
			e.enrichRepoGroup(g.dir, key, g.branches, g.windows, refreshChecks || due, !pendingOnly)
		})
	}
	if !pendingOnly {
		for u, ws := range urlWindows {
			wg.Go(func() { e.refreshURLPR(u, ws) })
		}
	}
	wg.Wait()
	// Every marker is past pendingMaxSeconds: push .last-pending-tick a settled
	// refresh ahead so the gate stops launching passes that find nothing due.
	// armPendingMarker drops it the moment a repo goes pending again.
	if pendingOnly && !fast {
		touchAt(filepath.Join(e.cacheDir, ".last-pending-tick"+e.srv), now.Add(time.Duration(e.o.checkRefreshSeconds)*time.Second))
	}
}

// sweepMarkers removes this server's pending markers that no live repo owns.
// The cache dir is matched literally, never as a glob pattern.
func (e *enricher) sweepMarkers(live map[string]bool) {
	entries, err := os.ReadDir(e.cacheDir)
	if err != nil {
		return
	}
	suffix := ".checks-pending" + e.srv
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), suffix) {
			continue
		}
		m := filepath.Join(e.cacheDir, ent.Name())
		if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() || live[m] {
			continue
		}
		_ = os.Remove(m)
	}
}

// markerFirstSeen is the marker's line-1 epoch; anything but digits reads 0.
func markerFirstSeen(b []byte) int64 {
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.Trim(line, " \t")
	if line == "" || strings.IndexFunc(line, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0
	}
	n, _ := strconv.ParseInt(line, 10, 64)
	return n
}

// enrichRepoGroup is one repo's slice of the full pass. A successful identity
// batch is authoritative for open PRs: a head missing from it has none, so only
// a terminal lookup is left. refreshIdentity false is a checks-only pass.
func (e *enricher) enrichRepoGroup(dir, repoID string, branches []string, windows []groupWindow, refreshChecks, refreshIdentity bool) {
	cachePath := func(br string) string { return filepath.Join(e.cacheDir, sha1Key(repoID+"|"+br)+".json") }
	if refreshIdentity {
		var openPR map[string][]byte
		batchOK := false
		if e.r.lookPath("gh") {
			out, err := e.r.output(dir, "gh", "pr", "list", "--state", "open", "--limit", "100", "--json", identityBatchField)
			if err == nil && out != "" {
				batchOK = true
				openPR = indexSameRepo([]byte(out), func(pr map[string]json.RawMessage) any {
					delete(pr, "isCrossRepository")
					return []map[string]json.RawMessage{pr}
				})
			}
		}
		for _, br := range branches {
			key := sha1Key(repoID + "|" + br)
			switch obj, ok := openPR[br]; {
			case ok:
				_ = writeAtomic(cachePath(br), obj)
			case batchOK:
				e.fetchPRCached(dir, br, key, "all", ttlTerminal)
			default:
				// The batch failed: nothing is ruled out, so run the full lookup,
				// which serves the cache on failure rather than wiping to "none".
				e.fetchPRCached(dir, br, key, "open+all", ttlNone)
			}
		}
	}
	if refreshIdentity && !refreshChecks {
		refreshChecks = slices.ContainsFunc(branches, func(br string) bool { return headStale(cachePath(br)) })
	}
	if refreshChecks {
		e.refreshRepoChecks(dir, repoID, branches)
	}

	var pending []string
	for _, br := range branches {
		cache := cachePath(br)
		for _, w := range windows {
			if w.branch != br {
				continue
			}
			if e.applyCache(w.target, cache, br) == "pending" {
				pending = append(pending, br+"|"+episodeHead(cache))
			}
		}
	}
	if len(pending) > 0 {
		e.armPendingMarker(repoID, pendingFingerprint(pending))
		return
	}
	_ = os.Remove(e.pendingMarker(repoID))
}

// indexSameRepo maps each non-fork PR's headRefName to the JSON of conv(pr);
// a later PR on the same head wins. Malformed input yields an empty index.
func indexSameRepo(raw []byte, conv func(map[string]json.RawMessage) any) map[string][]byte {
	idx := map[string][]byte{}
	var prs []map[string]json.RawMessage
	if json.Unmarshal(raw, &prs) != nil {
		return idx
	}
	for _, pr := range prs {
		if cross, ok := pr["isCrossRepository"]; ok && string(cross) != "null" && string(cross) != "false" {
			continue
		}
		var head string
		if json.Unmarshal(pr["headRefName"], &head) != nil || head == "" {
			continue
		}
		b, err := json.Marshal(conv(pr))
		if err != nil {
			continue
		}
		idx[head] = b
	}
	return idx
}

// refreshRepoChecks refreshes a repo's open-PR rollups. Identity has its own,
// faster cache, so routine refreshes never combine the two GraphQL selections.
func (e *enricher) refreshRepoChecks(dir, repoID string, branches []string) {
	if !e.r.lookPath("gh") {
		return
	}
	out, err := e.r.output(dir, "gh", "pr", "list", "--state", "open", "--limit", "100", "--json", checksBatchFields)
	if err != nil || out == "" {
		return
	}
	type checks struct {
		Rollup json.RawMessage `json:"statusCheckRollup"`
		Head   json.RawMessage `json:"headRefOid"`
	}
	idx := indexSameRepo([]byte(out), func(pr map[string]json.RawMessage) any {
		return []checks{{Rollup: pr["statusCheckRollup"], Head: pr["headRefOid"]}}
	})
	for _, br := range branches {
		obj, ok := idx[br]
		if !ok {
			obj = []byte("[]")
		}
		_ = writeAtomic(filepath.Join(e.cacheDir, sha1Key(repoID+"|"+br)+".checks.json"), obj)
	}
}

// headRefOid is jq's `.[0].headRefOid // ""` over a checks cache.
func headRefOid(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // checks cache path is the poller's own cache dir plus a sha1 key
	if err != nil {
		return ""
	}
	var prs []map[string]json.RawMessage
	if json.Unmarshal(b, &prs) != nil || len(prs) == 0 {
		return ""
	}
	raw := prs[0]["headRefOid"]
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return ""
	}
	return string(raw)
}

// pendingFingerprint is sha1 over the sorted unique "branch|headRefOid" lines.
func pendingFingerprint(lines []string) string {
	s := slices.Clone(lines)
	slices.Sort(s)
	return sha1Key(strings.Join(slices.Compact(s), "\n"))
}

// armPendingMarker reconciles the repo's marker with the current pending head
// set. A new or changed fingerprint restarts the episode at now and drops
// .last-pending-tick, undoing a pass's push-ahead; an unchanged one keeps the
// content, whose mtime the pass already stamped at the refresh start.
func (e *enricher) armPendingMarker(repoID, fp string) {
	m := e.pendingMarker(repoID)
	if b, err := os.ReadFile(m); err == nil { //nolint:gosec // marker path is the poller's own cache dir plus a sha1 key
		lines := strings.SplitN(string(b), "\n", 3)
		cur := ""
		if len(lines) > 1 {
			cur = lines[1]
		}
		if cur == fp {
			return
		}
	}
	if os.WriteFile(m, []byte(strconv.FormatInt(e.now().Unix(), 10)+"\n"+fp+"\n"), 0o644) != nil { //nolint:gosec // cache files are not secret
		return
	}
	_ = os.Remove(filepath.Join(e.cacheDir, ".last-pending-tick"+e.srv))
}

// refreshURLPR refreshes a PR by url for windows whose checkout is gone; `gh pr
// view <url>` needs no repo context. A failed call leaves the cache alone.
func (e *enricher) refreshURLPR(url string, windows []groupWindow) {
	key := sha1Key("url|" + url)
	cache := filepath.Join(e.cacheDir, key+".json")
	if !e.serveCache(cache, ttlNone) && e.r.lookPath("gh") {
		e.fetchURLPR(url, key, cache)
	}
	for _, w := range windows {
		e.applyCache(w.target, cache, w.branch)
	}
}

func (e *enricher) fetchURLPR(url, key, cache string) {
	release, ok := acquireLock(filepath.Join(e.cacheDir, key+".lock"))
	if !ok {
		return
	}
	defer release()
	out, err := e.r.output("", "gh", "pr", "view", url, "--json", prFields+",statusCheckRollup")
	if err != nil {
		e.logEvent("event", "pr_url_fail", "url", url)
		return
	}
	var pr map[string]json.RawMessage
	if out == "" || json.Unmarshal([]byte(out), &pr) != nil {
		return
	}
	rollup := pr["statusCheckRollup"]
	if len(rollup) == 0 || string(rollup) == "null" || string(rollup) == "false" {
		rollup = json.RawMessage("[]")
	}
	delete(pr, "statusCheckRollup")
	if b, err := json.Marshal([]map[string]json.RawMessage{pr}); err == nil {
		_ = writeAtomic(cache, b)
	}
	if b, err := json.Marshal([]map[string]json.RawMessage{{"statusCheckRollup": rollup, "headRefOid": pr["headRefOid"]}}); err == nil {
		_ = writeAtomic(checksPath(cache), b)
	}
}

// glyphSync rewrites every non-mirror window's stale @pr_glyph. The options
// are re-read just before each write so a concurrent writer's newer values win.
func (e *enricher) glyphSync(rows []windowRow) {
	var sessions []string
	firstTarget := map[string]string{}
	for _, w := range rows {
		if w.bridgeWin == "1" {
			continue
		}
		if prGlyph(e.o.icons, w.prNumber, w.prState, w.prCheck, w.prMergeable, w.prDraft, w.prProgress) == w.prGlyph {
			continue
		}
		out, err := e.r.output("", "tmux", "display-message", "-t", w.target, "-p", glyphRereadFormat)
		if err != nil {
			continue
		}
		f := strings.SplitN(out, "|", 7)
		f = append(f, make([]string, 7-len(f))...)
		want := prGlyph(e.o.icons, f[0], f[1], f[2], f[3], f[4], f[5])
		if want == f[6] {
			continue
		}
		_ = e.r.run("tmux", "set-option", "-t", w.target, "-w", "@pr_glyph", want)
		sess, _, _ := strings.Cut(w.target, ":")
		if _, ok := firstTarget[sess]; !ok {
			firstTarget[sess] = w.target
			sessions = append(sessions, sess)
		}
	}
	if e.o.reflow == "" {
		return
	}
	for _, s := range sessions {
		name, _ := e.r.output("", "tmux", "display-message", "-t", firstTarget[s], "-p", "#{session_name}")
		e.r.detach(e.o.reflow, name, "--force")
	}
}
