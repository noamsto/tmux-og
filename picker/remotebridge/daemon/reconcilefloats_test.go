package daemon

import (
	"fmt"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// newPaneReply is one canned answer to the Add path's create, which is the only
// way it learns the local float's pane id.
type newPaneReply struct {
	id  string
	err error
}

// floatTmux fakes the local tmux seam and records every argv, so a test can
// assert on the sequence reconcileFloats issued rather than on its effects.
type floatTmux struct {
	mu      sync.Mutex
	argv    [][]string
	newPane []newPaneReply
	created int
}

func (f *floatTmux) run(argv ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, argv)
	return nil
}

func (f *floatTmux) out(argv ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, argv)
	i := f.created
	f.created++
	if i >= len(f.newPane) {
		return "", nil
	}
	return f.newPane[i].id, f.newPane[i].err
}

func (f *floatTmux) config() Config {
	return Config{
		SockPath:    "/run/sock",
		RendererBin: "/nix/store/renderer",
		LocalTmux:   f.run, LocalTmuxOut: f.out,
	}
}

// find returns the first recorded argv starting with verb followed by prefix,
// so an assertion names the command instead of its position in the trace.
func (f *floatTmux) find(verb string, prefix ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.argv {
		if a[0] != verb || len(a) <= len(prefix) {
			continue
		}
		if len(prefix) == 0 || reflect.DeepEqual(a[1:1+len(prefix)], prefix) {
			return a
		}
	}
	return nil
}

// at returns the trace position of the first argv issuing verb against target,
// or -1, for the assertions that are about ordering.
func (f *floatTmux) at(verb, target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.argv {
		if a[0] != verb {
			continue
		}
		if slices.Contains(a, target) {
			return i
		}
	}
	return -1
}

// hellos is the waiter for a test that adds floats: it hands back a canned
// pane->conn map rather than waiting on a socket.
func hellos(byPane map[string]net.Conn) helloWaiter {
	return func([]string) (map[string]net.Conn, error) { return byPane, nil }
}

// deadRT is the round-trip for a path that must not seed: every reply reads as
// a closed stream, which PaneSeeds reports as a per-pane error.
func deadRT(...string) replies {
	return func() (controlmode.Line, bool) { return controlmode.Line{}, false }
}

// oneSeedScript is a single pane's PaneSeed reply pair — the cursor read, then
// the capture. Every test below seeds exactly one float.
func oneSeedScript(screen string) string {
	return strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", screen, "%end 1 2 1",
	}, "\n") + "\n"
}

// TestReconcileFloatsAddMirrorsARemoteFloat covers the whole Add path: the
// outer-box create flags, the outer-box @float_geom stamp, the respawn that
// puts a renderer in the pane, and both map entries. The stamp is the
// non-obvious one — nothing enforces it for a daemon-made float, but
// tmux-float-refit replays it on every window-resized.
func TestReconcileFloatsAddMirrorsARemoteFloat(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	f := &floatTmux{newPane: []newPaneReply{{id: "%l9\n"}}}
	w := newRegistry().add("@1", "@101")
	cell := controlmode.PaneCell{ID: "%7", W: 58, H: 18, X: 11, Y: 6}
	L := controlmode.Layout{W: 100, H: 40, Floats: []controlmode.PaneCell{cell}}

	reconcileFloats(f.config(), w, L, func(string) {}, NewRouter(),
		hellos(map[string]net.Conn{"%7": conn}), setupWindowRT(oneSeedScript("FLOAT-SEED")))

	wantCreate := []string{
		"new-pane", "-d", "-P", "-F", "#{pane_id}", "-t", "@101",
		"-B", "heavy", "-A", "-x", "60", "-y", "20", "-X", "10", "-Y", "5",
	}
	if got := f.find("new-pane"); !reflect.DeepEqual(got, wantCreate) {
		t.Errorf("new-pane argv = %v, want %v", got, wantCreate)
	}
	wantStamp := []string{"set-option", "-p", "-t", "%l9", "@float_geom", "60 20 10 5"}
	if got := f.find("set-option", "-p", "-t", "%l9", "@float_geom"); !reflect.DeepEqual(got, wantStamp) {
		t.Errorf("@float_geom stamp = %v, want %v", got, wantStamp)
	}
	wantSpawn := []string{"respawn-pane", "-k", "-t", "%l9", "--", "/nix/store/renderer", "/run/sock", "%7"}
	if got := f.find("respawn-pane"); !reflect.DeepEqual(got, wantSpawn) {
		t.Errorf("respawn-pane argv = %v, want %v", got, wantSpawn)
	}
	if got := f.find("respawn-pane"); got != nil {
		for _, w := range got {
			if strings.HasPrefix(w, "-e") || strings.Contains(w, "OG_RENDER") {
				t.Errorf("respawn-pane still wires by env: %v", got)
			}
		}
	}
	if w.localFloats["%7"] != "%l9" {
		t.Errorf("localFloats = %v, want %%7 -> %%l9", w.localFloats)
	}
	if w.floatGeom["%7"] != cell {
		t.Errorf("floatGeom[%%7] = %v, want %v", w.floatGeom["%7"], cell)
	}

	// The cell reaches the renderer unconverted: it is the float's usable size,
	// and the inset lives only in tmux's create/resize/move flags.
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	seed, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if seed.Type != wire.FrameSeed || !strings.Contains(string(seed.Payload), "FLOAT-SEED") {
		t.Fatalf("first frame = %v %q, want the float's seed", seed.Type, seed.Payload)
	}
	resize, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read resize: %v", err)
	}
	gw, gh, err := wire.DecodeResize(resize.Payload)
	if err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if gw != 58 || gh != 18 {
		t.Errorf("resize = %dx%d, want the cell 58x18, not the outer box", gw, gh)
	}
}

// TestReconcileFloatsCapsHowManyFloatsItMirrors pins both halves of the cap:
// the mirrored set stops at maxMirroredFloats, and it STAYS there. The cap is
// on the wanted set rather than on the add loop for exactly that second half —
// a loop cut short leaves the excess out of localFloats, so every later pass
// re-plans them as missing and fires another new-pane burst per %layout-change.
func TestReconcileFloatsCapsHowManyFloatsItMirrors(t *testing.T) {
	f := &floatTmux{}
	conns := map[string]net.Conn{}
	floats := make([]controlmode.PaneCell, 0, maxMirroredFloats+8)
	for i := range maxMirroredFloats + 8 {
		id := fmt.Sprintf("%%%d", 100+i)
		floats = append(floats, controlmode.PaneCell{ID: id, W: 20, H: 6, X: 5, Y: 5})
		f.newPane = append(f.newPane, newPaneReply{id: fmt.Sprintf("%%l%d", 100+i)})
		conn, peer := net.Pipe()
		defer func() { _ = conn.Close() }()
		defer func() { _ = peer.Close() }()
		go func() { _, _ = io.Copy(io.Discard, peer) }()
		conns[id] = conn
	}
	L := controlmode.Layout{W: 190, H: 45, Floats: floats}
	w := newRegistry().add("@1", "@101")

	wired := reconcileFloats(f.config(), w, L, func(string) {}, NewRouter(), hellos(conns), deadRT)

	if len(wired) != maxMirroredFloats || len(w.localFloats) != maxMirroredFloats {
		t.Fatalf("mirrored %d floats (%d wired), want the cap %d", len(w.localFloats), len(wired), maxMirroredFloats)
	}
	// f.created counts creates: new-pane is the only command reconcileFloats
	// reads a reply from.
	if f.created != maxMirroredFloats {
		t.Errorf("issued %d creates, want %d", f.created, maxMirroredFloats)
	}
	if _, ok := w.localFloats["%100"]; !ok {
		t.Errorf("localFloats = %v, want the first floats reported, truncated from the tail", w.localFloats)
	}

	if again := reconcileFloats(f.config(), w, L, func(string) {}, NewRouter(), hellos(conns), deadRT); len(again) != 0 {
		t.Errorf("second pass mirrored %v, want nothing: the wanted set is what the cap truncates", again)
	}
	if f.created != maxMirroredFloats {
		t.Errorf("%d creates after the second pass, want no further create", f.created)
	}
}

// TestReconcileFloatsRemoveTearsDownTheLocalFloat pins the teardown's
// completeness: a leftover map entry makes a later pass read the float as still
// mirrored and never re-add it, and a leftover sink outlives the daemon.
func TestReconcileFloatsRemoveTearsDownTheLocalFloat(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = peer.Close() }()

	router := NewRouter()
	router.Register("%7", newOutputSink(conn, nil))

	f := &floatTmux{}
	w := newRegistry().add("@1", "@101")
	w.localFloats["%7"] = "%l9"
	w.floatGeom["%7"] = controlmode.PaneCell{ID: "%7", W: 58, H: 18, X: 11, Y: 6}
	w.conns["%7"] = conn

	reconcileFloats(f.config(), w, controlmode.Layout{W: 100, H: 40}, func(string) {}, router, noHellos, deadRT)

	want := []string{"kill-pane", "-t", "%l9"}
	if got := f.find("kill-pane"); !reflect.DeepEqual(got, want) {
		t.Errorf("kill-pane argv = %v, want %v", got, want)
	}
	if router.sink("%7") != nil {
		t.Error("the float's sink is still registered")
	}
	if len(w.localFloats) != 0 || len(w.floatGeom) != 0 || len(w.conns) != 0 {
		t.Errorf("state after remove: localFloats=%v floatGeom=%v conns=%v, want all empty",
			w.localFloats, w.floatGeom, w.conns)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("the renderer conn is still open")
	}
}

// TestReconcileFloatsMoveResizesThenMovesAndRepaints pins the two halves of a
// geometry change: the outer-box commands in order, and the resize+reseed the
// renderer needs because it holds no back-buffer to reflow.
func TestReconcileFloatsMoveResizesThenMovesAndRepaints(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	router := NewRouter()
	router.Register("%7", newOutputSink(conn, nil))

	f := &floatTmux{}
	w := newRegistry().add("@1", "@101")
	w.localFloats["%7"] = "%l9"
	w.floatGeom["%7"] = controlmode.PaneCell{ID: "%7", W: 58, H: 18, X: 11, Y: 6}

	moved := controlmode.PaneCell{ID: "%7", W: 68, H: 22, X: 21, Y: 9}
	L := controlmode.Layout{W: 100, H: 40, Floats: []controlmode.PaneCell{moved}}

	reconcileFloats(f.config(), w, L, func(string) {}, router, noHellos, setupWindowRT(oneSeedScript("MOVED-REPAINT")))

	wantResize := wantLocalFloatResize("%l9", 70, 24)
	if got := f.find("if-shell", "-t", "%l9"); !reflect.DeepEqual(got, wantResize) {
		t.Errorf("guarded resize argv = %v, want %v", got, wantResize)
	}
	wantMove := []string{"move-pane", "-t", "%l9", "-X", "20", "-Y", "8"}
	if got := f.find("move-pane"); !reflect.DeepEqual(got, wantMove) {
		t.Errorf("move-pane argv = %v, want %v", got, wantMove)
	}
	if r, m := f.at("if-shell", "%l9"), f.at("move-pane", "%l9"); r < 0 || m < 0 || r > m {
		t.Errorf("resize at %d, move at %d: want the size asserted before the position", r, m)
	}
	wantStamp := []string{"set-option", "-p", "-t", "%l9", "@float_geom", "70 24 20 8"}
	if got := f.find("set-option", "-p", "-t", "%l9", "@float_geom"); !reflect.DeepEqual(got, wantStamp) {
		t.Errorf("@float_geom re-stamp = %v, want %v", got, wantStamp)
	}
	if w.floatGeom["%7"] != moved {
		t.Errorf("floatGeom[%%7] = %v, want the cell just applied %v", w.floatGeom["%7"], moved)
	}

	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	resize, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read resize: %v", err)
	}
	gw, gh, err := wire.DecodeResize(resize.Payload)
	if err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if resize.Type != wire.FrameResize || gw != 68 || gh != 22 {
		t.Fatalf("first frame = %v %dx%d, want a resize to the new cell 68x22", resize.Type, gw, gh)
	}
	seed, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if seed.Type != wire.FrameSeed || !strings.Contains(string(seed.Payload), "MOVED-REPAINT") {
		t.Errorf("second frame = %v %q, want a repaint from the remote", seed.Type, seed.Payload)
	}
}

// TestReconcileFloatsWiresTheGraphicsProxy is the yazi case the feature exists
// for: a mirrored float showing image previews needs its kitty stores localised
// exactly as a tiled mirror pane's are, so cfg.graphicsFor has to reach the
// float's sink.
func TestReconcileFloatsWiresTheGraphicsProxy(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	f := &floatTmux{newPane: []newPaneReply{{id: "%l9"}}}
	cfg := f.config()
	cfg.NewGraphics = func(string) *graphics.Proxy {
		return graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	}

	router := NewRouter()
	w := newRegistry().add("@1", "@101")
	cell := controlmode.PaneCell{ID: "%7", W: 58, H: 18, X: 11, Y: 6}
	L := controlmode.Layout{W: 100, H: 40, Floats: []controlmode.PaneCell{cell}}

	reconcileFloats(cfg, w, L, func(string) {}, router,
		hellos(map[string]net.Conn{"%7": conn}), setupWindowRT(oneSeedScript("FLOAT-SEED")))

	sink := router.sink("%7")
	if sink == nil {
		t.Fatal("the float was not registered with the router")
	}
	_, _ = sink.Write(testKittyStore("3"))

	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	for i := range 2 {
		if _, err := wire.ReadFrame(peer); err != nil { // the seed, then the resize
			t.Fatalf("read frame %d: %v", i, err)
		}
	}
	store, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if store.Type != wire.FrameOutput || !strings.Contains(string(store.Payload), kittyLocalisedMarker) {
		t.Errorf("store frame = %v %q, want a locally-fetched payload", store.Type, store.Payload)
	}
}

// TestReconcileFloatsSkipsAFloatItCannotCreate pins the best-effort contract:
// every step of the setupWindow caller around it is fatal, so one decorative
// float that fails must cost only itself — not the other floats, and certainly
// not the mirror.
func TestReconcileFloatsSkipsAFloatItCannotCreate(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	// The first create answers with a reply carrying no pane id — a shape the
	// Add path must reject rather than address a float by.
	f := &floatTmux{newPane: []newPaneReply{{id: "no such window\n"}, {id: "%l9"}}}

	w := newRegistry().add("@1", "@101")
	L := controlmode.Layout{W: 100, H: 40, Floats: []controlmode.PaneCell{
		{ID: "%7", W: 58, H: 18, X: 11, Y: 6},
		{ID: "%8", W: 40, H: 10, X: 5, Y: 5},
	}}

	reconcileFloats(f.config(), w, L, func(string) {}, NewRouter(),
		hellos(map[string]net.Conn{"%8": conn}), setupWindowRT(oneSeedScript("SECOND-FLOAT")))

	if _, ok := w.localFloats["%7"]; ok {
		t.Errorf("localFloats = %v, want no entry for the float that failed to create", w.localFloats)
	}
	if w.localFloats["%8"] != "%l9" {
		t.Errorf("localFloats = %v, want the second float still mirrored", w.localFloats)
	}
}

// TestReconcileFloatsPumpsAFloatWhoseSeedFailed pins the reading that is the
// opposite of the naive one: seedRenderer returning false does not mean a
// stranded pane. wireRenderer registers the sink and wires the pane unseeded
// either way, so tearing it down here would kill a float a later reseed repairs
// — and would leave its keystrokes unrouted in the meantime.
func TestReconcileFloatsPumpsAFloatWhoseSeedFailed(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	f := &floatTmux{newPane: []newPaneReply{{id: "%l9"}}}
	router := NewRouter()
	w := newRegistry().add("@1", "@101")
	L := controlmode.Layout{W: 100, H: 40, Floats: []controlmode.PaneCell{{ID: "%7", W: 58, H: 18, X: 11, Y: 6}}}

	sent := make(chan string, 4)
	// deadRT fails the capture, so the float is wired unseeded.
	reconcileFloats(f.config(), w, L, func(s string) { sent <- s }, router,
		hellos(map[string]net.Conn{"%7": conn}), deadRT)

	if router.sink("%7") == nil {
		t.Fatal("the float's sink was unregistered after a failed seed")
	}
	if w.localFloats["%7"] != "%l9" || w.conns["%7"] == nil {
		t.Fatalf("state after a failed seed: localFloats=%v conns=%v, want the float still mirrored",
			w.localFloats, w.conns)
	}

	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case cmd := <-sent:
		if !strings.Contains(cmd, "%7") {
			t.Errorf("sent %q, want send-keys aimed at the remote float", cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no input reached the remote: the float's pump was never started")
	}
}
