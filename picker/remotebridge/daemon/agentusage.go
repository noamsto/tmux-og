package daemon

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// agentUsageFormat carries the remote's open agent panes and its published
// usage caches in one value: "<cmd> <cmd> …|<json>". The S:/W:/P: loop walks
// every pane on the remote server, so the open half is host-wide like the
// caches it gates. It is evaluated on every subscription tick rather than read
// off the remote's poller, which is what makes the gate live: the poller only
// scans past its refresh window, so a closed remote agent would otherwise keep
// rendering for up to two minutes where the local segment hides one within a
// second. Keyed on pane_current_command, never @bridge_proc, exactly as the
// local gate is.
// Unquoted: it is a subscription format, and the call site owns the quoting.
const agentUsageFormat = "#{S:#{W:#{P:#{?#{m/r:(^|/)[.]?(claude|codex|cursor-agent|pi)(-wrapped)?$,#{pane_current_command}},#{pane_current_command} ,}}}}|#{@og_agent_usage}"

// usageRawMaxLen caps the published caches (the JSON half, after the cut).
// Four caches of a few windows each fit in well under half of it. The open
// half is not capped: it grows with the remote host's agent-pane count.
const usageRawMaxLen = 4096

// usageMaxWindows bounds the windows one agent may carry; no provider emits
// more than a handful, and the segment renders every one.
const usageMaxWindows = 8

// usageLabelRe is the whole alphabet a carried label may use. It excludes '#',
// '|', spaces and braces by construction, so a #(…), #{…} or #[…] payload
// cannot survive into the segment's format — the renderer does not re-sanitize.
var usageLabelRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,12}$`)

var usageWrappedRe = regexp.MustCompile(`^\.(.*)-wrapped$`)

// The cache keys in the segment's left-to-right order, and the normalised
// command each one is open for.
var (
	usageAgentKeys = []string{"claude", "codex", "cursor", "pi"}
	usageAgentCmds = map[string]string{"claude": "claude", "codex": "codex", "cursor-agent": "cursor", "pi": "pi"}
)

// The re-typing structs: what survives a decode into these is all that is ever
// carried. json tags match picker/statusline's usageCache, which decodes the
// output. spend.label and spend.period are not rendered and so not carried.
type usageWindow struct {
	Label   string  `json:"label"`
	Pct     float64 `json:"pct"`
	ResetAt int64   `json:"reset_at,omitempty"`
}

type usageSpend struct {
	USD      float64  `json:"usd"`
	LimitUSD *float64 `json:"limit_usd,omitempty"`
}

type usageCache struct {
	Windows []usageWindow `json:"windows"`
	Monthly *usageWindow  `json:"monthly,omitempty"`
	Spend   *usageSpend   `json:"spend,omitempty"`
}

// usageOpenSet normalises the open half exactly as the renderer's openAgents
// does, so the remote and local gates agree on what "open" means.
func usageOpenSet(part string) map[string]bool {
	open := map[string]bool{}
	for _, tok := range strings.Fields(part) {
		base := path.Base(tok)
		if m := usageWrappedRe.FindStringSubmatch(base); m != nil {
			base = m[1]
		}
		if agent, ok := usageAgentCmds[base]; ok {
			open[agent] = true
		}
	}
	return open
}

func validUsageWindow(w usageWindow) bool {
	return usageLabelRe.MatchString(w.Label) && w.Pct >= 0 && w.Pct <= 1000 && w.ResetAt >= 0
}

func validUsageMoney(v float64) bool { return v >= 0 && v <= 1e7 }

// validUsageCache is the identity-field policy: one bad field drops the whole
// agent, since a partial reading of it would render as a different account.
func validUsageCache(c usageCache) bool {
	if len(c.Windows) > usageMaxWindows {
		return false
	}
	for _, w := range c.Windows {
		if !validUsageWindow(w) {
			return false
		}
	}
	if c.Monthly != nil && !validUsageWindow(*c.Monthly) {
		return false
	}
	if s := c.Spend; s != nil && (!validUsageMoney(s.USD) || s.LimitUSD != nil && !validUsageMoney(*s.LimitUSD)) {
		return false
	}
	return true
}

func shiftUsageReset(w *usageWindow, skew int64) {
	if w.ResetAt != 0 {
		w.ResetAt += skew
	}
}

// sanitizeUsage turns one report into the @bridge_usage value: compact JSON of
// only the open, valid agents, or "" for nothing to show. Every failure is ""
// rather than a partial result, and the caller unsets on "": a figure the
// daemon cannot vouch for must not stand in for the remote's.
//
// reset_at is shifted by skew (localNow - remoteNow) so the renderer's
// countdown runs on the local clock.
func sanitizeUsage(v string, skew int64) string {
	openPart, jsonPart, ok := strings.Cut(v, "|")
	if !ok {
		return ""
	}
	if len(jsonPart) > usageRawMaxLen {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(jsonPart), &raw) != nil {
		return ""
	}
	open := usageOpenSet(openPart)
	kept := map[string]usageCache{}
	for _, agent := range usageAgentKeys {
		msg, ok := raw[agent]
		if !ok || !open[agent] {
			continue
		}
		// A plain decode, never DisallowUnknownFields: a provider adding a
		// field must not drop the agent, and the typed struct already keeps the
		// unknown field out of the output.
		var c usageCache
		if json.Unmarshal(msg, &c) != nil || !validUsageCache(c) {
			continue
		}
		for i := range c.Windows {
			shiftUsageReset(&c.Windows[i], skew)
		}
		if c.Monthly != nil {
			shiftUsageReset(c.Monthly, skew)
		}
		kept[agent] = c
	}
	if len(kept) == 0 {
		return ""
	}
	out, err := json.Marshal(kept)
	// Unreachable by construction, and checked anyway because the cost of
	// being wrong is not cosmetic: the statusline reads this out of a
	// '|'-delimited row that fails closed on a wrong field count, and a '#'
	// is the start of every tmux format directive.
	if err != nil || strings.ContainsAny(string(out), "|#") {
		return ""
	}
	return string(out)
}

// usageShipper stamps the remote host's agent usage onto the local mirror
// session as @bridge_usage, so a mirror's usage segment shows the remote's
// accounts instead of this host's.
//
// Stored per mirror session rather than per host: each daemon owns exactly one
// mirror session, so one daemon's teardown cannot delete what another daemon
// of the same host still serves, and the value dies with the session. Like
// resShipper it has no poll mode; repair's re-subscribe is what recovers it.
// Unlike resShipper it takes every session's report: the value is host-wide,
// so a session-pin excursion (#396) reports the same thing.
type usageShipper struct {
	skew int64

	// pending is the raw value the last notification carried, applied by flush
	// rather than by the dispatch that queued it.
	pending     string
	havePending bool

	// written is the last value stamped ("" for an unset); known is whether
	// anything has been stamped since start or reset, so the first report
	// always applies and replaces whatever a previous daemon left.
	written string
	known   bool
}

func newUsageShipper(skew int64) *usageShipper { return &usageShipper{skew: skew} }

// reskew re-points the shipper at a freshly measured clock offset.
func (u *usageShipper) reskew(skew int64) { u.skew = skew }

// queue records a %subscription-changed value. Pure, for the same reason
// resShipper.queue is: dispatch may run inside a reply reader's drain.
func (u *usageShipper) queue(v string) { u.pending, u.havePending = v, true }

// flush stamps whatever the last notification queued. Main loop only; it
// issues no round-trip.
func (u *usageShipper) flush(cfg Config) {
	if !u.havePending {
		return
	}
	v := u.pending
	u.pending, u.havePending = "", false
	if cfg.LocalSess == "" {
		return
	}
	out := sanitizeUsage(v, u.skew)
	if u.known && out == u.written {
		return
	}
	u.written, u.known = out, true
	if out == "" {
		clearBridgeUsage(cfg)
		return
	}
	cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_usage", out)
}

// reset forgets what was written, so the re-report after repair's re-subscribe
// stamps even when identical: reattach dropped the stamp. pending goes with it,
// since a value queued before the drop describes the link as it was.
func (u *usageShipper) reset() {
	u.written, u.known = "", false
	u.pending, u.havePending = "", false
}

// clear drops the stamp while the mirror session still exists — the teardown
// path that matters only when its kill-session fails.
func (u *usageShipper) clear(cfg Config) { clearBridgeUsage(cfg) }
