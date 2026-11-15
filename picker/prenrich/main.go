package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func newEnricher(o opts, r runner, getenv func(string) string, now func() time.Time) *enricher {
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	return &enricher{
		r:        r,
		o:        o,
		cacheDir: env("OG_ENRICH_CACHE_DIR", "/tmp/og-pr"),
		srv:      srvSuffix(getenv("TMUX")),
		now:      now,
		sentinel: env("OG_DEBUG_SENTINEL", "/tmp/og-debug.on"),
	}
}

func (e *enricher) mkCacheDir() { _ = os.MkdirAll(e.cacheDir, 0o755) } //nolint:gosec // cache dir is shared by the user's own tmux servers

func (e *enricher) isMirror(target string) bool {
	out, _ := e.r.output("", "tmux", "show-options", "-t", target, "-wqv", "@bridge_win")
	return out == "1"
}

func (e *enricher) run(self string) {
	o := e.o
	switch {
	case o.mode == "mock":
		e.runMock()
	case o.mode == "tickrun":
		e.mkCacheDir()
		e.runFullPass(false)
	case o.mode == "tickrunpending":
		e.mkCacheDir()
		e.runFullPass(true)
	case o.target != "" && o.url != "":
		e.runURL()
	case o.target != "" && o.branch != "":
		e.runSingleTarget()
	default:
		e.runTickGate(self)
	}
}

func (e *enricher) runMock() {
	m := e.o.mock
	if e.o.target == "" {
		return
	}
	e.writePROptions(e.o.target, prValues{
		number: m.number, title: sanitizeTitle(m.title), state: m.state, check: m.check,
		url: m.url, mergeable: m.mergeable, branch: e.o.branch,
		draft: m.draft, review: m.review, autoMerge: m.autoMerge, progress: m.progress,
	})
}

// runURL is the on-demand refresh for a window whose checkout is gone: it tags
// with the window's own @branch and refetches regardless of TTL.
func (e *enricher) runURL() {
	o := e.o
	if !prURLWellformed(o.url) || e.isMirror(o.target) {
		return
	}
	e.mkCacheDir()
	bt, _ := e.r.output("", "tmux", "show-options", "-t", o.target, "-wqv", "@branch")
	e.o.force = true
	e.refreshURLPR(o.url, []groupWindow{{o.target, bt}})
}

func (e *enricher) runSingleTarget() {
	o := e.o
	if e.isMirror(o.target) {
		return
	}
	e.mkCacheDir()
	repo := e.gitCommonDir(o.dir)
	if repo == "" {
		// The checkout is gone: the window's @pr_url is the only repo context left.
		url, _ := e.r.output("", "tmux", "show-options", "-t", o.target, "-wqv", "@pr_url")
		if !prURLWellformed(url) {
			return
		}
		e.refreshURLPR(url, []groupWindow{{o.target, o.branch}})
		return
	}
	key := sha1Key(repo + "|" + o.branch)
	cache := e.fetchPRCached(o.dir, o.branch, key, "open+all", ttlNone)
	if e.applyCache(o.target, cache, o.branch) != "pending" {
		return
	}
	e.armPendingMarker(repo, sha1Key(o.branch+"|"+headRefOid(checksPath(cache))))
}

// hasPendingMarker reports whether this tmux server has a repo with pending checks.
func (e *enricher) hasPendingMarker() bool {
	entries, err := os.ReadDir(e.cacheDir)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".checks-pending"+e.srv) {
			return true
		}
	}
	return false
}

// runTickGate is the cheap per-tick check: it detaches a full pass only when due.
func (e *enricher) runTickGate(self string) {
	now := e.now()
	lastTick := filepath.Join(e.cacheDir, ".last-tick"+e.srv)
	if last := fileMtime(lastTick); !e.o.force && last != 0 && now.Unix()-last < e.o.refreshSeconds {
		if !e.hasPendingMarker() {
			return
		}
		pendingTick := filepath.Join(e.cacheDir, ".last-pending-tick"+e.srv)
		check, _, _ := pendingClocks(e.o.checkRefreshSeconds)
		if p := fileMtime(pendingTick); p != 0 && now.Unix()-p < check {
			return
		}
		touchAt(pendingTick, now)
		e.r.detach(self, append(append([]string(nil), e.o.cfgArgs...), "--tick-run-pending")...)
		return
	}
	e.mkCacheDir()
	touchAt(lastTick, now)
	e.r.detach(self, append(append([]string(nil), e.o.cfgArgs...), "--tick-run")...)
}

func main() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	newEnricher(parseArgs(os.Args[1:]), execRunner{}, os.Getenv, time.Now).run(self)
}
