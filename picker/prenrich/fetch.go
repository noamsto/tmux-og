package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Seconds a cached gh answer is trusted: an open PR (ttl), a "no PR" (ttlNone),
// and a merged or closed PR (ttlTerminal), which only a reopen can change.
const (
	ttl         = 60
	ttlNone     = 15
	ttlTerminal = 3600
)

const prFields = "number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefOid,createdAt"

type enricher struct {
	r        runner
	o        opts
	cacheDir string
	srv      string
	now      func() time.Time
	sentinel string
}

// fetchPRCached returns the identity cache path, refreshing it via gh when the
// cache decision says fetch. states is "open+all" (open PR first, then
// merged/closed) or "all". A dir that is not a directory fails every gh call
// (cmd.Dir). A failed fetch never touches the cache, so the last-known PR keeps
// showing instead of wiping to "none".
func (e *enricher) fetchPRCached(dir, branch, key, states string, noneTTL int64) string {
	cache := filepath.Join(e.cacheDir, key+".json")
	if e.serveCache(cache, noneTTL) || !e.r.lookPath("gh") {
		return cache
	}
	release, ok := acquireLock(filepath.Join(e.cacheDir, key+".lock"))
	if !ok {
		return cache
	}
	defer release()
	fields := prFields
	if e.o.force {
		fields += ",statusCheckRollup"
	}
	fields += ",isCrossRepository"
	var list []byte
	if states == "open+all" {
		var ok bool
		if list, ok = e.listPRs(dir, branch, "open", fields); !ok {
			return cache
		}
	}
	if len(list) == 0 || string(list) == "[]" {
		var ok bool
		if list, ok = e.listPRs(dir, branch, "all", fields); !ok {
			return cache
		}
	}
	e.writeFetched(cache, list)
	return cache
}

func (e *enricher) serveCache(cache string, noneTTL int64) bool {
	var content string
	var age int64
	b, err := os.ReadFile(cache) //nolint:gosec // cache path is built from the poller's own dir and a sha1 key
	exists := err == nil
	if exists {
		content = string(b)
		age = e.now().Unix() - fileMtime(cache)
	}
	return cacheDecision(e.o.force, exists, content, age, ttl, noneTTL, ttlTerminal) == "serve"
}

// listPRs asks gh for isCrossRepository too and drops fork PRs: --head matches
// the branch name across forks, and the limit must exceed the fork PRs that
// could share the name or they would crowd the repo's own PR out of the page.
func (e *enricher) listPRs(dir, branch, state, fields string) ([]byte, bool) {
	out, err := e.r.output(dir, "gh", "pr", "list", "--head", branch, "--state", state, "--limit", "100", "--json", fields)
	if err != nil {
		return nil, false
	}
	return sameRepoPR([]byte(out)), true
}

// writeFetched stores identity and, under --force, the checks cache beside it,
// so the fast identity refresh never has to carry the heavy rollup selection.
func (e *enricher) writeFetched(cache string, list []byte) {
	if !e.o.force {
		_ = writeAtomic(cache, list)
		return
	}
	var prs []map[string]json.RawMessage
	if json.Unmarshal(list, &prs) != nil {
		return
	}
	type checks struct {
		Rollup json.RawMessage `json:"statusCheckRollup"`
		Head   json.RawMessage `json:"headRefOid"`
	}
	ident := make([]map[string]json.RawMessage, len(prs))
	chk := make([]checks, len(prs))
	for i, pr := range prs {
		c := checks{Rollup: pr["statusCheckRollup"], Head: pr["headRefOid"]}
		if len(c.Rollup) == 0 || bytes.Equal(c.Rollup, []byte("null")) {
			c.Rollup = json.RawMessage("[]")
		}
		chk[i] = c
		delete(pr, "statusCheckRollup")
		ident[i] = pr
	}
	if b, err := json.Marshal(ident); err == nil {
		_ = writeAtomic(cache, b)
	}
	if b, err := json.Marshal(chk); err == nil {
		_ = writeAtomic(checksPath(cache), b)
	}
}

func checksPath(cache string) string {
	return cache[:len(cache)-len(".json")] + ".checks.json"
}
