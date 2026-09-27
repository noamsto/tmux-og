package main

import (
	"os"
	"path/filepath"
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

// 9(a): Update never launches synchronously. Enter returns almost instantly
// with attach set and a Cmd that has not run yet; only once that Cmd is
// actually driven (as bubbletea's runtime would) does the launcher fork, and
// cancel rolls all the way through to a cleared attach.
func TestAttachUpdateNeverLaunchesSynchronously(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "STARTED")
	bin := fakeLauncher(t, "touch "+started+"\nprintf 'connect\\n' >&3\nsleep 30")
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

	msg = awaitAttachMsg(t, ch2, 5*time.Second, func(m tea.Msg) bool {
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

// 9(b): every key but esc/ctrl+c is a no-op while an attach is in flight, so
// a second concurrent attach can never start. The base model's cursor sits on
// a real Remote-section row (remote:lab:mono) precisely so an un-gated enter
// would also return nil here — with no rows, it couldn't tell a real gate from
// an empty list — and instead exercises activateCurrent, returning a Cmd and a
// new attach.id.
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

// 9(c): esc starts cancelling; a second ctrl+c while cancelling ends the TUI,
// but runTUI still waits (bounded) for the launcher's rollback before returning.
func TestAttachEscCancelsCtrlCQuitsWhileCancelling(t *testing.T) {
	run := newAttachRun(attachSpec{bin: fakeLauncher(t, "sleep 30"), host: "lab", sess: "mono"})
	m := tuiModel{attach: &attachState{id: 1, run: run, host: "lab", sess: "mono", label: "lab/mono", phase: phaseConnect}}

	next, cmd := m.Update(wallKey("esc"))
	nm := next.(tuiModel)
	if cmd != nil {
		t.Errorf("esc returned a Cmd, want nil")
	}
	if !nm.attach.cancelling {
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
	if !nm2.attach.cancelling {
		t.Fatal("cancelling not set by the first ctrl+c")
	}
	if run2.ctx.Err() == nil {
		t.Error("run's ctx not cancelled")
	}

	if _, cmd = nm2.Update(wallKey("ctrl+c")); cmd == nil {
		t.Fatal("expected tea.Quit on the second ctrl+c while cancelling")
	}
}

// 9(d): mouse clicks and wheel events are ignored while an attach is in
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

// 9(e): the four outcomes finishAttach can land on.
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

// 9(f): a failure or timeout leaves the cursor where it was, so Enter retries
// the same row — as a fresh attempt, with a higher id than the one it replaces.
func TestAttachRetryStartsNewAttachWithHigherID(t *testing.T) {
	m := attachTestModel(fakeLauncher(t, "sleep 30"))
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.target == "remote:lab:mono" })

	next, cmd := m.Update(wallKey("enter"))
	nm := next.(tuiModel)
	if cmd == nil {
		t.Fatal("expected a Cmd")
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

// 9(g): a message from a superseded attempt (or an idle tick) is dropped.
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

// 9(h): the status line always fits exactly m.width, and at a width wide
// enough to show it, carries the phase, label and cancel hint — sanitized,
// even when the remote-derived label carries an escape sequence.
func TestAttachStatusLineRendering(t *testing.T) {
	m := tuiModel{width: 80}
	m.beginAttach(listItem{remoteHost: "lab", remoteSess: "mono"}, nil)
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

// 9(i): opening several marked sessions starts only the first as the attach;
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
