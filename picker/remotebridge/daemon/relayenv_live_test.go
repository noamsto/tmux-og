package daemon

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// liveReplyTimeout bounds every reply wait on a live controller's connection,
// so a desynced stream fails the test instead of wedging it.
const liveReplyTimeout = 5 * time.Second

// ctlProcess is a `tmux -C attach-session` child as the transport under a
// production ctlConn.
type ctlProcess struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (p *ctlProcess) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *ctlProcess) Write(b []byte) (int, error) { return p.in.Write(b) }

// Close kills the client rather than closing its stdin, so it stands in for
// a daemon that dies without running teardown; killing also unparks the pump
// blocked on stdout.
func (p *ctlProcess) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	})
	return nil
}

// liveController is one bridge daemon's owner state over its own control
// client of the isolated server.
type liveController struct {
	t    *testing.T
	conn *ctlConn
	ctl  ownerCtl
}

func startLiveController(t *testing.T, tmux func(args ...string) *exec.Cmd, sessID string, sixel bool, cell string) *liveController {
	t.Helper()
	cmd := tmux("-C", "attach-session", "-t", "w")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("control client: %v", err)
	}
	c := newCtlConn(&ctlProcess{cmd: cmd, in: in, out: out})
	c.replyTimeout = liveReplyTimeout
	c.bind(NewRouter())
	t.Cleanup(c.close)

	view := &Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(sixel, ""))}
	view.SetCell(cell)
	lc := &liveController{t: t, conn: c, ctl: ownerCtl{
		own:    newSizeOwner(newOwnerToken()),
		sessID: sessID,
		rt:     c.rt,
		send:   c.st.send,
		cv:     newConverger(),
		view:   view,
	}}
	lc.sync()
	return lc
}

// sync round-trips one cheap command. Commands on one control client run in
// order, an if-shell's branch included, so once its reply is back every
// command sent before it has run.
func (lc *liveController) sync() {
	lc.t.Helper()
	next := lc.conn.rt("display-message -p og-sync")
	l, ok := next()
	if !ok || l.Kind != controlmode.End || strings.TrimSpace(string(l.Data)) != "og-sync" {
		lc.t.Fatalf("sync round-trip: ok=%v kind=%v data=%q", ok, l.Kind, l.Data)
	}
}

// publish sends this controller's guarded env publish without claiming, the
// write a non-owner makes on attach or on a local capability change.
func (lc *liveController) publish() {
	lc.t.Helper()
	if !lc.ctl.send(lc.ctl.relayEnv().publish(lc.ctl.view)...) {
		lc.t.Fatal("publish send failed")
	}
	lc.sync()
}

func (lc *liveController) claim() {
	lc.t.Helper()
	lc.ctl.claim()
	lc.sync()
}

func (lc *liveController) envTeardown() {
	lc.t.Helper()
	lc.ctl.envTeardown()
	lc.sync()
}

func (lc *liveController) stop() { lc.conn.close() }

// liveEnvSlot is the isolated server's view of session w's relay and cell env.
type liveEnvSlot struct {
	t    *testing.T
	tmux func(args ...string) *exec.Cmd
}

func startLiveEnvSlot(t *testing.T) liveEnvSlot {
	t.Helper()
	requireLiveTmux(t)
	return liveEnvSlot{t: t, tmux: startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))}
}

func (s liveEnvSlot) run(args ...string) string {
	s.t.Helper()
	out, err := s.tmux(args...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("tmux %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s liveEnvSlot) sessionID() string {
	return s.run("display-message", "-p", "-t", "w", "#{session_id}")
}

// env reads one session variable; a removed (-NAME) and an absent variable
// both read as unset.
func (s liveEnvSlot) env(name string) (value string, set bool) {
	s.t.Helper()
	out, err := s.tmux("show-environment", "-t", "w", name).CombinedOutput()
	line := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(line, "unknown variable") {
			return "", false
		}
		s.t.Fatalf("show-environment %s: %v\n%s", name, err, out)
	}
	if line == "-"+name {
		return "", false
	}
	v, ok := strings.CutPrefix(line, name+"=")
	if !ok {
		s.t.Fatalf("show-environment %s: unexpected %q", name, line)
	}
	return v, true
}

func (s liveEnvSlot) assertValues(when, relay, cell string) {
	s.t.Helper()
	if v, set := s.env(RelayEnvVar); !set || v != relay {
		s.t.Errorf("%s: %s = %q (set=%v), want %q", when, RelayEnvVar, v, set, relay)
	}
	if v, set := s.env(CellEnvVar); !set || v != cell {
		s.t.Errorf("%s: %s = %q (set=%v), want %q", when, CellEnvVar, v, set, cell)
	}
}

func (s liveEnvSlot) assertUnset(when string) {
	s.t.Helper()
	for _, name := range []string{RelayEnvVar, CellEnvVar} {
		if v, set := s.env(name); set {
			s.t.Errorf("%s: %s = %q, want unset", when, name, v)
		}
	}
}

func (s liveEnvSlot) ownerRecord() string {
	out, err := s.tmux("show-options", "-v", "-t", "w", bridgeOwnerOption).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (s liveEnvSlot) waitClients(n int) {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(strings.Fields(s.run("list-clients", "-t", "w", "-F", "#{client_name}"))) != n {
		if time.Now().After(deadline) {
			s.t.Fatalf("tmux does not list exactly %d client(s)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRelayEnvTeardownKeepsOtherControllersValues: a controller leaving while
// another controller is attached leaves the owner's values in place; the last
// controller leaving clears them.
func TestRelayEnvTeardownKeepsOtherControllersValues(t *testing.T) {
	slot := startLiveEnvSlot(t)
	sessID := slot.sessionID()
	a := startLiveController(t, slot.tmux, sessID, true, "15x28")
	b := startLiveController(t, slot.tmux, sessID, false, "9x18")

	a.claim()
	b.claim()
	slot.assertValues("after B claims", "", "9x18")
	if t.Failed() {
		t.FailNow()
	}

	a.envTeardown()
	slot.assertValues("after A tears down with B attached", "", "9x18")

	a.stop()
	slot.waitClients(1)
	b.envTeardown()
	slot.assertUnset("after the last controller tears down")
}

// TestRelayEnvFollowsOwnerMixedCapability: the slot carries the record owner's
// values, and a non-owner's publish does not land.
func TestRelayEnvFollowsOwnerMixedCapability(t *testing.T) {
	slot := startLiveEnvSlot(t)
	sessID := slot.sessionID()
	a := startLiveController(t, slot.tmux, sessID, true, "15x28")
	b := startLiveController(t, slot.tmux, sessID, false, "9x18")

	a.claim()
	slot.assertValues("after A claims", "sixel", "15x28")

	b.publish()
	slot.assertValues("after non-owner B publishes", "sixel", "15x28")

	b.claim()
	slot.assertValues("after B claims", "", "9x18")

	a.publish()
	slot.assertValues("after non-owner A publishes", "", "9x18")
	if tok, _ := parseOwnerValue(slot.ownerRecord()); tok != b.ctl.own.token {
		t.Errorf("%s = %q, want B's token %s", bridgeOwnerOption, slot.ownerRecord(), b.ctl.own.token)
	}
}

// TestRelayEnvReleaseCarriesSurvivorValues: an owner killed without teardown
// leaves its record and values; the survivor's release clears the record and
// writes the survivor's values in the same branch.
func TestRelayEnvReleaseCarriesSurvivorValues(t *testing.T) {
	slot := startLiveEnvSlot(t)
	sessID := slot.sessionID()
	a := startLiveController(t, slot.tmux, sessID, true, "15x28")
	b := startLiveController(t, slot.tmux, sessID, false, "9x18")

	a.claim()
	slot.assertValues("after A claims", "sixel", "15x28")
	tokA, clientA := a.ctl.own.token, a.ctl.own.selfName()
	if tok, client := parseOwnerValue(slot.ownerRecord()); tok != tokA || client != clientA {
		t.Fatalf("%s = %q, want %s|%s", bridgeOwnerOption, slot.ownerRecord(), tokA, clientA)
	}

	a.stop()
	slot.waitClients(1)
	slot.assertValues("after A is killed", "sixel", "15x28")

	b.ctl.own.holdRelease(tokA, clientA, time.Now().Add(-time.Second))
	b.ctl.releaseDue(time.Now())
	b.sync()
	if rec := slot.ownerRecord(); rec != "" {
		t.Errorf("%s = %q after the release, want unset", bridgeOwnerOption, rec)
	}
	slot.assertValues("after B releases A's record", "", "9x18")
}

// TestRelayEnvOwnerExitNeutralisesItsValues: an owner leaving while another
// controller stays attached sets relay empty and unsets the cell, so the
// survivor never inherits a capability its terminal lacks, and leaves the
// owner record to the survivor's release.
func TestRelayEnvOwnerExitNeutralisesItsValues(t *testing.T) {
	slot := startLiveEnvSlot(t)
	sessID := slot.sessionID()
	a := startLiveController(t, slot.tmux, sessID, true, "15x28")
	b := startLiveController(t, slot.tmux, sessID, false, "9x18")

	b.claim()
	a.claim()
	slot.assertValues("after A claims", "sixel", "15x28")
	record := slot.ownerRecord()
	if tok, _ := parseOwnerValue(record); tok != a.ctl.own.token {
		t.Fatalf("%s = %q, want A's token %s", bridgeOwnerOption, record, a.ctl.own.token)
	}
	if t.Failed() {
		t.FailNow()
	}

	a.envTeardown()
	if v, set := slot.env(RelayEnvVar); !set || v != "" {
		t.Errorf("%s = %q (set=%v) after the owner exits, want present-empty", RelayEnvVar, v, set)
	}
	if v, set := slot.env(CellEnvVar); set {
		t.Errorf("%s = %q after the owner exits, want unset", CellEnvVar, v)
	}
	if got := slot.ownerRecord(); got != record {
		t.Errorf("%s = %q after the owner exits, want it unchanged (%q)", bridgeOwnerOption, got, record)
	}
}

// TestCarouselPressCarriesPressingControllerEnv: a carousel press on a
// controller that has not yet claimed ownership still opens the viewer pane
// with that controller's relay and cell values, not the record owner's. The
// stub toggle mimics aeye's: it forwards AEYE_BRIDGED and a non-empty
// AEYE_CELL_PX to the new pane with -e and never OG_RELAY_GRAPHICS, so the
// pane can only get the relay value from the session environment.
func TestCarouselPressCarriesPressingControllerEnv(t *testing.T) {
	requireLiveTmux(t)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.jsonl")
	if err := os.WriteFile(manifest, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	viewLog := filepath.Join(dir, "viewer.log")
	viewer := filepath.Join(dir, "viewer.sh")
	writeStub(t, viewer, `#!/bin/sh
printf '%s|%s\n' "$OG_RELAY_GRAPHICS" "$AEYE_CELL_PX" >>"`+viewLog+`"
`)
	toggle := filepath.Join(dir, "tmux-claude-images")
	writeStub(t, toggle, `#!/bin/sh
if [ "$1" = --resolve ]; then
	printf 'tmux\tkey\t`+manifest+`\n'
	exit 0
fi
if [ -n "$AEYE_CELL_PX" ]; then
	tmux split-window -d -t "$TMUX_PANE" -e AEYE_BRIDGED="$AEYE_BRIDGED" -e AEYE_CELL_PX="$AEYE_CELL_PX" "`+viewer+`"
else
	tmux split-window -d -t "$TMUX_PANE" -e AEYE_BRIDGED="$AEYE_BRIDGED" "`+viewer+`"
fi
`)

	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))
	slot := liveEnvSlot{t: t, tmux: tmux}
	// The option wins over PATH, which the tmux wrapper rewrites inside
	// run-shell.
	slot.run("set-option", "-g", "@carousel_bin", toggle)
	sessID := slot.sessionID()
	pane := slot.run("display-message", "-p", "-t", "w", "#{pane_id}")

	a := startLiveController(t, tmux, sessID, true, "15x28")
	b := startLiveController(t, tmux, sessID, false, "9x18")
	a.claim()
	slot.assertValues("after A claims", "sixel", "15x28")
	if t.Failed() {
		t.FailNow()
	}

	cst := newMirrorCtlState(b.ctl.view, sessID, b.ctl.own, true)
	cst.setWindowPanes("@1", []string{pane})
	rep, _, _ := replacerFixture("foot", true)
	argv := []string{wire.CtlProtocolVersion, "carousel", pane}
	if err := handleCtl(cst, rep, newCarouselProbe(), argv, "w", nil, b.ctl.send); err != nil {
		t.Fatalf("handleCtl: %v", err)
	}
	b.sync()
	if tok, _ := parseOwnerValue(slot.ownerRecord()); tok != a.ctl.own.token {
		t.Errorf("%s = %q, want A's token %s: the press must not have claimed", bridgeOwnerOption, slot.ownerRecord(), a.ctl.own.token)
	}
	if !b.ctl.own.takeClaim() {
		t.Error("takeClaim = false after the press, want the claim queued for the main loop")
	}

	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := os.ReadFile(viewLog); len(out) > 0 {
			got = strings.TrimSpace(string(out))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "|9x18" {
		t.Errorf("viewer pane env = %q (relay|cell), want %q: B's values, not A's %q", got, "|9x18", "sixel|15x28")
	}
}
