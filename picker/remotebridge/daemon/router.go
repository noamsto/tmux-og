package daemon

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	routerPendingMaxFrames     = 256
	routerPendingMaxBytes      = 1 << 20
	routerPendingMaxTotalBytes = 8 << 20
	routerPendingLifetime      = time.Minute
	routerGoneLifetime         = time.Minute
)

type pendingOutput struct {
	frames [][]byte
	bytes  int
	timer  *time.Timer
}

type gonePane struct {
	timer *time.Timer
}

type Router struct {
	mu                   sync.Mutex
	sinks                map[string]io.Writer
	pending              map[string]*pendingOutput
	pendingBytes         int
	pendingMaxTotalBytes int
	gone                 map[string]*gonePane
	routeDropLogged      map[string]struct{}
	dropped              int
	pendingLifetime      time.Duration
	goneLifetime         time.Duration
}

func NewRouter() *Router {
	return &Router{
		sinks:                map[string]io.Writer{},
		pending:              map[string]*pendingOutput{},
		pendingMaxTotalBytes: routerPendingMaxTotalBytes,
		gone:                 map[string]*gonePane{},
		routeDropLogged:      map[string]struct{}{},
		pendingLifetime:      routerPendingLifetime,
		goneLifetime:         routerGoneLifetime,
	}
}

func (r *Router) Register(paneID string, sink io.Writer) {
	r.mu.Lock()
	if g := r.gone[paneID]; g != nil {
		g.timer.Stop()
		delete(r.gone, paneID)
	}
	r.sinks[paneID] = sink
	p := r.pending[paneID]
	delete(r.pending, paneID)
	dropped := 0
	if p != nil {
		frames := p.frames
		dropped = len(frames)
		r.clearPendingLocked(p)
		if sink != nil {
			for _, data := range frames {
				r.writeLocked(sink, data)
			}
		} else {
			r.dropLocked(dropped)
		}
	}
	r.mu.Unlock()
	if sink == nil && dropped > 0 {
		logDrop(paneID, dropped, "registered without a sink")
	}
}

func (r *Router) Unregister(paneID string) {
	r.mu.Lock()
	sink := r.sinks[paneID]
	delete(r.sinks, paneID)
	dropped := 0
	if p := r.pending[paneID]; p != nil {
		delete(r.pending, paneID)
		dropped = len(p.frames)
		r.clearPendingLocked(p)
		r.dropLocked(dropped)
	}
	if g := r.gone[paneID]; g != nil {
		g.timer.Stop()
	}
	g := &gonePane{}
	r.gone[paneID] = g
	g.timer = time.AfterFunc(r.goneLifetime, func() { r.expireGone(paneID, g) })
	r.mu.Unlock()
	if dropped > 0 {
		logDrop(paneID, dropped, "pane unregistered before registration")
	}
	// Sinks that own a pump goroutine (outputSink) need a Close to stop it;
	// plain io.Writer fakes in tests don't implement it.
	if c, ok := sink.(interface{ Close() }); ok {
		c.Close()
	}
}

// ownedWriter is implemented by *outputSink. Route's data is always a fresh,
// solely-owned []byte from controlmode.Unescape, so a sink that supports it
// skips Write's defensive copy.
type ownedWriter interface {
	writeOwned([]byte)
}

func (r *Router) Route(paneID string, data []byte) {
	r.mu.Lock()
	sink, registered := r.sinks[paneID]
	reason := ""
	if registered {
		if sink == nil {
			reason = r.routeDropLocked("registered without a sink")
		} else {
			r.writeLocked(sink, data)
		}
	} else if _, gone := r.gone[paneID]; gone {
		reason = r.routeDropLocked("pane is gone")
	} else if len(data) > routerPendingMaxBytes {
		reason = r.routeDropLocked("frame exceeds per-pane limit")
	} else {
		p := r.pending[paneID]
		if p == nil {
			if r.pendingBytes+len(data) > r.pendingMaxTotalBytes {
				reason = r.routeDropLocked("total pre-registration buffer full")
			} else {
				p = &pendingOutput{}
				r.pending[paneID] = p
				p.timer = time.AfterFunc(r.pendingLifetime, func() { r.expirePending(paneID, p) })
			}
		}
		if p != nil {
			if len(p.frames) >= routerPendingMaxFrames || p.bytes+len(data) > routerPendingMaxBytes {
				reason = r.routeDropLocked("per-pane buffer full")
			} else if r.pendingBytes+len(data) > r.pendingMaxTotalBytes {
				reason = r.routeDropLocked("total pre-registration buffer full")
			} else {
				owned := append([]byte(nil), data...)
				p.frames = append(p.frames, owned)
				p.bytes += len(owned)
				r.pendingBytes += len(owned)
			}
		}
	}
	r.mu.Unlock()
	if reason != "" {
		logDrop(paneID, 1, reason)
	}
}

func (r *Router) writeLocked(sink io.Writer, data []byte) {
	if ow, ok := sink.(ownedWriter); ok {
		ow.writeOwned(data)
		return
	}
	_, _ = sink.Write(data) // best-effort; sink is non-blocking (see daemon.go), and test fakes only implement io.Writer
}

func (r *Router) expirePending(paneID string, p *pendingOutput) {
	r.mu.Lock()
	if p == nil || r.pending[paneID] != p {
		r.mu.Unlock()
		return
	}
	delete(r.pending, paneID)
	n := len(p.frames)
	r.clearPendingLocked(p)
	r.dropLocked(n)
	r.mu.Unlock()
	if n > 0 {
		logDrop(paneID, n, "pre-registration buffer expired")
	}
}

func (r *Router) dropLocked(n int) {
	r.dropped += n
}

func (r *Router) routeDropLocked(reason string) string {
	r.dropLocked(1)
	if _, logged := r.routeDropLogged[reason]; logged {
		return ""
	}
	r.routeDropLogged[reason] = struct{}{}
	return reason
}

func (r *Router) clearPendingLocked(p *pendingOutput) {
	r.pendingBytes -= p.bytes
	p.frames = nil
	p.bytes = 0
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (r *Router) expireGone(paneID string, g *gonePane) {
	r.mu.Lock()
	if g != nil && r.gone[paneID] == g {
		delete(r.gone, paneID)
		g.timer.Stop()
		g.timer = nil
	}
	r.mu.Unlock()
}

func logDrop(paneID string, n int, reason string) {
	fmt.Fprintf(os.Stderr, "daemon: %s: dropped %d output frame(s) (%s)\n", paneID, n, reason)
}

// sink returns paneID's registered *outputSink, or nil if none is registered
// or the registered writer is a test fake. A read accessor — it leaves
// Register/Unregister/Route unchanged — used by %pause/%continue to gate and
// re-seed the pane's serialized frame stream.
// dirtyPanes returns, sorted, the panes whose sink dropped frames and has since
// drained, clearing each one's count as it goes. A drop is recorded on the
// sink because it happens on the routing path, which must never block; this is
// how it reaches the main loop, the only place the re-seed round-trip may run.
func (r *Router) dirtyPanes() []string {
	r.mu.Lock()
	var ids []string
	type dirtyDrop struct {
		paneID string
		n      int
	}
	var drops []dirtyDrop
	for id, w := range r.sinks {
		s, ok := w.(*outputSink)
		if !ok {
			continue
		}
		if n, dirty := s.takeDirty(); dirty {
			drops = append(drops, dirtyDrop{paneID: id, n: n})
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, drop := range drops {
		fmt.Fprintf(os.Stderr, "daemon: %s: re-seeding after %d dropped frame(s)\n", drop.paneID, drop.n)
	}
	sort.Strings(ids)
	return ids
}

// reshapedPanes returns, sorted, the panes whose confirmation re-seed has come
// due, advancing every other mark one step as it goes. A pane marked on this
// pass is therefore never returned by it — which is the deferral markReshaped
// exists for, expressed as the shape of the walk rather than a timer.
func (r *Router) reshapedPanes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, w := range r.sinks {
		s, ok := w.(*outputSink)
		if !ok {
			continue
		}
		if s.takeReshaped() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (r *Router) sink(paneID string) *outputSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, _ := r.sinks[paneID].(*outputSink)
	return s
}
