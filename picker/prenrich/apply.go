package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

// prValues is one window's @pr_* option set. draft and autoMerge are "1" or empty.
type prValues struct {
	number, title, state, check, url, mergeable, branch string
	draft, review, autoMerge, progress                  string
}

// applyCache writes the cached PR to target and returns the collapsed check
// state it wrote, or "" when nothing was applied. A missing cache means the
// branch was never fetched: keep the last-known options (offline resilience); a
// genuine "no PR" is a present "[]".
func (e *enricher) applyCache(target, cache, branch string) string {
	b, err := os.ReadFile(cache) //nolint:gosec // cache path is built from the poller's own dir and a sha1 key
	if err != nil {
		return ""
	}
	if s := strings.TrimSpace(string(b)); s == "[]" || s == "" {
		e.writePROptions(target, prValues{number: "none", branch: branch})
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var prs []map[string]any
	if dec.Decode(&prs) != nil {
		return ""
	}
	var pr map[string]any
	if len(prs) > 0 {
		pr = prs[0]
	}
	v := prValues{
		number:    jqNumber(pr["number"]),
		title:     "",
		state:     strings.ToLower(jqStr(pr, "state")),
		url:       jqStr(pr, "url"),
		mergeable: strings.ToLower(jqStr(pr, "mergeable")),
		review:    strings.ToLower(jqStr(pr, "reviewDecision")),
		branch:    branch,
	}
	if jqPresent(pr, "isDraft") {
		v.draft = "1"
	}
	if jqPresent(pr, "autoMergeRequest") {
		v.autoMerge = "1"
	}
	v.title = sanitizeTitle(jqStr(pr, "title"))
	v.check, v.progress = collapseRollup(rollupFor(cache, b))
	e.writePROptions(target, v)
	return v.check
}

func jqNumber(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case string:
		return n
	}
	return ""
}

// rollupFor prefers the checks cache; a combined identity entry from before the
// split still carries its own rollup, so an upgrade keeps its check state.
func rollupFor(cache string, identity []byte) string {
	src, key := identity, "statusCheckRollup"
	if b, err := os.ReadFile(checksPath(cache)); err == nil {
		src = b
	}
	var prs []map[string]json.RawMessage
	if json.Unmarshal(src, &prs) != nil || len(prs) == 0 {
		return "[]"
	}
	raw := string(prs[0][key])
	if raw == "" || raw == "null" || raw == "false" {
		return "[]"
	}
	return raw
}

// prGlyph is the @pr_glyph a window's PR options imply. A window with no PR
// (the "none" sentinel or no number at all) wants an empty glyph, not the
// success icon Glyph would give; writePROptions and glyphSync must agree on
// this, or the sync rewrites every no-PR window each pass.
func prGlyph(ic enrichstate.Icons, number, state, check, mergeable, draft, progress string) string {
	if number == "" || number == "none" {
		return ""
	}
	return enrichstate.Glyph(ic, state, check, mergeable, draft, progress)
}

const prevSnapshotFormat = "#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_review}|#{@pr_auto_merge}|#{@pr_check_progress}|#{@pr_glyph}"

// writePROptions stamps the window options. The prior badge-driving values are
// read first so the cache-bypassing reflow only runs when the badge changed.
func (e *enricher) writePROptions(target string, v prValues) {
	prev, _ := e.r.output("", "tmux", "display-message", "-t", target, "-p", prevSnapshotFormat)
	set := func(opt, val string) {
		_ = e.r.run("tmux", "set-option", "-t", target, "-w", opt, val)
	}
	glyph := prGlyph(e.o.icons, v.number, v.state, v.check, v.mergeable, v.draft, v.progress)
	set("@pr_number", v.number)
	set("@pr_title", v.title)
	set("@pr_state", v.state)
	set("@pr_check_state", v.check)
	set("@pr_url", v.url)
	set("@pr_mergeable", v.mergeable)
	set("@pr_draft", v.draft)
	set("@pr_review", v.review)
	set("@pr_auto_merge", v.autoMerge)
	set("@pr_check_progress", v.progress)
	set("@pr_glyph", glyph)
	// Tags the branch this data describes so displays hide it once the pane
	// moves to another branch.
	set("@pr_branch", v.branch)
	e.logEvent("event", "pr", "target", target, "number", v.number, "state", v.state, "check", v.check,
		"mergeable", v.mergeable, "draft", v.draft, "review", v.review, "auto_merge", v.autoMerge, "progress", v.progress)
	cur := strings.Join([]string{v.number, v.state, v.check, v.mergeable, v.draft, v.review, v.autoMerge, v.progress, glyph}, "|")
	if prev != cur && e.o.reflow != "" {
		sess, _ := e.r.output("", "tmux", "display-message", "-t", target, "-p", "#{session_name}")
		e.r.detach(e.o.reflow, sess, "--force")
	}
	e.notifyPRChange(target, prev, v)
}

// notifyPRChange fires one notification per genuinely new event: a flip to
// merged, or a check flip to failure or success. An empty prior field is
// discovery (the first stamp learning what already existed), not an event, and
// merged wins over a same-write check flip so one change never notifies twice.
func (e *enricher) notifyPRChange(target, prev string, v prValues) {
	if e.o.notify == "" || strings.HasPrefix(e.o.notify, "@") {
		return
	}
	f := strings.Split(prev, "|")
	if len(f) < 3 {
		return
	}
	prevState, prevCheck := f[1], f[2]
	var level, head string
	switch {
	case v.state == "merged" && prevState != "" && prevState != "merged":
		level, head = "info", "PR merged"
	case prevCheck != "" && v.check != prevCheck:
		switch v.check {
		case "failure":
			level, head = "error", "checks failed"
		case "success":
			level, head = "info", "checks passed"
		}
	}
	if level == "" {
		return
	}
	e.r.detach(e.o.notify, "emit", "--source", "pr", "--level", level,
		"--window", target, "--title", head, "--body", "#"+v.number+" "+v.title)
}

// logEvent appends to the debug event log through lib-log.sh, only while the
// debug sentinel exists.
func (e *enricher) logEvent(kv ...string) {
	if _, err := os.Stat(e.sentinel); err != nil {
		return
	}
	if e.o.libLog == "" || strings.HasPrefix(e.o.libLog, "@") {
		return
	}
	args := append([]string{"-c", `source "$1"; shift; log_event "$@"`, "_", e.o.libLog, "enrich"}, kv...)
	_ = e.r.run("bash", args...)
}
