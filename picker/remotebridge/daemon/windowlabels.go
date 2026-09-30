package daemon

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// windowLabelPollInterval is the floor between two polls while this shipper is
// unsubscribed, and so the whole mechanism's latency then. Main loop only: rt
// reads the stream, which has one consumer.
const windowLabelPollInterval = time.Second

// windowLabelBackstopInterval is that floor once %subscription-changed carries
// the labels: the poll is then a reconciler, not the mechanism. See pollFloor.
const windowLabelBackstopInterval = 30 * time.Second

// mainLoopTickInterval is the main loop's coarse wake-up, and so the ceiling on
// both this poll and the agent-status one. A remote window-option change emits
// no control-stream traffic at all — a @crew_name stamped just after the
// %window-add, a @pr_* refresh, a remote reflow re-stamping @window_label_* —
// so on a quiet mirror the stream alone would never bring the loop back around
// and the label would be stale indefinitely. 5s is therefore the worst-case
// first-appearance latency for a codename; a rename emits %window-renamed and
// stays sub-second under the floor above.
const mainLoopTickInterval = 5 * time.Second

// windowLabelFormat reads each remote window's crew badge, the label segments
// its own reflow already built, its PR state, and the issue/PR identity the
// enrich card needs. Sanitization runs after the split and cannot repair a
// shift, so every free-form field is wrapped '#{s/[|]/ /:…}' and loses its
// pipes on the REMOTE, before the row is assembled — the bracket expression is
// load-bearing, since a bare s/|/ / is an ERE empty alternation.
// @window_label_rest_long is the one free-form field left unwrapped, and must
// stay last: a '|' inside it then lands in itself instead of shifting the row.
// Position 14 resolves worktree||git_root on the remote — one field, one
// authority — so no consumer re-implements that fallback.
// Unquoted: it is both a -F argument and a subscription format, and only the
// call site knows which quoting each needs.
const windowLabelFormat = "#{window_id}|#{@crew_name}|#{@crew_color}|#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@window_pr_plain}|" +
	"#{s/[|]/ /:@issue_provider}|#{s/[|]/ /:@issue_id}|#{s/[|]/ /:@issue_url}|#{s/[|]/ /:@pr_url}|#{s/[|]/ /:@pr_draft}|" +
	"#{s/[|]/ /:@branch}|#{s/[|]/ /:#{?@worktree,#{@worktree},#{@git_root}}}|#{s/[|]/ /:@issue_title}|#{s/[|]/ /:@pr_title}|" +
	"#{s/[|]/ /:@pr_review}|#{s/[|]/ /:@pr_auto_merge}|#{s/[|]/ /:@pr_check_progress}|" +
	"#{@window_label_id}|#{@window_label_rest_long}"

// windowLabelFields is windowLabelFormat's field count, shared with the test
// fixture so the parser and the fixture cannot drift apart.
const windowLabelFields = 22

// labelRow is one remote window's carried label state, already sanitized and
// validated. Comparable, so the unchanged-row check is a struct compare.
type labelRow struct {
	id            string // remote window id, @N
	crewName      string
	crewColor     string
	prNumber      string
	prState       string
	prCheck       string
	prMergeable   string
	prPlain       string
	issueProvider string
	issueID       string
	issueURL      string
	prURL         string
	prDraft       string
	branch        string
	dir           string // the remote's worktree || git_root, already resolved
	issueTitle    string
	prTitle       string
	prReview      string
	prAutoMerge   string
	prProgress    string
	labelID       string
	labelRest     string
}

// bridgeLabelOptions maps each carried value to the daemon-owned @bridge_*
// option it is stamped into. The daemon never writes @crew_*, @window_label_*
// or @pr_*: tmux-reflow-windows stamps those on every window of the mirror
// session, mirrors included, so a same-name write is a two-writer race the
// daemon loses on every reflow pass — and a mirror window carries stale local
// @issue_*/@pr_* residue from the after-new-window hook that fired against the
// launcher's cwd. One ordered list, so the stamp loop, the per-field diff and
// clear cannot drift apart.
var bridgeLabelOptions = []struct {
	opt string
	get func(labelRow) string
}{
	{"@bridge_crew_name", func(r labelRow) string { return r.crewName }},
	{"@bridge_crew_color", func(r labelRow) string { return r.crewColor }},
	{"@bridge_pr_number", func(r labelRow) string { return r.prNumber }},
	{"@bridge_pr_state", func(r labelRow) string { return r.prState }},
	{"@bridge_pr_check_state", func(r labelRow) string { return r.prCheck }},
	{"@bridge_pr_mergeable", func(r labelRow) string { return r.prMergeable }},
	{"@bridge_pr_plain", func(r labelRow) string { return r.prPlain }},
	{"@bridge_issue_provider", func(r labelRow) string { return r.issueProvider }},
	{"@bridge_issue_id", func(r labelRow) string { return r.issueID }},
	{"@bridge_issue_url", func(r labelRow) string { return r.issueURL }},
	{"@bridge_pr_url", func(r labelRow) string { return r.prURL }},
	{"@bridge_pr_draft", func(r labelRow) string { return r.prDraft }},
	{"@bridge_branch", func(r labelRow) string { return r.branch }},
	{"@bridge_dir", func(r labelRow) string { return r.dir }},
	{"@bridge_issue_title", func(r labelRow) string { return r.issueTitle }},
	{"@bridge_pr_title", func(r labelRow) string { return r.prTitle }},
	{"@bridge_pr_review", func(r labelRow) string { return r.prReview }},
	{"@bridge_pr_auto_merge", func(r labelRow) string { return r.prAutoMerge }},
	{"@bridge_pr_check_progress", func(r labelRow) string { return r.prProgress }},
	{"@bridge_label_id", func(r labelRow) string { return r.labelID }},
	{"@bridge_label_rest_long", func(r labelRow) string { return r.labelRest }},
}

const (
	crewNameMaxRunes  = 24 // a codename; a cap so one value cannot dominate a column
	labelTextMaxRunes = 120
	// A 256-char title (GitHub's limit) plus the label rest's leading space.
	labelTitleMaxRunes = 257

	// The exact-cleaned identity caps are sized to their own domain rather than
	// sharing labelTextMaxRunes: a Linear issue URL routinely passes 120, 255 is
	// git's refname limit and 4096 is PATH_MAX. A value over its cap is dropped,
	// so a cap that is too small is a field that silently never arrives.
	providerMaxRunes = 16
	issueIDMaxRunes  = 64
	urlMaxRunes      = 512
	prDraftMaxRunes  = 1
	branchMaxRunes   = 255
	dirMaxRunes      = 4096

	reviewMaxRunes   = 17 // len("changes_requested")
	progressMaxRunes = 16
)

var (
	lowerWordRe = regexp.MustCompile(`^[a-z]+$`)
	digitsRe    = regexp.MustCompile(`^[0-9]+$`)
	// crewColorRe is interpolated into #[fg=…] by two format strings and into
	// ANSI by the picker, so it must never be markup. Hex is matched
	// case-insensitively: ansiFg accepts either case, and a lowercase-only
	// regex would silently drop an uppercase colour to the mauve fallback.
	crewColorRe = regexp.MustCompile(`^(#[0-9A-Fa-f]{6}|colour([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])|[a-z]+)$`)

	issueIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	urlRe     = regexp.MustCompile(`^https?://\S+$`)
	prDraftRe = regexp.MustCompile(`^1$`)
	branchRe  = regexp.MustCompile(`^\S+$`)
	// dirRe rejects a path containing a space, which is a legal worktree path:
	// the value crosses a '|'-delimited row and, for the card, a --dir argument,
	// and an absent dir line is correct-but-incomplete where a shifted row is
	// neither. Whitespace is what keeps both safe, so it stays excluded.
	dirRe = regexp.MustCompile(`^/\S*$`)

	reviewRe   = regexp.MustCompile(`^(approved|changes_requested|review_required)$`)
	progressRe = regexp.MustCompile(`^[0-9]+/[0-9]+$`)
)

// parseWindowLabels turns a windowLabelFormat reply body into one sanitized row
// per remote window, so nothing unclean reaches a caller.
func parseWindowLabels(body string) []labelRow {
	var out []labelRow
	for line := range strings.SplitSeq(body, "\n") {
		// Only the CR comes off: @window_pr_plain carries a LEADING space by
		// construction (" <glyph> #<n>") and reflow's pr_colw padding assumes
		// it, so no TrimSpace, per row or per field.
		line = strings.TrimRight(line, "\r")
		// Trailing empty fields may or may not survive the trip, so read them
		// positionally rather than demanding the full width.
		fields := strings.SplitN(line, "|", windowLabelFields)
		at := func(i int) string {
			if i < len(fields) {
				return fields[i]
			}
			return ""
		}
		if at(0) == "" {
			continue
		}
		out = append(out, labelRow{
			id:          at(0),
			crewName:    cleanLabelValue(at(1), crewNameMaxRunes),
			crewColor:   matching(cleanLabelValue(at(2), labelTextMaxRunes), crewColorRe),
			prNumber:    matching(cleanLabelValue(at(3), labelTextMaxRunes), digitsRe),
			prState:     matching(cleanLabelValue(at(4), labelTextMaxRunes), lowerWordRe),
			prCheck:     matching(cleanLabelValue(at(5), labelTextMaxRunes), lowerWordRe),
			prMergeable: matching(cleanLabelValue(at(6), labelTextMaxRunes), lowerWordRe),
			prPlain:     cleanLabelValue(at(7), labelTextMaxRunes),
			// Identity fields drop rather than truncate; the two titles are
			// display text and truncate. See cleanLabelValueExact.
			issueProvider: matching(cleanLabelValueExact(at(8), providerMaxRunes), lowerWordRe),
			issueID:       matching(cleanLabelValueExact(at(9), issueIDMaxRunes), issueIDRe),
			issueURL:      matching(cleanLabelValueExact(at(10), urlMaxRunes), urlRe),
			prURL:         matching(cleanLabelValueExact(at(11), urlMaxRunes), urlRe),
			prDraft:       matching(cleanLabelValueExact(at(12), prDraftMaxRunes), prDraftRe),
			branch:        matching(cleanLabelValueExact(at(13), branchMaxRunes), branchRe),
			dir:           matching(cleanLabelValueExact(at(14), dirMaxRunes), dirRe),
			issueTitle:    cleanLabelValue(at(15), labelTitleMaxRunes),
			prTitle:       cleanLabelValue(at(16), labelTitleMaxRunes),
			prReview:      matching(cleanLabelValueExact(at(17), reviewMaxRunes), reviewRe),
			prAutoMerge:   matching(cleanLabelValueExact(at(18), prDraftMaxRunes), prDraftRe),
			prProgress:    matching(cleanLabelValueExact(at(19), progressMaxRunes), progressRe),
			labelID:       cleanLabelValue(at(20), labelTextMaxRunes),
			labelRest:     cleanLabelValue(at(21), labelTitleMaxRunes),
		})
	}
	return out
}

// cleanLabelValue drops framing bytes and #[...] markup (stripWindowName, whose
// '#'-doubling twin is deliberately not used — the local label options already
// store raw '#', so a bridge copy must too), then caps the length.
//
// Two values are dropped whole rather than cleaned, because cfg.LocalTmux execs
// without a shell and tmux's own parser sees the argv: one beginning with '-',
// which args_parse reads as a flag, and one that is exactly ";", the separator
// joining apply's per-window command sequence — tmux fails the whole batch on it
// ("empty value", exit 1) and drops every later option in that sequence. A ';'
// inside a value is not a separator and is kept.
//
// NOT format-safe, and no caller may assume it is: stripWindowName removes
// '#[...]' markup but leaves '#{...}' and '#(...)' intact. Every consumer of
// the values this cleaner produces reads them as plain text — the enrich card
// via `show-options -w` stdout, then Go string rendering — so a remote-supplied
// title carrying '#(cmd)' can only garble a display today. Interpolate one into
// a rendered tmux format and that becomes command execution on the LOCAL host,
// off a remote-controlled value: a genuine remote-to-local crossing, not the
// garbling this currently is. A future consumer that needs a format-safe value
// must double every '#' itself (the @window_bridge_name dialect) rather than
// assume this did it.
func cleanLabelValue(v string, maxRunes int) string {
	v = stripWindowName(v)
	if strings.HasPrefix(v, "-") || v == ";" {
		return ""
	}
	r := []rune(v)
	if len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return v
}

// cleanLabelValueExact rejects where cleanLabelValue repairs: it shares the
// whole-value drops for a leading '-' and a bare ';', but returns "" for a value
// over the cap, and for one stripWindowName would have had to alter at all.
//
// Truncation is right for display text and wrong for an identity: a cut URL
// opens the wrong page, a cut branch refreshes the wrong branch on the remote,
// and a cut path names a directory that is not the one on screen. Every consumer
// already renders an absent value correctly, so a silent wrong value is worse.
//
// stripWindowName DELETES rather than rejects, which is the same failure by a
// quieter route: "https://host/a#[b]c" would come back as "https://host/ac",
// which still satisfies urlRe and still opens the wrong page. Hence a
// before/after compare rather than a '#[' test — it costs the same and cannot be
// outflanked by whatever stripWindowName learns to strip next. A '|' is already
// handled a layer earlier by the remote's #{s/[|]/ /:…}, which turns it into a
// space that fails every validator here.
func cleanLabelValueExact(v string, maxRunes int) string {
	if stripWindowName(v) != v {
		return ""
	}
	if strings.HasPrefix(v, "-") || v == ";" {
		return ""
	}
	if len([]rune(v)) > maxRunes {
		return ""
	}
	return v
}

// matching keeps v only if it is shaped as expected; a failure is an empty
// value, which unsets the option rather than stamping something a format string
// would have to survive.
func matching(v string, re *regexp.Regexp) string {
	if !re.MatchString(v) {
		return ""
	}
	return v
}

// labelShipper stamps the remote's window labels onto the local mirror windows
// under @bridge_* names, the way agentShipper does the remote's pane state —
// one level up, window options rather than pane options.
type labelShipper struct {
	written   map[string]writtenLabels // remote window id -> what was last written for it
	lastPoll  time.Time
	lastApply time.Time // last time queued rows were applied; bounds the burst wait
	lastGen   uint64    // registry generation the last backstop read was made against

	// subscribed is set per connection by Run once the remote has accepted the
	// subscription; false leaves this shipper polling.
	subscribed bool
	// pending holds the rows notifications carried, keyed by remote window id so
	// a burst collapses to one row per window, and applied by flush rather than
	// by the dispatch that queued them.
	pending map[string]labelRow
}

// writtenLabels is one remote window's last stamp, and the local window it
// landed on. The local target is part of the key, not just the payload:
// retireMirror rebuilds a dead mirror through closeWindow + reconcileWindows,
// which re-adds the SAME remote id against a fresh local window, so a row
// compare alone would suppress the re-stamp and leave the replacement bare.
// agentShipper needs no equivalent because it keys on the local pane id, which
// a rebuild changes.
type writtenLabels struct {
	localWin string
	row      labelRow
}

func newLabelShipper() *labelShipper {
	return &labelShipper{
		written: map[string]writtenLabels{},
		pending: map[string]labelRow{},
	}
}

// queue records the row a %subscription-changed line carried. Pure: it is
// called from dispatch, which may itself be running inside a reply reader's
// drain, so it must not read the remote or fork tmux.
func (s *labelShipper) queue(value string) {
	for _, r := range parseWindowLabels(value) {
		s.pending[r.id] = r
	}
}

// flush applies whatever the notifications queued, then re-reads the remote if a
// backstop read is due. Main loop only: rt is not safe to share.
//
// Both halves feed the same apply, and the reflow is decided once for the pass
// so a snapshot that moves twenty windows still costs one — a label change
// alters no window count, so reflow's count:width:height cache would skip it
// (the @window_bridge_name precedent), but it is one forced reflow either way.
func (s *labelShipper) flush(cfg Config, reg *registry, rt roundTrip, gen uint64, drained bool) {
	changed := false
	if queuedApplyDue(len(s.pending), drained, s.lastApply) {
		rows := make([]labelRow, 0, len(s.pending))
		for _, r := range s.pending {
			rows = append(rows, r)
		}
		clear(s.pending)
		s.lastApply = time.Now()
		changed = s.apply(cfg, reg, rows)
	}
	if due(s.lastPoll, pollFloor(s.subscribed, windowLabelPollInterval, windowLabelBackstopInterval), gen, s.lastGen) {
		s.lastPoll, s.lastGen = time.Now(), gen
		if l, ok := one(rt, fmt.Sprintf("list-windows -t %s -F %s", tmuxQuote(cfg.RemoteSession), tmuxQuote(windowLabelFormat))); ok && l.Kind != controlmode.Error {
			changed = s.apply(cfg, reg, parseWindowLabels(string(l.Data))) || changed
		}
	}
	if changed {
		cfg.reflow()
	}
}

// apply stamps the rows whose values moved, and reports whether any did.
//
// A bare mirror's FIRST pass counts as changed: seen is false, so the row
// compare cannot fire and the window gets twenty-one `-u` for a row carrying
// nothing, forcing one reflow at daemon start.
func (s *labelShipper) apply(cfg Config, reg *registry, rows []labelRow) (changed bool) {
	for _, r := range rows {
		mw, ok := reg.byRemoteID(r.id)
		if !ok {
			continue
		}
		prev, seen := s.written[r.id]
		seen = seen && prev.localWin == mw.localWin
		if seen && prev.row == r {
			continue
		}
		s.written[r.id] = writtenLabels{localWin: mw.localWin, row: r}

		// One argv command sequence per window — the form
		// tmux-reflow-windows already uses — so a first pass over N windows
		// costs N forks rather than 9N.
		var argv []string
		for _, o := range bridgeLabelOptions {
			v := o.get(r)
			if seen && o.get(prev.row) == v {
				continue
			}
			if len(argv) > 0 {
				argv = append(argv, ";")
			}
			if v == "" {
				// An empty remote value unsets, so a mirror whose remote
				// carries nothing is option-free rather than holding "".
				argv = append(argv, "set-option", "-w", "-t", mw.localWin, "-u", o.opt)
				continue
			}
			argv = append(argv, "set-option", "-w", "-t", mw.localWin, o.opt, v)
		}
		_ = cfg.LocalTmux(argv...)
		changed = true
	}

	// Forget by absence from the REGISTRY, not from the reply as
	// agentShipper.apply does: a remote window that vanishes from list-windows
	// while its mirror is still registered keeps its last stamp until
	// reconcileWindows closes it.
	for id := range s.written {
		if _, ok := reg.byRemoteID(id); !ok {
			delete(s.written, id)
		}
	}
	return changed
}

// clear unsets every option this bridge wrote, while the mirror windows still
// exist. Near-vacuous in production — teardown ends in kill-session and window
// options die with the session — so unlike agentShipper.clear, whose files
// outlive tmux, this is kept for symmetry and for the paths where that kill
// fails.
func (s *labelShipper) clear(cfg Config, reg *registry) {
	for id := range s.written {
		mw, ok := reg.byRemoteID(id)
		if !ok {
			continue
		}
		var argv []string
		for _, o := range bridgeLabelOptions {
			if len(argv) > 0 {
				argv = append(argv, ";")
			}
			argv = append(argv, "set-option", "-w", "-t", mw.localWin, "-u", o.opt)
		}
		_ = cfg.LocalTmux(argv...)
	}
	s.written = map[string]writtenLabels{}
}
