package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
)

// Of the control clients on one remote session, the one whose user was last
// active sizes the windows (#979). Under window-size latest tmux still clamps a
// window by every non-ignored client's per-window cap (resize.c
// clients_calculate_size, the skip: loop), so the smallest controller wins
// unless the others carry ignore-size. The owner stays unflagged and flags the
// other control-mode clients; the record of who owns lives in a session option
// so a daemon that was asleep can read it back.

const (
	// bridgeOwnerOption holds "<token>|<client_name>" on the pinned session.
	bridgeOwnerOption = "@og_bridge_owner"
	ownerSubName      = "og_self"
	ownerSelfFormat   = "#{client_name}|#{client_flags}|#{@og_bridge_owner}"
)

// newOwnerToken mints the per-process token naming this daemon in the owner
// record; it survives the daemon's reconnects, which change only the client name.
func newOwnerToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ownerValueRe is the only shape accepted from the remote's owner option: the
// client name reaches refresh-client -t and the release condition, and this
// charset holds nothing (#, ',', '}', a quote) that could break out of either.
var ownerValueRe = regexp.MustCompile(`^([0-9a-f]{16})\|([A-Za-z0-9_./-]{1,64})$`)

// parseOwnerValue splits an owner record; anything malformed reads as unset.
func parseOwnerValue(v string) (token, client string) {
	m := ownerValueRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// selfReport is one og_self row: what this daemon's own client observes.
type selfReport struct {
	client                  string
	flagged                 bool
	ownerToken, ownerClient string
}

func parseSelfReport(v string) (selfReport, bool) {
	f := strings.SplitN(v, "|", 3)
	if len(f) < 2 || f[0] == "" {
		return selfReport{}, false
	}
	r := selfReport{client: f[0]}
	for flag := range strings.SplitSeq(f[1], ",") {
		if flag == "ignore-size" {
			r.flagged = true
		}
	}
	if len(f) == 3 {
		r.ownerToken, r.ownerClient = parseOwnerValue(f[2])
	}
	return r, true
}

// ignoreSizeCmd flips ignore-size on client, or on the control client itself
// for "". It does not recalculate; follow it with recalcCmd.
func ignoreSizeCmd(client string, on bool) string {
	flag := "ignore-size"
	if !on {
		flag = "!ignore-size"
	}
	if client == "" {
		return "refresh-client -f " + flag
	}
	return "refresh-client -t " + tmuxQuote(client) + " -f " + flag
}

func ownerClaimCmd(sessID, token, client string) string {
	return "set-option -t " + tmuxQuote(sessID) + " " + bridgeOwnerOption + " " + tmuxQuote(token+"|"+client)
}

// ownerReleaseCmd clears the record only while it still reads token|client, so
// concurrent survivors are harmless and a newer claim survives. if-shell
// expands its format against -t; without it the target defaults to the issuing
// client's current session, which for a moved stray is not the pinned one.
func ownerReleaseCmd(sessID, token, client string) string {
	cond := "#{==:#{" + bridgeOwnerOption + "}," + token + "|" + client + "}"
	unset := "set-option -u -t " + tmuxQuote(sessID) + " " + bridgeOwnerOption
	return "if-shell -F -t " + tmuxQuote(sessID) + " " + tmuxQuote(cond) + " " + tmuxQuote(unset)
}

// ownerReadCmd answers "<own client>|<owner record>" in one reply.
func ownerReadCmd(sessID string) string {
	return "display-message -p -t " + tmuxQuote(sessID) + " " + tmuxQuote(ctlSafe("#{client_name}|#{"+bridgeOwnerOption+"}"))
}

func sessionClientsCmd(sessID string) string {
	return "list-clients -t " + tmuxQuote(sessID) + " -F " + tmuxQuote(ctlSafe("#{client_name}|#{client_control_mode}|#{client_flags}"))
}

type sessionClient struct {
	name             string
	control, flagged bool
}

func parseSessionClients(body string) []sessionClient {
	var out []sessionClient
	for line := range strings.SplitSeq(body, "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) < 3 || f[0] == "" {
			continue
		}
		c := sessionClient{name: f[0], control: f[1] == "1"}
		for flag := range strings.SplitSeq(f[2], ",") {
			if flag == "ignore-size" {
				c.flagged = true
			}
		}
		out = append(out, c)
	}
	return out
}

// enforceTargets names the clients the owner must flag: control-mode ones, never
// a human's tty client (its size clamping the window stays as before #979).
func enforceTargets(clients []sessionClient, self string) []string {
	var out []string
	for _, c := range clients {
		if c.control && c.name != self && !c.flagged {
			out = append(out, c.name)
		}
	}
	return out
}

// reconcileCmd compares the observed flag with the observed owner, so a wrong
// flag set by anyone heals at the next report. The third row is the release:
// the owner is gone and a flagged client must count again.
func reconcileCmd(r selfReport, token string) (cmd string, ok bool) {
	switch {
	case r.ownerToken == token && r.flagged:
		return ignoreSizeCmd("", false), true
	case r.ownerToken != "" && r.ownerToken != token && !r.flagged:
		return ignoreSizeCmd("", true), true
	case r.ownerToken == "" && r.flagged:
		return ignoreSizeCmd("", false), true
	}
	return "", false
}

// foreignOwner reports whether the record names a daemon other than this one.
func foreignOwner(ownerToken, myToken string) bool {
	return ownerToken != "" && ownerToken != myToken
}

// activityRow is one local client attached to the mirror session.
type activityRow struct {
	focused bool
	stamp   int64
}

// activityMark detects the user acting on this controller from #{client_activity}
// (whole seconds). A stamp counts only when all of these hold:
//   - focused: the key callback stamps before branching on the key, so a focus-out
//     (a screen lock) advances it too, and CLIENT_FOCUSED is cleared by one;
//   - above every stamp seen earlier, so a poll sees each input once;
//   - recent: a client moved in with nobody at it (detach-on-destroy off after a
//     job killed its session) is set CLIENT_FOCUSED but keeps its old stamp.
//
// The first poll only seeds the mark.
type activityMark struct {
	seeded bool
	high   int64
}

// activityRecency is wider than two 1s poll ticks because stamps print
// truncated to the second and a one-shot switch-in must not miss the window.
const activityRecency = 3

func (m *activityMark) advance(rows []activityRow, now int64) bool {
	counted := false
	high := m.high
	for _, r := range rows {
		if m.seeded && r.focused && r.stamp > m.high && r.stamp >= now-activityRecency {
			counted = true
		}
		high = max(high, r.stamp)
	}
	m.high = high
	m.seeded = true
	return counted
}

// sizeOwner is this daemon's view of the ownership race. Written from the
// reveal watcher (activity), the control reader (reports) and the main loop.
type sizeOwner struct {
	mu            sync.Mutex
	token         string
	self          string
	ownerToken    string
	ownerClient   string
	ownedBefore   bool
	pendingReport *selfReport
	claimWanted   bool
	members       map[string]bool
	mark          activityMark
	// wake is the main loop's nudge; the loop does the claim round-trips.
	wake chan struct{}
}

func newSizeOwner(token string) *sizeOwner {
	return &sizeOwner{token: token, members: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func (o *sizeOwner) isOwner() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ownerToken == o.token
}

// queueReport keeps only the newest report: a daemon waking from suspend replays
// one per change it slept through, and reconciling a stale one (owner unset,
// from before a newer claim) would unflag it with nobody at the keyboard.
func (o *sizeOwner) queueReport(v string) {
	r, ok := parseSelfReport(v)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pendingReport = &r
	o.ownerToken, o.ownerClient = r.ownerToken, r.ownerClient
	if foreignOwner(r.ownerToken, o.token) {
		o.ownedBefore = false
	}
}

func (o *sizeOwner) takeReport() (selfReport, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pendingReport == nil {
		return selfReport{}, false
	}
	r := *o.pendingReport
	o.pendingReport = nil
	return r, true
}

func (o *sizeOwner) noteActivity(rows []activityRow, now int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.mark.advance(rows, now) || o.ownerToken == o.token {
		return
	}
	o.claimWanted = true
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *sizeOwner) takeClaim() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	wanted := o.claimWanted
	o.claimWanted = false
	return wanted
}

// noteClaim records our own claim ahead of the report that will confirm it, so
// activity right after does not claim again.
func (o *sizeOwner) noteClaim(self string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ownerToken, o.ownerClient = o.token, self
	o.ownedBefore = true
}

func (o *sizeOwner) setSelf(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.self = name
}

func (o *sizeOwner) setMembers(names []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.members = make(map[string]bool, len(names))
	for _, n := range names {
		o.members[n] = true
	}
}

func (o *sizeOwner) addMember(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.members[name] = true
}

// dropMember reports whether name was on the session.
func (o *sizeOwner) dropMember(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	was := o.members[name]
	delete(o.members, name)
	return was
}
