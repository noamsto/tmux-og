package daemon

import (
	"strings"
	"sync/atomic"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

// viewClientFormat lists the mirror session's own clients for
// resolveViewIdentity (R1/R2): one row per attached client, in
// #{client_control_mode}|#{client_termname}|#{client_termfeatures}|#{I/f:sixel}
// order — the control flag leads, matching scripts/tmux-default-size.sh's own
// #{client_control_mode}|... convention, so the "skip this row" test reads
// the same way in both places. The trailing #{I/f:sixel} is tmux's own
// per-client capability interrogation (R6) — client_termfeatures is still
// carried alongside it only for the raw diagnostic; the kitty-prefix check
// reads client_termname alone.
// '|'-delimited per R12/CLAUDE.md: a tab or newline collapses the row to one
// field for any client without a UTF-8 locale, and none of these four fields
// is free-form enough to carry one anyway.
const viewClientFormat = "#{client_control_mode}|#{client_termname}|#{client_termfeatures}|#{I/f:sixel}"

// ViewIdentity is what resolveViewIdentity resolves the mirror's attached
// clients down to: the termname a control client should advertise, and the
// graphics capability every one of those clients can actually paint (R1).
type ViewIdentity struct {
	Term  string
	Relay graphics.Relay
}

// resolveViewIdentity implements R1/R2 over a viewClientFormat listing. It is
// pure — no exec, no tmux — so the resolve/compare/raise sequence R6 requires
// can run synchronously inside a ctl handler with nothing to fake in a test.
//
// The second return is false when no non-control client remains, including a
// nil, empty, or all-control input: R3 says the caller keeps whatever
// identity it last advertised rather than reading "nobody's looking" as
// "degrade the mirror".
func resolveViewIdentity(lines []string) (ViewIdentity, bool) {
	type client struct {
		term  string
		feats string
		kitty bool
		sixel bool
	}

	var clients []client
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		fields := strings.Split(line, "|")
		if len(fields) != 4 {
			continue // malformed/short row; one bad line must not fail the whole resolve
		}
		control, term, feats, sixelFlag := fields[0], fields[1], fields[2], fields[3]
		if control == "1" {
			// A control-mode client's client_termfeatures is always empty
			// (E1), so counting it here would force the AND'd sixel
			// capability false and could win the lexicographic termname pick
			// — for a "client" that never paints anything itself.
			continue
		}
		clients = append(clients, client{
			term:  term,
			feats: feats,
			kitty: strings.HasPrefix(term, "xterm-kitty") || strings.HasPrefix(term, "xterm-ghostty"),
			sixel: sixelFlag == "1",
		})
	}
	if len(clients) == 0 {
		return ViewIdentity{}, false
	}

	kittyAll, sixelAll := true, true
	for _, c := range clients {
		kittyAll = kittyAll && c.kitty
		sixelAll = sixelAll && c.sixel
	}

	// The termname is the lexicographic minimum over the clients that
	// *witness* the AND'd kitty capability: every client when it holds (each
	// one is evidence the AND is true), otherwise only the non-kitty-capable
	// ones (each is evidence the AND is false) — non-empty by construction,
	// since kittyAll is false only because at least one client failed the
	// prefix test. Lexicographic over the set, never input order, so the
	// identity is a pure function of who is attached.
	term := ""
	for _, c := range clients {
		if !kittyAll && c.kitty {
			continue
		}
		if term == "" || c.term < term {
			term = c.term
		}
	}

	// Mirror the same witness/minimum construction for the Relay's raw
	// diagnostic (R11: Proxy.Filter's drop log interpolates it), rather than
	// concatenating every client's termfeatures — a union of tokens would
	// falsely read as sixel-capable the moment any one client's list carried
	// the token, which is exactly the AND this resolver exists to compute.
	// The AND itself (sixelAll) is computed directly from each client's
	// #{I/f:sixel}-interrogated sixel flag above — raw is carried only for
	// the diagnostic, never re-derived from it.
	feats := ""
	for _, c := range clients {
		if sixelAll && !c.sixel {
			continue
		}
		if !sixelAll && c.sixel {
			continue
		}
		if feats == "" || c.feats < feats {
			feats = c.feats
		}
	}

	return ViewIdentity{Term: term, Relay: graphics.NewRelayFromClient(sixelAll, feats)}, true
}

// ResolveLocalViewIdentity runs viewClientFormat's list-clients query against
// session through out (cfg.LocalTmuxOut in production) and resolves the
// result with resolveViewIdentity. It is the single place that builds this
// query's argv, so the daemon-side watcher (watchLocalClient) and the startup
// seed in cmd/daemon/main.go share one command construction instead of each
// building the same argv on its own.
//
// An exec error, a nil out, or an empty session mean the same as an empty
// client set: (ViewIdentity{}, false) — R3's caller keeps whatever identity it
// last advertised rather than reading a query failure as "nobody's looking".
func ResolveLocalViewIdentity(out func(args ...string) (string, error), session string) (ViewIdentity, bool) {
	if out == nil || session == "" {
		return ViewIdentity{}, false
	}
	raw, err := out("list-clients", "-t", session, "-F", viewClientFormat)
	if err != nil {
		return ViewIdentity{}, false
	}
	return resolveViewIdentity(strings.Split(strings.TrimRight(raw, "\n"), "\n"))
}

// Viewing is the daemon's live view-identity cell, and the canonical
// statement of which value is read where:
// the dial argv reads Desired, the raise guard compares a fresh resolve
// against Advertised, and the two must never move together.
//
//   - Desired is the termname we WANT the next control client to carry.
//     Written by any resolve that returns non-empty.
//   - Advertised is what the CURRENTLY published control client was actually
//     dialled with. Written only where a connection is published, as a
//     snapshot of Desired taken before the dial — never a fresh resolve at
//     publish time, which would widen the window a viewer switch can land in
//     from one gesture to the whole dial-plus-identity-read span.
//   - Relay is the pointer to graphics.RelaySource, the single capability
//     cell — never a copy, so the drop policy and the published value are
//     literally one value rather than two that could drift.
//
// The two term cells are independent atomics: writing one must never move
// the other, and nothing in this daemon reads or writes them together.
type Viewing struct {
	desired    atomic.Pointer[string]
	advertised atomic.Pointer[string]
	Relay      *graphics.RelaySource
}

func (v *Viewing) Desired() string        { return loadTermCell(&v.desired) }
func (v *Viewing) SetDesired(term string) { storeTermCell(&v.desired, term) }

func (v *Viewing) Advertised() string        { return loadTermCell(&v.advertised) }
func (v *Viewing) setAdvertised(term string) { storeTermCell(&v.advertised, term) }

// Seed sets Desired AND Advertised to term. The one place outside a publish
// site allowed to write Advertised, and only because it exists for startup:
// the very first dial this process makes really will carry term, so there is
// no prior client for the two to ever disagree about (cmd/daemon/main.go's
// only caller).
func (v *Viewing) Seed(term string) {
	v.SetDesired(term)
	v.setAdvertised(term)
}

func loadTermCell(c *atomic.Pointer[string]) string {
	if p := c.Load(); p != nil {
		return *p
	}
	return ""
}

func storeTermCell(c *atomic.Pointer[string], term string) {
	c.Store(&term)
}
