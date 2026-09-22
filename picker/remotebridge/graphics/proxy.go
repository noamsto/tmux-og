package graphics

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
)

// fetchTimeout bounds how long one output batch may hold its pane's byte
// stream. A frozen pane is worse than a missing image (spec D4), so a timeout
// drops the batch's unlocalised stores down the same path as any other
// unlocalisable one and the stream resumes. The budget is per BATCH, not per
// sequence: a carousel re-transmit stores the preview plus every filmstrip
// thumbnail in one batch, and a serialized per-sequence budget held the pane
// for N×timeout on a slow link (#556).
const fetchTimeout = 2 * time.Second

// retainMaxIDs caps how many distinct kitty image ids one pane's proxy keeps
// for post-reseed replay. Each id holds only its newest localised store. Set
// high enough for the aeye carousel: one preview id plus one id per visible
// filmstrip thumbnail — an LRU cap any tighter would evict the preview first.
const retainMaxIDs = 32

// Proxy filters one pane's output stream. It is owned by that pane's output
// sink and called only from the sink's pump goroutine — Filter on every
// output batch, Replay immediately before each FrameSeed, Close on teardown —
// so retain needs no locking. That confinement outlives Close: the pump may
// needs to inspect retain state from outside the pump — a test, typically —
// must wait for the pump to actually exit (outputSink.Wait) rather than
// racing that flush. Filter may block there, bounded by timeout: holding one
// pane's stream at a sequence boundary is what keeps a store ahead of the
// placements that reference it (spec D4).
//
// The capability now follows the viewer (R4), which adds one shared read to
// that confinement: src is an atomic cell other goroutines write, and Filter
// loads it once per call, at the top, before Feed. rel is the pump-local
// cache of that load, and every field derived from the capability — rel
// itself and the scanner's raster hold — is written ONLY there, never from
// outside the pump. A mid-hold flip is safe in both directions: scan.go's
// SetRasterHold doc says the budget "decides when a partial raster is given
// up on, never what happens to it", and holdLimit re-reads it on every Feed,
// so narrowing it under a held partial sixel just makes the existing
// overflow-discard path (already exercised at every value of n) fire sooner
// — it cannot corrupt a sequence in flight.
type Proxy struct {
	sc        *Scanner
	loc       Localizer
	logf      func(format string, args ...any)
	timeout   time.Duration
	retainCap int
	// retain holds the last localised wrapped store per image id for replay
	// after a mirror re-seed restores placeholders without the store APC.
	retain map[string][]byte
	order  []string // oldest-to-newest ids; drives Replay order and LRU eviction

	src  *RelaySource // nil for a Proxy built via New; live capability for NewRelay (R4)
	rel  Relay        // pump-local cache of src's last load; synced at the top of Filter
	hold int64        // raster-hold budget applied while rel.Sixel() is true
	// relayOnly is set only by NewRelay when it is handed no Localizer (R7).
	// A full proxy placed on a same-machine transport would change four
	// kitty behaviours no identity Localizer neutralises: t=s would start
	// being dropped though shared memory is genuinely reachable there; t=t
	// would be rewritten to t=f, leaking the sender's own temp file; a bare
	// APC would gain a \ePtmux; wrapper it never had; and Coalesce would
	// discard stores that reach the terminal today. relayOnly mode instead
	// applies the raster policy (below) and nothing else: kitty sequences
	// forward byte-identically from Chunk.Raw and Coalesce does not run.
	relayOnly bool
	// loggedRelayOff latches the "no sixel capability" drop log to once per
	// pane — a viewer repaints at frame rate, so a per-sequence line at
	// multi-MB rates is spam.
	loggedRelayOff bool
}

func New(loc Localizer, logf func(format string, args ...any)) *Proxy {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Proxy{
		sc:        NewScanner(),
		loc:       loc,
		logf:      logf,
		timeout:   fetchTimeout,
		retainCap: retainMaxIDs,
		retain:    make(map[string][]byte),
	}
}

// NewRelay creates a Proxy with the raster relay policy enabled, wired to src
// — the one cell the local-client watcher and the ctl handler both write as
// the viewer changes (R4). hold is the byte budget for holding a bare partial
// sixel while src reports a sixel-capable viewer (R4), handed to the scanner
// via SetRasterHold; Filter applies and withdraws it live as src's value
// moves, so the scanner keeps its 64 KiB non-relay default whenever nothing
// is watching for sixel.
//
// loc == nil means relay-only mode (R7); see the relayOnly field doc.
func NewRelay(loc Localizer, logf func(format string, args ...any), src *RelaySource, hold int64) *Proxy {
	p := New(loc, logf)
	p.src = src
	p.hold = hold
	p.relayOnly = loc == nil
	return p
}

// Filter returns the bytes to forward to the renderer. An incomplete trailing
// sequence is held until the next call.
func (p *Proxy) Filter(data []byte) []byte {
	// Load the live capability before Feed sees any bytes (R4/R11): a viewer
	// switch must gate the very batch it lands in, not the next one. Only a
	// change to Sixel() touches the scanner — narrowing or widening the hold
	// on every call this cheaply would be pointless work on the hot path, and
	// scan.go's holdLimit re-reads rasterHold on every Feed regardless of when
	// it last changed.
	cur := p.src.Load()
	if cur.Sixel() != p.rel.Sixel() {
		if cur.Sixel() {
			p.sc.SetRasterHold(int(p.hold))
		} else {
			p.sc.SetRasterHold(0) // restores the scanner's non-relay default (maxPartial)
		}
	}
	p.rel = cur

	beforeMalformed := p.sc.Malformed
	beforeInline := p.sc.InlineImage
	chunks := p.sc.Feed(data)
	if !p.relayOnly {
		// Relay-only mode never coalesces (R7): a same-machine transport
		// reaches the terminal directly, so dropping a frame Coalesce judges
		// superseded would discard a store that arrives today.
		chunks = Coalesce(chunks)
	}
	if n := p.sc.Malformed - beforeMalformed; n > 0 {
		// Never reaches the per-sequence log below, because a scanner drop
		// yields no chunk at all: this is the scanner refusing to forward a
		// kitty sequence it could not decode whole. No legitimate sender emits
		// one, so it is worth a line.
		p.logf("graphics: dropped %d undecodable kitty sequence(s)", n)
	}
	if n := p.sc.InlineImage - beforeInline; n > 0 {
		// Same reasoning as Malformed above: an OSC 1337 File= drop (R3)
		// yields no chunk either, so this is the only place it can be seen.
		p.logf("graphics: dropped %d inline image sequence(s) (OSC 1337 File=)", n)
	}

	var outcomes map[string]fetchOutcome
	if !p.relayOnly {
		// One deadline for the whole batch's fetches. D4's guarantee is
		// unchanged — a store still never trails the placements referencing
		// it — but the batch as a whole is what the timeout bounds, and the
		// fetches run concurrently when the localizer supports it.
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
		outcomes = p.fetchBatch(ctx, chunks)
		cancel()
	}
	var out []byte
	for _, c := range chunks {
		switch {
		case c.Raster != nil:
			// A sixel is cursor-positioned, so it is forwarded bare or
			// dropped — never EncodeWrapped, never retained for Replay: a
			// replay after a reseed would paint it at the wrong place.
			if p.rel.Sixel() {
				out = append(out, c.Raster...)
			} else if !p.loggedRelayOff {
				p.loggedRelayOff = true
				p.logf("graphics: dropping sixel(s) — client_termfeatures=%q has no sixel terminal-feature", p.rel.raw)
			}
			continue
		case c.Seq == nil:
			out = append(out, c.Literal...)
			continue
		case p.relayOnly:
			// Relay-only (R7): forward a kitty sequence byte-identically
			// from its verbatim input bytes — bare or \ePtmux;-wrapped, as
			// received. No Rewrite/EncodeWrapped/retain, so t=s and t=t
			// cross unchanged instead of being dropped or rewritten. The one
			// exception to "byte-identical": an APC the scanner classified
			// dropMalformed is consumed upstream and never reaches here as a
			// chunk at all, so relay-only mode does not forward it either —
			// that is the #319-class protection, not a gap in this guarantee.
			out = append(out, c.Raw...)
			continue
		}
		q, drop, err := rewrite(c.Seq, func(remote string) (string, error) {
			oc, ok := outcomes[remote]
			if !ok {
				// By construction unreachable: fetchBatch covers every
				// decodable t=f/t=t path in the batch, and rewrite only calls
				// this for those.
				return "", fmt.Errorf("graphics: no batch outcome for %s", remote)
			}
			return oc.local, oc.err
		})
		if drop {
			if err != nil {
				p.logf("graphics: dropped i=%s: %v", c.Seq.Get("i"), err)
			} else {
				p.logf("graphics: dropped i=%s (t=%s cannot cross hosts)", c.Seq.Get("i"), c.Seq.Get("t"))
			}
			continue
		}
		if q.Get("a") == "d" {
			if id := q.Get("i"); id != "" {
				p.evict(id)
			} else {
				switch q.Get("d") {
				case "A", "a":
					// Bulk delete: clear every retained id so a later re-seed
					// cannot resurrect images the sender already killed.
					p.retain = make(map[string][]byte)
					p.order = nil
				}
			}
		}
		wrapped := q.EncodeWrapped()
		if isStore(q) {
			p.retainStore(q.Get("i"), wrapped)
		}
		out = append(out, wrapped...)
	}
	return out
}

// fetchOutcome is one path's result in a batch fetch.
type fetchOutcome struct {
	local string
	err   error
}

// fetchBatch localises every distinct t=f/t=t payload path in the batch in one
// go — concurrently when the localizer implements BatchLocalizer (the
// production SSHFetcher does), sequentially under the same shared deadline
// otherwise. Sequences whose payload is not base64 are skipped here; rewrite's
// own policy drops them below, which is also what keeps the outcome map
// complete for every path the rewrite loop can ask about.
func (p *Proxy) fetchBatch(ctx context.Context, chunks []Chunk) map[string]fetchOutcome {
	var paths []string
	seen := map[string]struct{}{}
	for _, c := range chunks {
		q := c.Seq
		if q == nil {
			continue
		}
		if t := q.Get("t"); t != "f" && t != "t" {
			continue
		}
		remote, err := base64.StdEncoding.DecodeString(string(q.Payload))
		if err != nil {
			continue
		}
		if _, dup := seen[string(remote)]; !dup {
			seen[string(remote)] = struct{}{}
			paths = append(paths, string(remote))
		}
	}
	if len(paths) == 0 {
		return nil
	}
	outcomes := make(map[string]fetchOutcome, len(paths))
	if bl, ok := p.loc.(BatchLocalizer); ok {
		locals, errs := bl.LocalizeBatch(ctx, paths)
		for i, path := range paths {
			outcomes[path] = fetchOutcome{locals[i], errs[i]}
		}
		return outcomes
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			outcomes[path] = fetchOutcome{err: err}
			continue
		}
		local, err := p.loc.Localize(ctx, path)
		outcomes[path] = fetchOutcome{local, err}
	}
	return outcomes
}

// Replay returns the retained localised stores in oldest-to-newest id order,
// ready to write immediately before a FrameSeed without another fetch or
// round-trip. It must precede the seed: a placeholder (U=1 virtual) store
// only resolves its image once the cell carrying it is painted, so the store
// needs to already be in the terminal's cache when the seed repaints.
func (p *Proxy) Replay() []byte {
	var out []byte
	for _, id := range p.order {
		if b, ok := p.retain[id]; ok {
			out = append(out, b...)
		}
	}
	return out
}

// Retained reports whether any store is currently retained for replay.
// Pump-confined like Replay.
func (p *Proxy) Retained() bool {
	return len(p.order) > 0
}

func (p *Proxy) retainStore(id string, wrapped []byte) {
	if id == "" {
		return
	}
	if _, ok := p.retain[id]; ok {
		p.removeFromOrder(id)
	} else if len(p.order) >= p.retainCap {
		p.evict(p.order[0])
	}
	p.retain[id] = append([]byte(nil), wrapped...)
	p.order = append(p.order, id)
}

func (p *Proxy) evict(id string) {
	if id == "" {
		return
	}
	delete(p.retain, id)
	p.removeFromOrder(id)
}

func (p *Proxy) removeFromOrder(id string) {
	for i, v := range p.order {
		if v == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			return
		}
	}
}

// Close flushes any held partial sequence so it isn't swallowed when the pane
// goes away, and drops retained replay state with the pane.
func (p *Proxy) Close() []byte {
	var out []byte
	for _, c := range p.sc.Flush() {
		out = append(out, c.Literal...)
	}
	p.retain = make(map[string][]byte)
	p.order = nil
	return out
}
