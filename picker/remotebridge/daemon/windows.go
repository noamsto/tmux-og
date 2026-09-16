package daemon

import (
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// mirrorWindow is one remote window's local mirror: the remote window id it
// tracks, the local tmux window target it renders into, the remote pane ids in
// creation order, the local pane ids rendering them, and the renderer conns
// keyed by remote pane id.
//
// localPanes is index-parallel to remotePanes and holds only the window's
// *tiled* panes: a float takes an ordinal slot of its own, so the local pane
// rendering remotePanes[i] is not reliably the window's i'th pane. Floats are
// tracked separately, in localFloats and floatGeom, for the same reason: a
// float has no cell in the tiled layout string and no ordinal slot in that
// index-parallel pair, so it cannot live alongside remotePanes/localPanes
// without breaking the invariant every reader of those two relies on.
type mirrorWindow struct {
	remoteID string
	// localWin is a local tmux window ID ("@7"), never "<sess>:<index>":
	// renumber-windows is on, so closing one mirror window renumbers every
	// window above it and an index captured at creation silently starts
	// addressing its neighbour (#411).
	localWin    string
	remotePanes []string
	localPanes  []string
	// localFloats maps a remote float pane id to the local float pane
	// rendering it.
	localFloats map[string]string
	// floatGeom maps a remote float pane id to the geometry last applied to
	// its local mirror — the "have" side of planFloatOps, so a reconcile only
	// ever reasserts what actually changed.
	floatGeom map[string]controlmode.PaneCell
	// floatsDropped records that a rebuild in this reconcile call has discarded
	// the window's mirrored floats, telling a failing exit that it owes them a
	// re-add.
	floatsDropped bool
	layout        string // last tiled layout string applied locally, "" = none yet
	// appliedZoom is the zoom flag last successfully asserted on the mirror
	// window via if -F. Compared against readLayout's remote flag for dedup;
	// zero value is unzoomed.
	appliedZoom bool
	conns       map[string]net.Conn
	// spawned reports whether the last setupWindow reached its spawnRenderer
	// loop — the point after which a kept pane's old renderer is dead
	// (respawn-pane -k) and its old conn must not be merged back on failure.
	// Reset at setupWindow entry; read by resetWindow's failure path.
	spawned bool
}

// allRemotePanes returns every remote pane id this window mirrors — tiled
// panes followed by floats — for call sites that must not miss a float:
// ctl's pane->window registration, teardown/unregister, and the session-pin
// reseed all need every live renderer, tiled or not. planPaneOps,
// localPaneAt, and select-layout must NOT use this — they depend on
// remotePanes/localPanes staying index-parallel and tiled-only.
//
// Float ids are sorted since localFloats is a map and iteration order is not
// deterministic; remotePanes is already in a defined order and is left alone.
// Returns a fresh slice so the caller can't alias and corrupt remotePanes's
// backing array via append.
func (w *mirrorWindow) allRemotePanes() []string {
	out := make([]string, len(w.remotePanes), len(w.remotePanes)+len(w.localFloats))
	copy(out, w.remotePanes)
	floats := make([]string, 0, len(w.localFloats))
	for id := range w.localFloats {
		floats = append(floats, id)
	}
	sort.Strings(floats)
	return append(out, floats...)
}

// registry maps remote window ids (@N) to their local mirror windows.
//
// The main loop mutates it while the resize watcher reads the mirrored ids
// each tick, so every access takes mu.
type registry struct {
	mu       sync.Mutex
	byRemote map[string]*mirrorWindow
	// generation counts changes to the mirror SET, so a reader can tell that the
	// rows it holds may no longer describe it. add counts too, and counts a
	// rebuild under an existing remote id: retireMirror re-adds the same id
	// against a fresh local window, which is a change no remote value reports.
	generation uint64
}

func newRegistry() *registry {
	return &registry{byRemote: map[string]*mirrorWindow{}}
}

func (r *registry) add(remoteID, localWin string) *mirrorWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := &mirrorWindow{
		remoteID:    remoteID,
		localWin:    localWin,
		conns:       map[string]net.Conn{},
		localFloats: map[string]string{},
		floatGeom:   map[string]controlmode.PaneCell{},
	}
	r.byRemote[remoteID] = w
	r.generation++
	return w
}

func (r *registry) byRemoteID(remoteID string) (*mirrorWindow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.byRemote[remoteID]
	return w, ok
}

func (r *registry) remove(remoteID string) (*mirrorWindow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.byRemote[remoteID]
	if ok {
		delete(r.byRemote, remoteID)
		r.generation++
	}
	return w, ok
}

// gen snapshots the generation. Read on the main loop, bumped there too, but
// takes mu like every other accessor: the resize watcher shares this lock.
func (r *registry) gen() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

func (r *registry) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byRemote) == 0
}

// remoteIDs snapshots the mirrored window ids for the resize watcher, which
// runs off the main loop's goroutine.
func (r *registry) remoteIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.byRemote))
	for id := range r.byRemote {
		ids = append(ids, id)
	}
	return ids
}

// all snapshots the mirror windows themselves, for teardown.
func (r *registry) all() []*mirrorWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*mirrorWindow, 0, len(r.byRemote))
	for _, w := range r.byRemote {
		out = append(out, w)
	}
	return out
}

// remoteWindow pairs a remote window's index (#{window_index}) with its id
// (#{window_id}, @N). --window / Config.RemoteWindow is an *index*; the registry
// is keyed by *id* — different tmux namespaces, so both must be carried.
type remoteWindow struct {
	index  string
	id     string
	active bool
	name   string
}

// windowListFormat is the one format every list-windows in the daemon uses.
// window_active sits BEFORE window_name because a name may contain spaces, so
// only the last field can be free-form.
const windowListFormat = "'#{window_index} #{window_id} #{window_active} #{window_name}'"

// parseWindowList turns a list-windows reply body (windowListFormat) into the
// ordered remote windows, dropping blank/malformed rows.
func parseWindowList(body string) []remoteWindow {
	var wins []remoteWindow
	for _, row := range strings.Split(body, "\n") {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		idx, rest, ok := strings.Cut(row, " ")
		if !ok {
			continue
		}
		id, rest, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		// name is optional; "" when absent
		active, name, _ := strings.Cut(rest, " ")
		wins = append(wins, remoteWindow{index: idx, id: id, active: active == "1", name: name})
	}
	return wins
}

// localWinForRemoteIndex resolves the initially-selected window: it maps a
// remote window *index* (as carried by --window) to the remote window *id* via
// the enumerated windows, then to the local window via the registry. This keeps
// --window <idx> from being misread as window id "@<idx>".
func localWinForRemoteIndex(wins []remoteWindow, reg *registry, remoteIdx string) (string, bool) {
	for _, rw := range wins {
		if rw.index == remoteIdx {
			if mw, ok := reg.byRemoteID(rw.id); ok {
				return mw.localWin, true
			}
		}
	}
	return "", false
}

// stripWindowName drops what must never reach a tmux command line — '|' (the
// reflow FMT delimiter), newlines and control chars — and then strips tmux
// #[...] style sequences.
//
// The style sequences are dropped rather than escaped. Escaping them would in
// fact render safely (sanitizeWindowName doubles the '#', and the status line
// collapses "##[" back to a literal "#["), but a remote name that carries style
// markup carries it *as markup* — see the nix-amd-ai fixture, whose #[fg=...]
// runs wrap its glyphs — so keeping it would paint the user's status line with
// raw escape text instead of the name they recognise. Dropping loses nothing a
// reader wanted; leaving it unescaped would let a remote host style our status
// line, which is the one option that is actually unsafe.
//
// Both orderings below are load-bearing:
//   - The drop runs FIRST because it can join a '#' to a '[' that a strip scan
//     never saw together: "a#|[x]b" would otherwise survive as "a##[x]b".
//   - The strip iterates to a fixed point because one pass can likewise join a
//     surviving '#' to a later '[': "##[a][" collapses to "#[".
//
// An unterminated "#[" (no ']' anywhere after it) drops to end-of-string.
func stripWindowName(s string) string {
	var dropped strings.Builder
	for _, r := range s {
		if r == '|' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			continue
		}
		dropped.WriteRune(r)
	}
	cur := dropped.String()
	for {
		var b strings.Builder
		for i := 0; i < len(cur); {
			if i+1 < len(cur) && cur[i] == '#' && cur[i+1] == '[' {
				j := strings.IndexByte(cur[i:], ']')
				if j < 0 {
					i = len(cur)
					continue
				}
				i += j + 1
				continue
			}
			b.WriteByte(cur[i])
			i++
		}
		if b.Len() == len(cur) {
			return cur
		}
		cur = b.String()
	}
}

// sanitizeWindowName cleans a remote-derived window name before it is written
// to @window_bridge_name: stripWindowName, then escape every surviving '#' as
// '##' so a format expansion of the option renders it literally.
func sanitizeWindowName(s string) string {
	var b strings.Builder
	for _, r := range stripWindowName(s) {
		if r == '#' {
			b.WriteString("##")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// decodeWindowName inverts sanitizeWindowName's escape. Its caller applies it to
// whatever the rename prompt returns, edited or not: the prompt is seeded from
// @window_bridge_name, so the whole field speaks that option's escaped dialect and
// nothing marks which parts the user retyped. A typed literal '##' therefore
// collapses to one '#'.
func decodeWindowName(s string) string {
	return strings.ReplaceAll(s, "##", "#")
}
