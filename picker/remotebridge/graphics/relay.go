package graphics

import (
	"sync/atomic"
)

// Relay is the local terminal's relayable-graphics capability: the one value
// computed once and read by both the local drop policy (R1, Proxy.Filter) and
// the value the daemon publishes to the remote (R5) — so the two can never
// disagree about what may be relayed (R6). The zero value means no relayable
// capability.
type Relay struct {
	sixel bool
	raw   string // the raw client_termfeatures this was derived from (R1 logging)
}

// NewRelayFromClient builds a Relay from a capability already resolved by
// tmux's own #{I/f:sixel} client interrogation (R6) — sixel is whatever tmux
// itself reports for the client, never re-derived by matching
// client_termfeatures tokens in Go. raw is kept only for Proxy.Filter's drop
// diagnostic (R1).
//
// Named NewRelayFromClient rather than NewRelay to avoid colliding with
// proxy.go's existing NewRelay (the *Proxy constructor) in this same package.
func NewRelayFromClient(sixel bool, raw string) Relay {
	return Relay{sixel: sixel, raw: raw}
}

// String renders the cross-repo grammar the daemon publishes to the remote
// session (R5): "sixel" when set, "" when not. What the daemon publishes is
// exactly this string of exactly the value that gates Sixel() below — that
// sentence is R6.
func (r Relay) String() string {
	if r.sixel {
		return "sixel"
	}
	return ""
}

// Sixel reports whether a complete bare sixel may be relayed to the local
// terminal. The predicate Proxy.Filter gates on.
func (r Relay) Sixel() bool { return r.sixel }

// DefaultRasterHold is the byte budget for holding a bare partial sixel meant
// for relay (R4), and the default for the daemon's -gfx-relay-max-bytes flag.
//
// 16 MiB: the largest chafa sixel measured is 7.64 MB (240x60 cells), so this
// is a little over 2x the worst case actually observed, and payload size
// scales with cell count, so a larger terminal needs the headroom.
//
// Deliberately independent of wire.maxFrameSize, which is also 16 MiB:
// wire.WriteStream splits an oversized payload across frames, so a held
// payload never has to fit in one. The two constants sharing a value is a
// coincidence, not a constraint.
const DefaultRasterHold = 16 << 20

// RelaySource is the one cell holding the daemon's current Relay (R4): the
// local-client watcher and the ctl handler that raises a viewer-change both
// write it, every Proxy reads it once per Filter call on its own sink pump,
// and the RelayEnvCmd publish site reads the same cell — so the drop policy
// and the published OG_RELAY_GRAPHICS value can never disagree (R6).
// atomic.Pointer, not a mutex: Proxy documents itself as lock-free and
// confined to its pump goroutine (R11), so a read here must never contend
// with a writer or with another pump's read.
type RelaySource struct {
	v atomic.Pointer[Relay]
}

// NewRelaySource returns a RelaySource seeded with r.
func NewRelaySource(r Relay) *RelaySource {
	s := &RelaySource{}
	s.v.Store(&r)
	return s
}

// Load returns the current Relay. A nil receiver returns the zero Relay
// (relay off) rather than panicking, which makes "no source configured" and
// "source configured with nothing to relay" the same code path for every
// caller — Proxy included, since not every proxy is wired to a live source.
func (s *RelaySource) Load() Relay {
	if s == nil {
		return Relay{}
	}
	if p := s.v.Load(); p != nil {
		return *p
	}
	return Relay{}
}

// Store publishes r as the current Relay.
func (s *RelaySource) Store(r Relay) {
	s.v.Store(&r)
}
