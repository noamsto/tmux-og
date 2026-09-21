package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// agentStatusPollInterval is the floor between two polls while this shipper is
// unsubscribed: rt reads the stream, so only the main loop may poll. An agent
// that changes state redraws its pane first, which is what wakes that loop on a
// busy bridge; mainLoopTickInterval backs it, so a state that changed without a
// redraw is picked up within that ceiling rather than never.
const agentStatusPollInterval = time.Second

// agentStatusBackstopInterval is that floor once %subscription-changed carries
// the state, where a stamp is reported as it is made. See pollFloor.
const agentStatusBackstopInterval = 30 * time.Second

// agentStatusFormat reads each remote pane's foreground command, whatever
// claude-status-update stamped on it, whatever agent-detect's
// statefile.Writer stamped for a screen-scraped agent (pi, codex, cursor —
// #635), and the dispatcher's per-pane role badge. @claude_status is
// "<state> <epoch> <unseen>"; @agent_screen is "<state> <epoch> [name=count
// ...]", a separate option so the two sources stay distinguishable once they
// cross the bridge. Both are drawn from fixed, non-free-form vocabularies, as
// is the crew trio — agent-writable, per GRID_PROTOCOL, and pipe-stripped on
// the REMOTE the way windowLabelFormat's free-form fields are — so they all
// sit ahead of the free-form task, which goes last so a '|' inside it lands
// in the final field instead of shifting the row.
// Unquoted: it is both a -F argument and a subscription format, and only the
// call site knows which quoting each needs.
const agentStatusFormat = "#{pane_id}|#{pane_current_command}|#{@claude_status}|#{@agent_screen}|#{@claude_issues}|" +
	"#{s/[|]/ /:@crew_role}|#{s/[|]/ /:@crew_state}|#{s/[|]/ /:@crew_role_color}|#{@claude_task}"

// agentStatusFields is agentStatusFormat's field count, shared with the test
// fixture so the parser and the fixture cannot drift apart.
const agentStatusFields = 9

const (
	crewRoleMaxRunes  = 24
	crewStateMaxRunes = 16
	// crewColorRe's own bare-word alternative is unbounded; every tmux colour
	// name fits here, as do "colour255" and a hex triplet.
	crewColorMaxRunes = 16
)

// crewWordRe shapes both the role and its state. Stricter than it looks: these
// are the first carried values interpolated into a format the LOCAL tmux
// RENDERS, and cleanLabelValue's contract stops at '#[...]' markup — '#{...}'
// and '#(...)' survive it, so a role of "#(cmd)" on a border would run cmd
// here. The character class settles that outright, rather than an escape pass a
// later consumer could forget to apply.
var crewWordRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// serverPIDRe is the shape the shell reader of a panes/ or screen/ file's
// server= field requires; anything else would be written into the file as-is.
var serverPIDRe = regexp.MustCompile(`^[0-9]+$`)

// paneStatus is one remote pane's foreground command and, when an agent runs
// there, the state either the hook writer or the screen scraper stamped.
type paneStatus struct {
	pane   string // remote pane id, with %
	proc   string // remote foreground command; the local pane only ever runs a renderer
	state  string // "" when no hook-driven agent reported
	ts     int64  // epoch on the REMOTE's clock
	unseen bool
	issues string
	task   string
	// The dispatcher's pane decorations, "" on any pane it never decorated.
	crewRole  string
	crewState string
	crewColor string

	// screenState/screenTS/screenFlags mirror agent-detect's screen/<pane_id>
	// file for a non-Claude agent (#635). screenFlags holds the raw
	// "name=count" tokens verbatim (not a map) so paneStatus stays comparable
	// with == for stamp's unchanged-row check.
	screenState string
	screenTS    int64
	screenFlags string
}

// parseAgentStatus turns an agentStatusFormat reply body into one row per
// remote pane. Rows without an agent are kept — their command still drives the
// mirror window's icons.
func parseAgentStatus(body string) []paneStatus {
	var out []paneStatus
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		// Trailing empty fields may or may not survive the trip, so read them
		// positionally rather than demanding the full width.
		fields := strings.SplitN(line, "|", agentStatusFields)
		at := func(i int) string {
			if i < len(fields) {
				return fields[i]
			}
			return ""
		}
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		row := paneStatus{
			pane:   fields[0],
			proc:   fields[1],
			issues: at(4),
			// Identity fields, dropped whole rather than truncated: a cut role
			// names a different role and a cut colour is not a colour.
			crewRole:  matching(cleanLabelValueExact(at(5), crewRoleMaxRunes), crewWordRe),
			crewState: matching(cleanLabelValueExact(at(6), crewStateMaxRunes), crewWordRe),
			crewColor: matching(cleanLabelValueExact(at(7), crewColorMaxRunes), crewColorRe),
			task:      at(8),
		}
		row.readStatus(at(2))
		row.readScreen(at(3))
		out = append(out, row)
	}
	return out
}

// readStatus parses the "<state> <epoch> <unseen>" triple, leaving state empty
// when the pane carries no usable stamp.
func (r *paneStatus) readStatus(v string) {
	st := strings.Fields(v)
	if len(st) < 2 {
		return
	}
	ts, err := strconv.ParseInt(st[1], 10, 64)
	if err != nil {
		return
	}
	r.state, r.ts = st[0], ts
	r.unseen = len(st) > 2 && st[2] == "1"
}

// readScreen parses the "<state> <epoch> [name=count ...]" form
// statefile.stampValue writes to @agent_screen, leaving screenState empty
// when the pane carries no usable stamp. The flag tokens are kept verbatim
// (already sorted by the writer) rather than parsed into a map, so paneStatus
// stays a comparable struct.
func (r *paneStatus) readScreen(v string) {
	st := strings.Fields(v)
	if len(st) < 2 {
		return
	}
	ts, err := strconv.ParseInt(st[1], 10, 64)
	if err != nil {
		return
	}
	r.screenState, r.screenTS = st[0], ts
	r.screenFlags = strings.Join(st[2:], " ")
}

// agentShipper writes the remote's agent state into the local claude-status
// tree under the LOCAL pane ids, which is what every local consumer — window
// icons, the session tint, the status aggregate, the pickers — reads.
type agentShipper struct {
	dir       string                // claude-status root
	sess      string                // local mirror session, recorded in each pane file
	skew      int64                 // localNow - remoteNow, so a remote stamp lands on our clock
	written   map[string]paneStatus // local pane id (no %) -> the row last written for it
	lastPoll  time.Time
	lastApply time.Time // last time queued rows were applied; bounds the burst wait
	lastGen   uint64    // registry generation the last backstop read was made against

	localPID string // the LOCAL tmux server's own #{pid}, once resolved

	// subscribed is set per connection by Run once the remote has accepted the
	// subscription; false leaves this shipper polling.
	subscribed bool
	// pending holds the rows notifications carried, keyed by remote pane id so a
	// burst collapses to one row per pane, and applied by flush rather than by
	// the dispatch that queued them.
	pending map[string]paneStatus
}

// queue records the row a %subscription-changed line carried. Pure: it is
// called from dispatch, which may itself be running inside a reply reader's
// drain, so it must not read the remote or fork tmux.
func (a *agentShipper) queue(value string) {
	for _, r := range parseAgentStatus(value) {
		a.pending[r.pane] = r
	}
}

// flush stamps whatever the notifications queued, then re-reads the remote if a
// backstop read is due. Main loop only: rt is not safe to share.
//
// Only the backstop half holds the whole remote pane set, so only it may reap —
// see apply.
func (a *agentShipper) flush(cfg Config, rt roundTrip, gen uint64, drained bool) {
	if queuedApplyDue(len(a.pending), drained, a.lastApply) {
		rows := make([]paneStatus, 0, len(a.pending))
		for _, r := range a.pending {
			rows = append(rows, r)
		}
		clear(a.pending)
		a.lastApply = time.Now()
		a.stamp(cfg, rows)
	}
	if !due(a.lastPoll, pollFloor(a.subscribed, agentStatusPollInterval, agentStatusBackstopInterval), gen, a.lastGen) {
		return
	}
	a.lastPoll, a.lastGen = time.Now(), gen
	l, ok := one(rt, fmt.Sprintf("list-panes -s -t %s -F %s", tmuxQuote(cfg.RemoteSession), tmuxQuote(agentStatusFormat)))
	if !ok || l.Kind == controlmode.Error {
		return
	}
	a.apply(cfg, parseAgentStatus(string(l.Data)))
}

func newAgentShipper(localSess string, skew int64) *agentShipper {
	dir := os.Getenv("CLAUDE_STATUS_DIR")
	if dir == "" {
		dir = "/tmp/claude-status"
	}
	return &agentShipper{
		dir:     dir,
		sess:    localSess,
		skew:    skew,
		written: map[string]paneStatus{},
		pending: map[string]paneStatus{},
	}
}

// reskew re-points the shipper at a freshly measured clock offset, keeping the
// files it already owns. Measured once per connection rather than once per
// session: NTP drift over a session is far below the fade's resolution, but a
// reconnect follows an outage of unknown length (#482).
func (a *agentShipper) reskew(skew int64) { a.skew = skew }

// localServerPID returns the LOCAL tmux server's own PID, the `server=`
// ownership stamp that keeps a second server's boot-time prune off a panes/
// file. Cached once resolved (it cannot change while the daemon runs); an
// unusable answer is not cached, so the next stamp pass retries.
func (a *agentShipper) localServerPID(cfg Config) string {
	if a.localPID != "" {
		return a.localPID
	}
	if cfg.LocalTmuxOut == nil {
		return ""
	}
	out, err := cfg.LocalTmuxOut("display-message", "-p", "#{pid}")
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(out)
	if !serverPIDRe.MatchString(pid) {
		return ""
	}
	a.localPID = pid
	return pid
}

// apply stamps rows and then drops the panes that stopped reporting (the agent
// exited, or its pane is gone). Full-set only: a pane absent from rows is taken
// as gone, so this may be called only with the whole remote pane set — which is
// the backstop read alone. A subscription delivers one pane, and reaping against
// it would drop every other pane's state on each notification.
func (a *agentShipper) apply(cfg Config, rows []paneStatus) {
	live, ok := a.stamp(cfg, rows)
	if !ok {
		return
	}
	for id := range a.written {
		if live[id] {
			continue
		}
		a.forget(id)
	}
}

// stamp writes each row that moved and returns the local pane ids it saw, so
// apply can reap against them. Safe with any subset of the remote's panes.
func (a *agentShipper) stamp(cfg Config, rows []paneStatus) (map[string]bool, bool) {
	if cfg.LocalPanes == nil {
		return nil, false
	}
	local := cfg.LocalPanes()
	live := make(map[string]bool, len(rows))

	for _, r := range rows {
		localPane, ok := local[r.pane]
		if !ok {
			continue
		}
		id := strings.TrimPrefix(localPane, "%")
		live[id] = true
		// Act only on a real change: rewriting an unchanged row would undo the
		// local mark-seen hook, which clears `unseen` the moment you look at the
		// mirror window while the remote's stamp still says 1. It also keeps the
		// stamp below from forking tmux once per pane per second.
		prev, seen := a.written[id]
		if seen && prev == r {
			continue
		}
		a.written[id] = r

		// @bridge_proc is the mirror pane's real command: the local one runs a
		// renderer, which would give every mirrored window the fallback icon.
		// Its own call, not folded into the crew sequence below: it is the one
		// carried pane value nothing validates, and tmux fails a whole ';'
		// sequence on one argv it reads as a flag.
		if !seen || prev.proc != r.proc {
			cfg.LocalTmux("set-option", "-p", "-t", localPane, "@bridge_proc", r.proc)
		}
		// Before the agent-less return below: a role pane the dispatcher
		// decorated still draws a border when no agent ever reported on it.
		stampCrew(cfg, localPane, r, prev, seen)
		if r.state == "" {
			// The pane is mirrored but has no hook-driven agent — nothing to
			// render but the icon, and a leftover file would keep one lit.
			a.removeFiles(id)
		} else {
			body := fmt.Sprintf("state=%s\ntimestamp=%d\nsession=%s\n", r.state, r.ts+a.skew, a.sess)
			if pid := a.localServerPID(cfg); pid != "" {
				body += "server=" + pid + "\n"
			}
			if r.unseen {
				body += "unseen=1\n"
			}
			// No transcript= line: it names a path on the remote's disk, and the
			// interrupt detector that reads it would tail a local file that either
			// doesn't exist or belongs to someone else's session.
			writeStatusFile(filepath.Join(a.dir, "panes", id), body)
			writeStatusFile(filepath.Join(a.dir, "tasks", id), lineOrEmpty(r.task))
			writeStatusFile(filepath.Join(a.dir, "issues", id), lineOrEmpty(r.issues))
		}

		// screen/ is independent of panes/ — a non-Claude agent (pi, codex,
		// cursor) has a screen-scraped state but no hook, so the two must not
		// gate each other's file.
		if r.screenState == "" {
			a.removeScreenFile(id)
		} else {
			body := fmt.Sprintf("state=%s\ntimestamp=%d\n", r.screenState, r.screenTS+a.skew)
			if pid := a.localServerPID(cfg); pid != "" {
				body += "server=" + pid + "\n"
			}
			if r.screenFlags != "" {
				// screenFlags is the writer's space-joined "name=count"
				// tokens; the on-disk form is the same tokens, one per line.
				body += strings.ReplaceAll(r.screenFlags, " ", "\n") + "\n"
			}
			writeStatusFile(filepath.Join(a.dir, "screen", id), body)
		}
	}
	return live, true
}

// bridgeCrewOptions maps each carried crew value to the daemon-owned @bridge_*
// option it is stamped into. The daemon never writes @crew_*: those are the
// dispatcher's own names, and a mirror that carried them would have a local
// tmux-og reading a remote pane's role as its own.
var bridgeCrewOptions = []struct {
	opt string
	get func(paneStatus) string
}{
	{"@bridge_crew_role", func(r paneStatus) string { return r.crewRole }},
	{"@bridge_crew_state", func(r paneStatus) string { return r.crewState }},
	{"@bridge_crew_role_color", func(r paneStatus) string { return r.crewColor }},
}

// stampCrew writes the crew values that moved onto one mirror pane, as a single
// argv command sequence so a decorated pane costs one fork rather than three.
func stampCrew(cfg Config, localPane string, r, prev paneStatus, seen bool) {
	var argv []string
	for _, o := range bridgeCrewOptions {
		v := o.get(r)
		if seen && o.get(prev) == v {
			continue
		}
		// A pane first seen carrying nothing has no option to clear, and every
		// undecorated mirror pane is one of those. bridgeLabelOptions diverges
		// here: its first-pass burst of `-u` is what forces the first reflow.
		if !seen && v == "" {
			continue
		}
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		if v == "" {
			argv = append(argv, "set-option", "-p", "-t", localPane, "-u", o.opt)
			continue
		}
		argv = append(argv, "set-option", "-p", "-t", localPane, o.opt, v)
	}
	if len(argv) == 0 {
		return
	}
	cfg.LocalTmux(argv...)
}

// clear drops every file this bridge wrote. The shell-side prune collects by
// server-start mtime, so nothing else would ever reap them.
func (a *agentShipper) clear() {
	for id := range a.written {
		a.forget(id)
	}
}

func (a *agentShipper) forget(id string) {
	a.removeFiles(id)
	a.removeScreenFile(id)
	delete(a.written, id)
}

func (a *agentShipper) removeFiles(id string) {
	for _, sub := range []string{"panes", "tasks", "issues"} {
		os.Remove(filepath.Join(a.dir, sub, id))
	}
}

func (a *agentShipper) removeScreenFile(id string) {
	os.Remove(filepath.Join(a.dir, "screen", id))
}

func lineOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return s + "\n"
}

// writeStatusFile writes body, or removes the file when body is empty — an
// empty tasks/issues file would read back as a blank task rather than none.
func writeStatusFile(path, body string) {
	if body == "" {
		os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	os.WriteFile(path, []byte(body), 0o644)
}

// remoteClockSkew measures localNow - remoteNow so a stamp made on the remote
// lands on our clock. Ages drive the staleness fade and the "last active"
// readout, and two hosts' clocks are never exactly equal. Measured once: NTP
// drift over a session is far below the fade's resolution.
func remoteClockSkew(rt roundTrip) int64 {
	l, ok := one(rt, "display-message -p '%s'")
	if !ok || l.Kind == controlmode.Error {
		return 0
	}
	remote, err := strconv.ParseInt(strings.TrimSpace(string(l.Data)), 10, 64)
	if err != nil {
		return 0
	}
	return time.Now().Unix() - remote
}
