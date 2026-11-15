package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// attachTestModel is a Remote-section model (remoteFixture, cursor unset)
// pointed at a fake og-remote-open so beginAttach never execs the real
// launcher.
func attachTestModel(bin string) tuiModel {
	m := tuiModel{
		allItems: remoteFixture(), sessionItems: remoteFixture()[:1], remoteItems: remoteFixture()[1:],
		tmuxOpts: map[string]string{"@remote_open_bin": bin},
	}
	return m.withFilter()
}

// runBatchAsync runs cmd — a plain Cmd, or the tea.BatchMsg a tea.Batch
// produces — each leaf in its own goroutine, forwarding every non-nil message
// to the returned channel. Mirrors the concurrency bubbletea's own runtime
// gives a Cmd, without a whole Program: each attach Cmd only ever yields one
// message per invocation, so a fresh call is needed after every Update.
func runBatchAsync(cmd tea.Cmd) <-chan tea.Msg {
	out := make(chan tea.Msg, 16)
	var dispatch func(tea.Cmd)
	dispatch = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if msg == nil {
				return
			}
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range batch {
					dispatch(sub)
				}
				return
			}
			out <- msg
		}()
	}
	dispatch(cmd)
	return out
}

// awaitAttachMsg reads ch until a message matching pred arrives, discarding
// everything else — a spinner tick whose Cmd is never re-dispatched here just
// stops ticking, which nothing in these tests asserts on.
func awaitAttachMsg(t *testing.T, ch <-chan tea.Msg, within time.Duration, pred func(tea.Msg) bool) tea.Msg {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case msg := <-ch:
			if pred(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("no matching message within " + within.String())
			return nil
		}
	}
}

func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// Update never launches synchronously. Enter returns almost instantly
// with attach set and a Cmd that has not run yet; only once that Cmd is
// actually driven (as bubbletea's runtime would) does the launcher fork, and
// cancel rolls all the way through to a cleared attach.
func TestAttachUpdateNeverLaunchesSynchronously(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "STARTED")
	bin := fakeLauncher(t, "touch "+started+"\nprintf 'connect\\n' >&3\nexec sleep 30")
	m := attachTestModel(bin)
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.target == "remote:lab:mono" })

	t0 := time.Now()
	next, cmd := m.Update(wallKey("enter"))
	if elapsed := time.Since(t0); elapsed >= 250*time.Millisecond {
		t.Fatalf("Update took %v, want < 250ms", elapsed)
	}
	nm := next.(tuiModel)
	if nm.attach == nil {
		t.Fatal("attach not set")
	}
	if cmd == nil {
		t.Fatal("expected a Cmd")
	}

	time.Sleep(200 * time.Millisecond)
	assertNoFile(t, started)

	id := nm.attach.id
	ch := runBatchAsync(cmd)
	waitForFile(t, started, 5*time.Second)

	msg := awaitAttachMsg(t, ch, 5*time.Second, func(m tea.Msg) bool {
		p, ok := m.(attachPhaseMsg)
		return ok && p.id == id && p.phase == phaseConnect
	})
	next, cmd = nm.Update(msg)
	nm = next.(tuiModel)
	if nm.attach == nil || nm.attach.phase != phaseConnect {
		t.Fatalf("attach = %+v, want phase connect", nm.attach)
	}
	ch2 := runBatchAsync(cmd)

	next, cmd = nm.Update(wallKey("esc"))
	nm = next.(tuiModel)
	if cmd != nil {
		t.Errorf("esc returned a Cmd, want nil")
	}
	if nm.attach == nil || !nm.attach.cancelling {
		t.Fatalf("attach.cancelling = %+v, want true", nm.attach)
	}

	// attachKillGrace, not a fixed 5s: a TERM that doesn't land forces the
	// production code itself to wait out the grace before KILL, so the test's
	// own deadline must never race that constant (cf. TestAttachCancelEscalatesToKill).
	// The launcher execs sleep, so no descendant holds the phase pipe; the orphan
	// case is pinned by TestAttachCancelBoundedByOrphanHoldingPipe.
	msg = awaitAttachMsg(t, ch2, attachKillGrace+2*time.Second, func(m tea.Msg) bool {
		d, ok := m.(attachDoneMsg)
		return ok && d.id == id
	})
	if got := msg.(attachDoneMsg).result.outcome; got != attachCancelled {
		t.Fatalf("outcome = %v, want attachCancelled", got)
	}
	next, _ = nm.Update(msg)
	nm = next.(tuiModel)
	if nm.attach != nil {
		t.Errorf("attach not cleared after done: %+v", nm.attach)
	}
	if !strings.Contains(nm.statusMsg, "cancelled") {
		t.Errorf("statusMsg = %q, want it to contain %q", nm.statusMsg, "cancelled")
	}
}

// Every key but esc/ctrl+c is a no-op while an attach is in flight, so
// a second concurrent attach can never start. The base model's cursor sits on
// a real Remote-section row (remote:lab:mono) precisely so an un-gated enter
// would reach activateCurrent and return a Cmd and a new attach.id — with no
// rows, it would return nil and look gated whether or not the gate held.
func TestAttachKeysIgnoredDuringAttach(t *testing.T) {
	run := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	base := attachTestModel(fakeLauncher(t, "sleep 30"))
	base.query = "q"
	base.cursor = findVisible(t, base, func(it listItem) bool { return it.target == "remote:lab:mono" })
	base.marked = map[string]bool{"remote:lab:mono": true}
	base.attach = &attachState{id: 7, run: run, host: "lab", sess: "mono", label: "lab/mono", phase: phaseConnect}
	wantCursor := base.cursor

	for _, key := range []string{"enter", "j", "ctrl+x", "tab", "ctrl+t", "down"} {
		next, cmd := base.Update(wallKey(key))
		nm := next.(tuiModel)
		if cmd != nil {
			t.Errorf("%s: expected nil cmd, got %v", key, cmd)
		}
		if nm.attach == nil || nm.attach.id != 7 {
			t.Errorf("%s: attach.id changed: %+v", key, nm.attach)
		}
		if nm.query != "q" || nm.cursor != wantCursor || !nm.marked["remote:lab:mono"] {
			t.Errorf("%s: model mutated: query=%q cursor=%d marked=%v", key, nm.query, nm.cursor, nm.marked)
		}
	}
}

// Esc starts cancelling; a second ctrl+c while cancelling ends the TUI,
// but runTUI still waits (bounded) for the launcher's rollback before returning.
func TestAttachEscCancelsCtrlCQuitsWhileCancelling(t *testing.T) {
	run := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	m := tuiModel{attach: &attachState{id: 1, run: run, host: "lab", sess: "mono", label: "lab/mono", phase: phaseConnect}}

	next, cmd := m.Update(wallKey("esc"))
	nm := next.(tuiModel)
	if cmd != nil {
		t.Errorf("esc returned a Cmd, want nil")
	}
	if nm.attach == nil || !nm.attach.cancelling {
		t.Fatal("cancelling not set")
	}
	if run.ctx.Err() == nil {
		t.Error("run's ctx not cancelled")
	}

	run2 := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	m2 := tuiModel{attach: &attachState{id: 1, run: run2, host: "lab", sess: "mono", label: "lab/mono", phase: phaseConnect}}
	next, cmd = m2.Update(wallKey("ctrl+c"))
	nm2 := next.(tuiModel)
	if cmd != nil {
		t.Fatal("first ctrl+c returned a Cmd, want cancelling (not tea.Quit)")
	}
	if nm2.attach == nil || !nm2.attach.cancelling {
		t.Fatal("cancelling not set by the first ctrl+c")
	}
	if run2.ctx.Err() == nil {
		t.Error("run's ctx not cancelled")
	}

	if _, cmd = nm2.Update(wallKey("ctrl+c")); cmd == nil {
		t.Fatal("expected tea.Quit on the second ctrl+c while cancelling")
	}
}

// Mouse clicks and wheel events are ignored while an attach is in
// flight, same as every other key.
func TestAttachMouseIgnoredDuringAttach(t *testing.T) {
	run := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	m := tuiModel{
		mode: modeList, cursor: 1, visible: remoteFixture(),
		attach: &attachState{id: 1, run: run, host: "lab", sess: "mono", label: "lab/mono"},
	}

	next, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	nm := next.(tuiModel)
	if cmd != nil || nm.cursor != 1 {
		t.Errorf("wheel handled during attach: cmd=%v cursor=%d", cmd, nm.cursor)
	}

	next, cmd = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 3})
	nm = next.(tuiModel)
	if cmd != nil || nm.cursor != 1 {
		t.Errorf("click handled during attach: cmd=%v cursor=%d", cmd, nm.cursor)
	}
}

// The four outcomes finishAttach can land on.
func TestAttachOutcomes(t *testing.T) {
	t.Run("ok launches the rest in order, quits, clears marks", func(t *testing.T) {
		var launched []string
		orig := launchDetached
		launchDetached = func(_ map[string]string, host, sess string, _ bool) {
			launched = append(launched, host+"/"+sess)
		}
		t.Cleanup(func() { launchDetached = orig })

		rest := []listItem{
			{remoteHost: "lab", remoteSess: "other"},
			{remoteHost: "lab2", remoteSess: "x"},
		}
		m := tuiModel{
			marked: map[string]bool{"remote:lab:mono": true, "remote:lab:other": true},
			attach: &attachState{id: 1, label: "lab/mono", rest: rest},
		}
		next, cmd := m.finishAttach(attachResult{outcome: attachOK})
		if cmd == nil {
			t.Fatal("expected tea.Quit")
		}
		nm := next.(tuiModel)
		if nm.attach != nil {
			t.Error("attach not cleared")
		}
		if len(nm.marked) != 0 {
			t.Error("marks not cleared")
		}
		want := []string{"lab/other", "lab2/x"}
		if len(launched) != len(want) {
			t.Fatalf("launchDetached calls = %v, want %v", launched, want)
		}
		for i := range want {
			if launched[i] != want[i] {
				t.Errorf("launchDetached()[%d] = %q, want %q", i, launched[i], want[i])
			}
		}
	})

	t.Run("failed keeps marks and cursor, sets retry status", func(t *testing.T) {
		m := tuiModel{marked: map[string]bool{"remote:lab:mono": true}, attach: &attachState{id: 1, label: "lab/mono"}}
		next, cmd := m.finishAttach(attachResult{outcome: attachFailed, msg: "connection refused"})
		if cmd != nil {
			t.Error("expected nil cmd for a failure")
		}
		nm := next.(tuiModel)
		if nm.attach != nil {
			t.Error("attach not cleared")
		}
		if len(nm.marked) != 1 {
			t.Error("marks cleared on failure, want kept")
		}
		want := "lab/mono: connection refused — enter to retry"
		if nm.statusMsg != want {
			t.Errorf("statusMsg = %q, want %q", nm.statusMsg, want)
		}
	})

	t.Run("timed out names the phase and budget", func(t *testing.T) {
		m := tuiModel{attach: &attachState{id: 1, label: "lab/mono"}}
		next, cmd := m.finishAttach(attachResult{outcome: attachTimedOut, phase: phaseConnect, budget: 20 * time.Second})
		if cmd != nil {
			t.Error("expected nil cmd for a timeout")
		}
		nm := next.(tuiModel)
		for _, want := range []string{"timed out connecting", "20s", "enter to retry"} {
			if !strings.Contains(nm.statusMsg, want) {
				t.Errorf("statusMsg = %q, want it to contain %q", nm.statusMsg, want)
			}
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		m := tuiModel{attach: &attachState{id: 1, label: "lab/mono"}}
		next, cmd := m.finishAttach(attachResult{outcome: attachCancelled})
		if cmd != nil {
			t.Error("expected nil cmd for a cancel")
		}
		nm := next.(tuiModel)
		if nm.statusMsg != "cancelled opening lab/mono" {
			t.Errorf("statusMsg = %q, want %q", nm.statusMsg, "cancelled opening lab/mono")
		}
	})
}

// A failure or timeout leaves the cursor where it was, so Enter retries
// the same row — as a fresh attempt, with a higher id than the one it replaces.
func TestAttachRetryStartsNewAttachWithHigherID(t *testing.T) {
	m := attachTestModel(fakeLauncher(t, "sleep 30"))
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.target == "remote:lab:mono" })

	next, cmd := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	if cmd == nil {
		t.Fatal("expected a Cmd")
	}
	if nm.attach == nil {
		t.Fatal("attach not started")
	}
	firstID := nm.attach.id

	next, _ = nm.finishAttach(attachResult{outcome: attachFailed, msg: "boom"})
	nm = next.(tuiModel)
	if nm.attach != nil {
		t.Fatal("attach not cleared after failure")
	}

	next, cmd = nm.Update(wallKey("enter"))
	nm = next.(tuiModel)
	if cmd == nil {
		t.Fatal("expected a Cmd on retry")
	}
	if nm.attach == nil || nm.attach.id <= firstID {
		t.Fatalf("attach.id = %+v, want > %d", nm.attach, firstID)
	}
}

// A message from a superseded attempt (or an idle tick) is dropped.
func TestAttachStaleMessagesIgnored(t *testing.T) {
	run := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	base := tuiModel{
		query:  "q",
		attach: &attachState{id: 5, run: run, host: "lab", sess: "mono", label: "lab/mono", phase: phaseConnect},
	}

	for _, msg := range []tea.Msg{
		attachPhaseMsg{id: 4, phase: phaseMirror},
		attachDoneMsg{id: 4, result: attachResult{outcome: attachOK}},
		attachTickMsg{id: 4},
	} {
		next, cmd := base.Update(msg)
		nm := next.(tuiModel)
		if cmd != nil {
			t.Errorf("%#v: expected nil cmd for a stale id", msg)
		}
		if nm.attach == nil || nm.attach.phase != phaseConnect || nm.attach.id != 5 {
			t.Errorf("%#v: model changed: %+v", msg, nm.attach)
		}
	}

	idle := tuiModel{}
	next, cmd := idle.Update(attachTickMsg{id: 1})
	if cmd != nil {
		t.Error("expected nil cmd for a tick while idle")
	}
	if next.(tuiModel).attach != nil {
		t.Error("attach set from a stray tick")
	}
}

// The status line always fits exactly m.width, and at a width wide
// enough to show it, carries the phase, label and cancel hint — sanitized,
// even when the remote-derived label carries an escape sequence.
func TestAttachStatusLineRendering(t *testing.T) {
	m := tuiModel{width: 80}
	m.beginAttach(listItem{remoteHost: "lab", remoteSess: "mono"}, nil)
	if m.attach == nil {
		t.Fatal("attach not started")
	}
	id := m.attach.id
	next, _ := m.Update(attachPhaseMsg{id: id, phase: phaseConnect})
	m = next.(tuiModel)

	for _, w := range []int{12, 40, 80, 160} {
		m.width = w
		got := m.renderHints()
		if visibleWidth(got) != w {
			t.Errorf("width %d: visibleWidth(%q) = %d, want %d", w, got, visibleWidth(got), w)
		}
	}

	m.width = 80
	plain := stripANSI(m.renderHints())
	for _, want := range []string{"connecting", "lab/mono", "esc:cancel"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered status %q missing %q", plain, want)
		}
	}

	tainted := tuiModel{width: 80}
	tainted.beginAttach(listItem{remoteHost: "lab", remoteSess: "mo\x1b[2Jno"}, nil)
	if rendered := tainted.renderHints(); strings.Contains(rendered, "\x1b[2J") {
		t.Errorf("rendered status leaked a raw escape sequence: %q", rendered)
	}
}

// Opening several marked sessions starts only the first as the attach;
// the rest wait as its remainder until it succeeds.
func TestAttachMultiOpenDefersRest(t *testing.T) {
	var launched []string
	orig := launchDetached
	launchDetached = func(_ map[string]string, host, sess string, _ bool) {
		launched = append(launched, host+"/"+sess)
	}
	t.Cleanup(func() { launchDetached = orig })

	marked := []listItem{
		{target: "remote:lab:mono", remoteHost: "lab", remoteSess: "mono"},
		{target: "remote:lab:other", remoteHost: "lab", remoteSess: "other"},
	}
	m := tuiModel{marked: map[string]bool{"remote:lab:mono": true, "remote:lab:other": true}}
	next, cmd := m.openMarkedRemote(marked)
	if cmd == nil {
		t.Fatal("expected a Cmd starting the attach")
	}
	nm := next.(tuiModel)
	if nm.attach == nil || nm.attach.host != "lab" || nm.attach.sess != "mono" {
		t.Fatalf("attach = %+v, want lab/mono", nm.attach)
	}
	if len(nm.attach.rest) != 1 || nm.attach.rest[0].remoteHost != "lab" || nm.attach.rest[0].remoteSess != "other" {
		t.Fatalf("attach.rest = %+v, want [lab/other]", nm.attach.rest)
	}
	if len(launched) != 0 {
		t.Errorf("launchDetached called before the first attach resolved: %v", launched)
	}
}

// windowAttachModel is a window-mode model with the cursor on lab's remote
// window api:2 and the launcher swapped for a fake.
func windowAttachModel(t *testing.T, bin string) tuiModel {
	t.Helper()
	m := windowModeRemoteModel(t, false)
	m.tmuxOpts["@remote_open_bin"] = bin
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.target == "remote:lab:api:@4" })
	return m
}

func TestAttachWindowRowPassesIDToLauncher(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "ARGV")
	bin := fakeLauncher(t, `printf '%s\n' "$@" > `+argv)
	m := windowAttachModel(t, bin)

	next, cmd := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	if nm.attach == nil || cmd == nil {
		t.Fatalf("enter did not start an attach: attach=%+v cmd=%v", nm.attach, cmd)
	}
	if nm.attach.label != "lab/api:2" {
		t.Errorf("attach.label = %q, want lab/api:2", nm.attach.label)
	}
	time.Sleep(200 * time.Millisecond)
	assertNoFile(t, argv)

	id := nm.attach.id
	ch := runBatchAsync(cmd)
	awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
		d, ok := msg.(attachDoneMsg)
		return ok && d.id == id
	})
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if want := "lab\napi\n@4\n"; string(got) != want {
		t.Errorf("launcher argv = %q, want %q", got, want)
	}
}

// Index 0 is a real window (base-index 0); passing the index would drop it.
func TestAttachWindowRowAtIndexZeroPassesID(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "ARGV")
	m := windowAttachModel(t, fakeLauncher(t, `printf '%s\n' "$@" > `+argv))
	first := listItem{
		isRemoteRow: true, remoteHost: "lab", remoteSess: "api",
		remoteWindowID: "@0", remoteWindowIndex: 0,
	}

	cmd := m.beginAttach(first, nil)
	if m.attach == nil {
		t.Fatal("beginAttach left no attach")
	}
	id := m.attach.id
	ch := runBatchAsync(cmd)
	awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
		d, ok := msg.(attachDoneMsg)
		return ok && d.id == id
	})
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if want := "lab\napi\n@0\n"; string(got) != want {
		t.Errorf("launcher argv = %q, want %q", got, want)
	}
}

// Esc on an attach begun from a window row cancels it like any other attach.
func TestAttachEscCancelsWindowRowAttach(t *testing.T) {
	m := windowAttachModel(t, fakeLauncher(t, "sleep 30"))
	next, _ := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	if nm.attach == nil {
		t.Fatal("enter did not start an attach")
	}
	run := nm.attach.run
	t.Cleanup(run.cancel)

	next, cmd := nm.Update(wallKey("esc"))
	nm = next.(tuiModel)
	if cmd != nil {
		t.Errorf("esc returned a Cmd, want nil")
	}
	if nm.attach == nil || !nm.attach.cancelling {
		t.Fatal("cancelling not set")
	}
	if run.ctx.Err() == nil {
		t.Error("run's ctx not cancelled")
	}
}

func TestAttachFailureOnWindowRowOffersRetry(t *testing.T) {
	bin := fakeLauncher(t, "echo boom >&2\nexit 1")
	m := windowAttachModel(t, bin)

	next, cmd := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	if nm.attach == nil {
		t.Fatal("enter did not start an attach")
	}
	id := nm.attach.id
	ch := runBatchAsync(cmd)
	done := awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
		d, ok := msg.(attachDoneMsg)
		return ok && d.id == id
	})
	next, _ = nm.Update(done)
	nm = next.(tuiModel)
	if !strings.HasPrefix(nm.statusMsg, "lab/api:2: ") || !strings.HasSuffix(nm.statusMsg, "— enter to retry") {
		t.Errorf("statusMsg = %q, want lab/api:2 prefix and retry suffix", nm.statusMsg)
	}
}

func TestWindowRowIsNotMarkable(t *testing.T) {
	m := windowAttachModel(t, "unused")
	next, _ := m.handleKey(wallKey("ctrl+t"))
	if marked := next.(tuiModel).marked; len(marked) != 0 {
		t.Errorf("^t marked a remote window row: %v", marked)
	}
}

// A window row names a window, so ^x must never stage the session kill its
// session-mode twin would.
func TestCtrlXOnRemoteWindowRowStagesNoSessionKill(t *testing.T) {
	m := windowAttachModel(t, "unused")
	next, _ := m.handleKey(wallKey("ctrl+x"))
	if confirm := next.(tuiModel).killConfirm; len(confirm) != 0 {
		t.Errorf("^x staged a kill for a window row: %+v", confirm)
	}
}

// A host row keeps the session picker's Enter in window mode: auth runs in the
// popup's pty, and the inert / tailscale refusals stay put.
func TestWindowModeHostRowEnterKeepsSessionBehaviour(t *testing.T) {
	hostModel := func(err error) tuiModel {
		useRemoteCache(t)
		opts := map[string]string{"@remote_bridge_hosts": "lab", "@remote_auth_bin": "/nonexistent/og-remote-auth"}
		probe := func(string) (remoteProbeResult, error) { return remoteProbeResult{}, err }
		m := newPickerModel(true, false, false, opts, "dark", localWindowItems(false), "")
		m.remoteItems = collectRemoteWindowItems(opts, nil, probe)
		m = m.recombine().withFilter()
		m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteHost == "lab" && it.remoteSess == "" })
		return m
	}

	t.Run("needs auth runs the handshake in the popup", func(t *testing.T) {
		m := hostModel(fmt.Errorf("%w: exit status 255", errRemoteNeedsAuth))
		next, cmd := m.activateCurrent()
		if cmd == nil {
			t.Fatal("want an ExecProcess cmd")
		}
		if got := fmt.Sprintf("%T", cmd()); !strings.HasSuffix(got, "execMsg") {
			t.Errorf("cmd yields %s, want an exec message", got)
		}
		if nm := next.(tuiModel); nm.attach != nil || nm.statusMsg != "" {
			t.Errorf("needs-auth must not start an attach: attach=%+v status=%q", nm.attach, nm.statusMsg)
		}
	})

	t.Run("host key changed refuses", func(t *testing.T) {
		m := hostModel(fmt.Errorf("%w: x", errRemoteHostKeyChanged))
		next, cmd := m.activateCurrent()
		if cmd != nil || !strings.Contains(next.(tuiModel).statusMsg, "host key") {
			t.Errorf("cmd=%v status=%q, want a host-key refusal", cmd, next.(tuiModel).statusMsg)
		}
	})

	t.Run("tailscale check refuses", func(t *testing.T) {
		m := hostModel(fmt.Errorf("%w: x", errRemoteTailscaleCheck))
		next, cmd := m.activateCurrent()
		if got := next.(tuiModel).statusMsg; cmd != nil || !strings.Contains(got, "tailscale check") || !strings.Contains(got, "ssh lab") {
			t.Errorf("cmd=%v status=%q, want the tailscale refusal", cmd, got)
		}
	})
}

// stubPlace seams the tmux lookups behind Enter on a window row and returns the
// windows selectWindowFn was asked to select.
func stubPlace(t *testing.T, cur currentSession, curOK bool, placed map[string]string) *[]string {
	t.Helper()
	origCur, origPlaced, origSel := currentSessionFn, placedWindowFn, selectWindowFn
	t.Cleanup(func() { currentSessionFn, placedWindowFn, selectWindowFn = origCur, origPlaced, origSel })
	currentSessionFn = func(string) (currentSession, bool) { return cur, curOK }
	placedWindowFn = func(sessID, host, sess, pane string) (string, bool) {
		w, ok := placed[sessID+"|"+host+"|"+sess+"|"+pane]
		return w, ok
	}
	var selected []string
	selectWindowFn = func(id string) error { selected = append(selected, id); return nil }
	return &selected
}

// runAttachArgv presses key on m and returns the launcher's argv once the
// attach it started has finished, plus the model after the done message.
func runAttachArgv(t *testing.T, m tuiModel, argvFile, key string) (tuiModel, string) {
	t.Helper()
	next, cmd := m.Update(wallKey(key))
	nm := next.(tuiModel)
	if nm.attach == nil || cmd == nil {
		t.Fatalf("%s did not start an attach", key)
	}
	id := nm.attach.id
	done := awaitAttachMsg(t, runBatchAsync(cmd), 5*time.Second, func(msg tea.Msg) bool {
		d, ok := msg.(attachDoneMsg)
		return ok && d.id == id
	})
	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	next, _ = nm.Update(done)
	return next.(tuiModel), string(got)
}

func placeModel(t *testing.T, body string) (tuiModel, string) {
	t.Helper()
	argv := filepath.Join(t.TempDir(), "ARGV")
	m := windowAttachModel(t, fakeLauncher(t, `printf '%s\n' "$@" > `+argv+"\n"+body))
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.target == "remote:lab:api:@4" })
	return m, argv
}

func TestEnterOnWindowRowPlacesIntoCurrentSession(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	selected := stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	m, argv := placeModel(t, "")

	_, got := runAttachArgv(t, m, argv, "enter")

	if want := "lab\napi\n--into\nwork\n--window\n@4\n"; got != want {
		t.Errorf("launcher argv = %q, want %q", got, want)
	}
	if len(*selected) != 0 {
		t.Errorf("selected %v without a probe pane id, want none", *selected)
	}
}

func TestCtrlEOnWindowRowOpensDedicated(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	m, argv := placeModel(t, "")

	_, got := runAttachArgv(t, m, argv, "ctrl+e")

	if want := "lab\napi\n@4\n"; got != want {
		t.Errorf("launcher argv = %q, want %q", got, want)
	}
}

func TestEnterOnWindowRowDedicatedFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		cur    currentSession
		curOK  bool
		lookup bool
	}{
		{"no current window", "", currentSession{id: "$2", name: "work"}, true, false},
		{"lookup failed", "@9", currentSession{}, false, true},
		{"current session is a dedicated mirror", "@9", currentSession{id: "$2", name: "lab-api", dedicated: true}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OG_PICKER_CURRENT_WINDOW", tc.env)
			stubPlace(t, tc.cur, tc.curOK, nil)
			m, argv := placeModel(t, "")

			_, got := runAttachArgv(t, m, argv, "enter")

			if want := "lab\napi\n@4\n"; got != want {
				t.Errorf("launcher argv = %q, want the dedicated %q", got, want)
			}
		})
	}
}

// A placed mirror window in a local session is not a dedicated session, so the
// placed argv still applies (currentSessionFormat blanks its window-scoped stamps).
func TestEnterOnWindowRowFromPlacedMirrorWindowStillPlaces(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	m, argv := placeModel(t, "")

	_, got := runAttachArgv(t, m, argv, "enter")

	if !strings.Contains(got, "--into\nwork\n") {
		t.Errorf("launcher argv = %q, want a placed open", got)
	}
}

func TestEnterOnAlreadyPlacedWindowFocusesWithoutLauncher(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	selected := stubPlace(t, currentSession{id: "$2", name: "work"}, true,
		map[string]string{"$2|lab|api|%7": "@12"})
	argv := filepath.Join(t.TempDir(), "ARGV")
	m := windowAttachModel(t, fakeLauncher(t, `printf '%s\n' "$@" > `+argv))
	m.visible[m.cursor].remoteWindowPane = "%7"

	next, cmd := m.Update(wallKey("enter"))

	nm := next.(tuiModel)
	if nm.attach != nil {
		t.Fatal("an attach started for an already placed window")
	}
	if cmd == nil || fmt.Sprintf("%T", cmd()) != "tea.QuitMsg" {
		t.Errorf("cmd = %v, want tea.Quit", cmd)
	}
	if want := []string{"@12"}; !reflect.DeepEqual(*selected, want) {
		t.Errorf("selected = %v, want %v", *selected, want)
	}
	assertNoFile(t, argv)
}

func TestPlacedAttachSelectsPlacedWindowOnSuccess(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	// Not placed yet at decision time; placed once the launcher returns.
	launched := false
	selected := stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	placedWindowFn = func(sessID, host, sess, pane string) (string, bool) {
		if !launched {
			return "", false
		}
		return "@12", sessID == "$2" && host == "lab" && sess == "api" && pane == "%7"
	}
	m, _ := placeModel(t, "")
	m.visible[m.cursor].remoteWindowPane = "%7"

	next, cmd := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	id := nm.attach.id
	done := awaitAttachMsg(t, runBatchAsync(cmd), 5*time.Second, func(msg tea.Msg) bool {
		d, ok := msg.(attachDoneMsg)
		return ok && d.id == id
	})
	launched = true
	_, quit := nm.Update(done)

	if quit == nil || fmt.Sprintf("%T", quit()) != "tea.QuitMsg" {
		t.Errorf("cmd = %v, want tea.Quit", quit)
	}
	if want := []string{"@12"}; !reflect.DeepEqual(*selected, want) {
		t.Errorf("selected = %v, want %v", *selected, want)
	}
}

func TestPlacedAttachErrorLandsInStatusLine(t *testing.T) {
	t.Setenv("OG_PICKER_CURRENT_WINDOW", "@9")
	selected := stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	m, argv := placeModel(t, "echo 'refusing: session is a mirror' >&2\nexit 1")

	nm, _ := runAttachArgv(t, m, argv, "enter")

	if want := "lab/api:2: refusing: session is a mirror — enter to retry"; nm.statusMsg != want {
		t.Errorf("statusMsg = %q, want %q", nm.statusMsg, want)
	}
	if len(*selected) != 0 {
		t.Errorf("selected %v after a failed launch", *selected)
	}
}

func TestPlacedRunGetsLongMirrorBudget(t *testing.T) {
	placed := newAttachRun(attachSpec{host: "lab", sess: "api", into: "work", selector: "@4"})
	if got := placed.budget(phaseMirror); got != placedMirrorBudget {
		t.Errorf("placed mirror budget = %v, want %v", got, placedMirrorBudget)
	}
	if got := newAttachRun(attachSpec{host: "lab", sess: "api"}).budget(phaseMirror); got != attachBudgets[phaseMirror] {
		t.Errorf("dedicated mirror budget = %v, want %v", got, attachBudgets[phaseMirror])
	}
}

func TestBuildAttachCmdPlacedIgnoresRestoreAndNewDir(t *testing.T) {
	cmd := buildAttachCmd(attachSpec{bin: "og-remote-open", host: "lab", sess: "api", into: "work", selector: "@4", window: "@4", restore: true, newDir: "/x"}, time.Second)

	if want := []string{"og-remote-open", "lab", "api", "--into", "work", "--window", "@4"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "OG_REMOTE_RESTORE=") || strings.HasPrefix(e, "OG_REMOTE_NEW_DIR=") {
			t.Errorf("placed open set %s", e)
		}
	}
}

func TestCtrlEIsNoOpOffWindowRowsAndInWall(t *testing.T) {
	stubPlace(t, currentSession{id: "$2", name: "work"}, true, nil)
	m := attachTestModel("unused")
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteHost != "" && it.remoteWindowID == "" })
	next, cmd := m.Update(wallKey("ctrl+e"))
	if nm := next.(tuiModel); nm.attach != nil || cmd != nil {
		t.Errorf("ctrl+e on a non-window row started an attach")
	}

	w := windowAttachModel(t, "unused")
	w.mode = modeWall
	next, cmd = w.Update(wallKey("ctrl+e"))
	if nm := next.(tuiModel); nm.attach != nil || cmd != nil {
		t.Errorf("ctrl+e in wall mode started an attach")
	}
}

func TestFindPlacedWindow(t *testing.T) {
	out := "@1|||||\n@2|1|lab|%3|api\n@5|1|lab|%7|a|b\n@6||lab|%7|api\n"
	cases := []struct {
		host, sess, pane, want string
	}{
		{"lab", "api", "%3", "@2"},
		{"lab", "a|b", "%7", "@5"},
		{"lab", "api", "%7", ""}, // @6 is not placed
		{"other", "api", "%3", ""},
		{"lab", "web", "%3", ""},
	}
	for _, tc := range cases {
		got, ok := findPlacedWindow(out, tc.host, tc.sess, tc.pane)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("findPlacedWindow(%s,%s,%s) = %q,%v, want %q", tc.host, tc.sess, tc.pane, got, ok, tc.want)
		}
	}
}
