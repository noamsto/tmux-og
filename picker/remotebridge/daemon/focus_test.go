package daemon

import (
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// focusDriver runs the two halves of the echo-suppression machine against each
// other the way the daemon does: a local report goes through planFocusLocked and,
// if it sends, the remote eventually reports it back through applyRemoteFocus;
// a remote-driven local move calls noteLocalFocus first, exactly as the reconcile
// and the main loop do.
type focusDriver struct {
	c   *ctlState
	win string
	seq int64
	// remote is the remote's real active pane, independent of any belief.
	remote string
	// local is the remote pane the local active pane renders.
	local string
	// pending are reports the remote owes us, in order.
	pending []string
}

func newFocusDriver(win string, panes ...string) *focusDriver {
	if len(panes) == 0 {
		panic("newFocusDriver: no panes")
	}
	c := newCtlState()
	c.setWindowPanes(win, panes)
	return &focusDriver{c: c, win: win, remote: panes[0], local: panes[0]}
}

// localMove is the human moving local focus: tmux fires after-select-pane, ctl
// reports it, the daemon may command the remote.
func (d *focusDriver) localMove(pane string) {
	d.local = pane
	d.seq++
	if cmd, ok := d.c.planFocusLocked(d.win, pane, d.seq); ok {
		if cmd != "select-pane -t "+pane {
			panic("unexpected command: " + cmd)
		}
		d.remote = pane
		d.pending = append(d.pending, pane)
	}
}

// deliver hands the daemon the next report the remote owes, returning whether it
// moved local focus (which would fire the hook again).
func (d *focusDriver) deliver() (moved bool) {
	if len(d.pending) == 0 {
		return false
	}
	pane := d.pending[0]
	d.pending = d.pending[1:]
	return d.applyRemote(pane)
}

// applyRemote is a %window-pane-changed arriving for pane.
func (d *focusDriver) applyRemote(pane string) (moved bool) {
	got, follow := d.c.applyRemoteFocus(d.win, pane)
	if !follow {
		return false
	}
	// The daemon records before issuing the local select-pane, then the local hook
	// fires and ctl reports it back.
	d.c.noteLocalFocus(d.win, got)
	d.local = got
	d.seq++
	if cmd, ok := d.c.planFocusLocked(d.win, got, d.seq); ok {
		panic("local echo was sent back to the remote: " + cmd)
	}
	return true
}

// A local focus change reaches the remote, and the remote's report of it does
// not bounce back into local focus.
func TestFocusLocalToRemoteThenEchoStops(t *testing.T) {
	d := newFocusDriver("@1", "%1", "%2", "%3")
	d.localMove("%2")
	if d.remote != "%2" {
		t.Fatalf("remote = %q, want %%2", d.remote)
	}
	if d.deliver() {
		t.Error("our own report moved local focus")
	}
	if d.local != "%2" || d.remote != "%2" {
		t.Errorf("local=%q remote=%q, want both %%2", d.local, d.remote)
	}
}

// An external remote focus change pulls local focus along, and the local hook's
// resulting report is not sent back — the loop closes in one step.
func TestFocusRemoteToLocalThenEchoStops(t *testing.T) {
	d := newFocusDriver("@1", "%1", "%2", "%3")
	if !d.applyRemote("%3") {
		t.Fatal("an external remote change should move local focus")
	}
	if d.local != "%3" {
		t.Errorf("local = %q, want %%3", d.local)
	}
	if len(d.pending) != 0 {
		t.Errorf("pending = %v, want nothing sent back", d.pending)
	}
}

// A fast local walk must not make the mirror walk back through the stale
// reports: every commanded pane is absorbed as an echo.
func TestFocusRapidWalkDoesNotFlicker(t *testing.T) {
	d := newFocusDriver("@1", "%1", "%2", "%3")
	d.localMove("%2")
	d.localMove("%3")
	d.localMove("%1")
	for len(d.pending) > 0 {
		if d.deliver() {
			t.Fatal("a commanded report moved local focus (flicker)")
		}
	}
	if d.local != "%1" || d.remote != "%1" {
		t.Errorf("local=%q remote=%q, want both %%1", d.local, d.remote)
	}
}

// tmux does not report a select-pane that was already a no-op, so a commanded
// pane can go unreported forever. Popping through a later match must flush it,
// or the stale entry would swallow a future genuine external change.
func TestFocusUnreportedCommandDoesNotLeak(t *testing.T) {
	d := newFocusDriver("@1", "%1", "%2", "%3")
	d.localMove("%2")
	d.localMove("%3")
	// The remote only ever reports the second one.
	d.pending = []string{"%3"}
	if d.deliver() {
		t.Fatal("our own report moved local focus")
	}

	f := d.c.focus["@1"]
	if f == nil {
		t.Fatal("no focus state for @1")
	}
	if len(f.commanded) != 0 {
		t.Fatalf("commanded = %v, want flushed", f.commanded)
	}
	// A genuine external change must now still get through.
	if !d.applyRemote("%1") {
		t.Error("a genuine external change was swallowed by a stale entry")
	}
}

// An unmatched report means the remote moved for its own reasons, so any
// outstanding intents are stale and must not swallow later reports.
func TestFocusUnmatchedReportClearsOutstanding(t *testing.T) {
	d := newFocusDriver("@1", "%1", "%2", "%3")
	d.localMove("%2")
	if !d.applyRemote("%3") {
		t.Fatal("an external change during an outstanding command should apply")
	}
	f := d.c.focus["@1"]
	if f == nil {
		t.Fatal("no focus state for @1")
	}
	if len(f.commanded) != 0 {
		t.Errorf("commanded = %v, want cleared", f.commanded)
	}
}

// The focus hook runs backgrounded, so requests can arrive out of order. An
// older one must not install a stale belief.
func TestFocusStaleSequenceDropped(t *testing.T) {
	c := newCtlState()
	c.setWindowPanes("@1", []string{"%1", "%2", "%3"})
	if _, ok := c.planFocusLocked("@1", "%3", 5); !ok {
		t.Fatal("first request should send")
	}
	if _, ok := c.planFocusLocked("@1", "%2", 4); ok {
		t.Error("an out-of-order older request should be dropped")
	}
	f := c.focus["@1"]
	if f == nil {
		t.Fatal("no focus state for @1")
	}
	if got := f.localActive; got != "%3" {
		t.Errorf("localActive = %q, want %%3 (the newer request)", got)
	}
}

// The FIFO is bounded, so a storm of unreported commands cannot grow it without
// limit.
func TestFocusCommandedFIFOBounded(t *testing.T) {
	c := newCtlState()
	c.setWindowPanes("@1", []string{"%1", "%2"})
	for i := range maxCommandedFocus * 3 {
		pane := "%1"
		if i%2 == 1 {
			pane = "%2"
		}
		c.planFocusLocked("@1", pane, int64(i+1))
	}
	f := c.focus["@1"]
	if f == nil {
		t.Fatal("no focus state for @1")
	}
	if got := len(f.commanded); got > maxCommandedFocus {
		t.Errorf("commanded grew to %d, want <= %d", got, maxCommandedFocus)
	}
}

// A report for a window this daemon does not mirror must never act (the standing
// registry guard).
func TestFocusUnknownWindowIsInert(t *testing.T) {
	c := newCtlState()
	c.setWindowPanes("@1", []string{"%1"})
	if _, ok := c.applyRemoteFocus("@99", "%7"); ok {
		t.Error("a report for an unmirrored window should do nothing")
	}
	if _, ok := c.planFocusLocked("@99", "%7", 1); ok {
		t.Error("a focus request for an unmirrored window should do nothing")
	}
}

// A verb that implicitly moves the remote's active pane leaves the belief
// unknowable, so it must be invalidated rather than guessed — otherwise the
// belief-equality guard would suppress the next focus command.
func TestSplitInvalidatesRemoteActiveBelief(t *testing.T) {
	c := newCtlState()
	c.setWindowPanes("@1", []string{"%1", "%2"})
	if _, ok := c.planFocusLocked("@1", "%2", 1); !ok {
		t.Fatal("first focus should send")
	}
	f := c.focus["@1"]
	if f == nil {
		t.Fatal("no focus state for @1")
	}
	if got := f.remoteActivePane; got != "%2" {
		t.Fatalf("remoteActivePane = %q, want %%2", got)
	}

	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "split-h", "%2"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl: %v", err)
	}
	c.submit(req, func(...string) bool { return true })

	if got := f.remoteActivePane; got != "" {
		t.Errorf("remoteActivePane = %q, want invalidated after a split", got)
	}
}

// A verb carrying moves:true can land the remote's focus on a float — prefix + g
// opens lazygit floating — and a float holds no slot in the tiled pane order, so
// the ordinal lookup could only ever miss it and leave local focus on a renderer
// the user is not typing into.
func TestFocusLocalPaneFollowsAMirroredFloat(t *testing.T) {
	c := newCtlState()
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1"}
	w.localPanes = []string{"%l0", "%l1"}
	w.localFloats["%9"] = "%l9"
	c.setWindowPanes("@1", w.allRemotePanes())

	var argv [][]string
	notedFirst := false
	cfg := Config{LocalTmux: func(a ...string) error {
		argv = append(argv, a)
		c.mu.Lock()
		notedFirst = c.focus["@1"].localActive == "%9"
		c.mu.Unlock()
		return nil
	}}

	focusLocalPane(cfg, c, w, w.remotePanes, "%9")

	if len(argv) != 1 || argv[0][0] != "select-pane" || argv[0][len(argv[0])-1] != "%l9" {
		t.Fatalf("issued %v, want one select-pane against the local float %%l9", argv)
	}
	// The local select-pane fires after-select-pane, so the note has to be in
	// place by then or the resulting ctl report reads as a fresh gesture.
	if !notedFirst {
		t.Error("noteLocalFocus had not run when select-pane was issued")
	}
}
