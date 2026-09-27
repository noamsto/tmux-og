package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// writeSSHShim shadows ssh on PATH with a script that execs straight into its
// tail command, with no shell wrapper left holding stdout/stderr — so a
// WaitDelay or a context cancel can actually cut it short.
func writeSSHShim(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// Update never launches ssh synchronously. y on a staged kill confirmation
// returns almost instantly with killRun set and a Cmd that has not run yet;
// only once that Cmd is actually driven (as bubbletea's runtime would) does
// ssh fork.
func TestKillUpdateNeverLaunchesSynchronously(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	writeSSHShim(t, "touch "+started+"\nexec sleep 30")

	m := tuiModel{killConfirm: []listItem{killRemoteRow("lab", "mono")}}

	t0 := time.Now()
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'y'})
	if elapsed := time.Since(t0); elapsed >= 250*time.Millisecond {
		t.Fatalf("Update took %v, want < 250ms", elapsed)
	}
	nm := next.(tuiModel)
	if nm.killRun == nil {
		t.Fatal("killRun not set")
	}
	if cmd == nil {
		t.Fatal("expected a Cmd")
	}

	time.Sleep(200 * time.Millisecond)
	assertNoFile(t, started)

	runBatchAsync(cmd)
	waitForFile(t, started, 5*time.Second)
}

// killMsgIsProgressOrDone is the predicate driveKill and the tests below use
// to fish a killProgressMsg or killDoneMsg out of a channel of Cmd output.
func killMsgIsProgressOrDone(msg tea.Msg) bool {
	switch msg.(type) {
	case killProgressMsg, killDoneMsg:
		return true
	default:
		return false
	}
}

// The run emits one killProgressMsg per target, in order, before the final
// killDoneMsg — the model's index/total tracking depends on that ordering.
func TestKillProgressAdvancesThroughTargets(t *testing.T) {
	writeSSHShim(t, "exit 0")
	targets := []listItem{
		killRemoteRow("lab", "one"),
		killRemoteRow("lab", "two"),
		killRemoteRow("lab", "three"),
	}
	m := tuiModel{width: 120, remoteItems: targets}
	cmd := m.beginKill(targets)

	type pair struct{ index, total int }
	var seen []pair
	var labels []string
	for {
		ch := runBatchAsync(cmd)
		msg := awaitAttachMsg(t, ch, 5*time.Second, killMsgIsProgressOrDone)
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(tuiModel)
		if p, ok := msg.(killProgressMsg); ok {
			seen = append(seen, pair{p.progress.index, p.progress.total})
			labels = append(labels, m.killRun.label)
			continue
		}
		break
	}

	want := []pair{{0, 3}, {1, 3}, {2, 3}}
	if len(seen) != len(want) {
		t.Fatalf("progress sequence = %+v, want %+v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("progress[%d] = %+v, want %+v", i, seen[i], want[i])
		}
	}

	// The label must track the target actually being attempted, not stay
	// pinned to targets[0], so a caller mid-batch sees the right row.
	wantLabels := []string{
		remoteRowLabel(targets[0]),
		remoteRowLabel(targets[1]),
		remoteRowLabel(targets[2]),
	}
	if len(labels) != len(wantLabels) {
		t.Fatalf("labels = %+v, want %+v", labels, wantLabels)
	}
	for i := range wantLabels {
		if labels[i] != wantLabels[i] {
			t.Errorf("label[%d] = %q, want %q", i, labels[i], wantLabels[i])
		}
	}
}

// Esc cancels the in-flight target promptly (via WaitDelay/context cancel)
// and the run stops before ever attempting the targets after it.
func TestKillCancelStopsRemainingTargets(t *testing.T) {
	dir := t.TempDir()
	sentinel := func(host string) string { return filepath.Join(dir, host) }
	script := fmt.Sprintf(`case "$*" in
	*t1*) touch %q; sleep 2 ;;
	*t2*) touch %q; sleep 2 ;;
	*t3*) touch %q; sleep 2 ;;
esac
exit 0`, sentinel("t1"), sentinel("t2"), sentinel("t3"))
	writeSSHShim(t, script)

	targets := []listItem{
		killRemoteRow("t1", "mono"),
		killRemoteRow("t2", "mono"),
		killRemoteRow("t3", "mono"),
	}
	m := tuiModel{width: 120, remoteItems: targets}
	cmd := m.beginKill(targets)

	ch := runBatchAsync(cmd)
	msg := awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
		_, ok := msg.(killProgressMsg)
		return ok
	})
	next, cmd := m.Update(msg)
	m = next.(tuiModel)
	waitForFile(t, sentinel("t1"), 2*time.Second)

	next, _ = m.handleKillRunKey("esc")
	m = next.(tuiModel)
	if !m.killRun.cancelling {
		t.Fatal("cancelling not set")
	}

	ch = runBatchAsync(cmd)
	msg = awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
		_, ok := msg.(killDoneMsg)
		return ok
	})
	next, _ = m.Update(msg)
	mm := next.(tuiModel)

	if !strings.Contains(mm.statusMsg, "cancelled") {
		t.Errorf("statusMsg = %q, want it to contain %q", mm.statusMsg, "cancelled")
	}
	if mm.killRun != nil {
		t.Error("killRun not cleared after cancel")
	}
	assertNoFile(t, sentinel("t2"))
	assertNoFile(t, sentinel("t3"))
}

// When a cancel lands before any target is even attempted, killResult.results
// is empty, so the per-item bucketing in finishKill produces no message. The
// status line must still say the kill was cancelled rather than showing
// whatever statusMsg happened to hold beforehand.
func TestKillCancelWithNoAttemptsSurfacesStatus(t *testing.T) {
	targets := []listItem{killRemoteRow("t1", "mono")}
	m := tuiModel{
		width:     120,
		statusMsg: "stale message from before the kill started",
	}
	m.killRun = &killRunState{label: remoteRowLabel(targets[0]), targets: targets, total: 1}

	next, _ := m.finishKill(killResult{outcome: killRunCancelled})
	mm := next.(tuiModel)

	if !strings.Contains(mm.statusMsg, "cancelled") {
		t.Errorf("statusMsg = %q, want it to contain %q", mm.statusMsg, "cancelled")
	}
	if strings.Contains(mm.statusMsg, "stale message") {
		t.Errorf("statusMsg = %q, still holds the pre-kill status", mm.statusMsg)
	}
	if mm.killRun != nil {
		t.Error("killRun not cleared after cancel")
	}
}

// A kill that hangs past remoteProbeTimeout surfaces a sanitized error: the
// raw control chars in a hostile session name never reach statusMsg or the
// rendered hint line.
func TestKillTimeoutSurfacesSanitizedError(t *testing.T) {
	writeSSHShim(t, "exec sleep 5")
	item := killRemoteRow("lab", "sess\x1b[31mX")
	m := tuiModel{width: 120, killConfirm: []listItem{item}}

	next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y'})
	mm := driveKill(t, next, cmd)

	if !strings.Contains(mm.statusMsg, "unreachable") {
		t.Errorf("statusMsg = %q, want it to contain %q", mm.statusMsg, "unreachable")
	}
	if strings.Contains(mm.statusMsg, "\x1b") {
		t.Errorf("statusMsg leaked a raw escape: %q", mm.statusMsg)
	}
	if hints := mm.renderHints(); strings.Contains(hints, "\x1b[31m") {
		t.Errorf("renderHints leaked a raw escape: %q", hints)
	}
}

// Mirrors killRemoteSessions's old bucketing: success and "already gone" both
// forget their row, an unreachable host keeps its row, and the hint line
// joins every message.
func TestKillOutcomesMixedSuccessAndFailure(t *testing.T) {
	useRemoteCache(t)
	seedRemoteCache(t, "hostok", time.Now(), "s1")
	seedRemoteCache(t, "hostgone", time.Now(), "s2")
	seedRemoteCache(t, "hostbad", time.Now(), "s3")
	writeSSHShim(t, `case "$*" in
	*hostok*) exit 0 ;;
	*hostgone*) exit 1 ;;
	*hostbad*) exit 255 ;;
esac
exit 0`)

	ok := killRemoteRow("hostok", "s1")
	gone := killRemoteRow("hostgone", "s2")
	bad := killRemoteRow("hostbad", "s3")
	targets := []listItem{ok, gone, bad}
	m := tuiModel{width: 120, remoteItems: targets}
	cmd := m.beginKill(targets)
	mm := driveKill(t, m, cmd)

	if len(mm.remoteItems) != 1 || mm.remoteItems[0].remoteHost != "hostbad" {
		t.Errorf("remoteItems after mixed kill = %+v, want only hostbad", mm.remoteItems)
	}
	if !strings.Contains(mm.statusMsg, "already gone") {
		t.Errorf("statusMsg = %q, want an already-gone hint", mm.statusMsg)
	}
	if !strings.Contains(mm.statusMsg, "unreachable") {
		t.Errorf("statusMsg = %q, want an unreachable hint", mm.statusMsg)
	}
}

// Every key but esc/ctrl+c is a no-op while a kill batch is in flight, and
// mouse events are ignored the same way they are during an attach.
func TestKillKeysAndMouseIgnoredDuringKillRun(t *testing.T) {
	run := newKillRun([]listItem{killRemoteRow("lab", "mono")})
	base := tuiModel{
		mode:    modeList,
		cursor:  1,
		visible: remoteFixture(),
		query:   "q",
		marked:  map[string]bool{"remote:lab:mono": true},
		killRun: &killRunState{id: 7, run: run, total: 1, label: "lab/mono"},
	}
	wantCursor := base.cursor

	for _, key := range []string{"enter", "j", "ctrl+x", "tab", "ctrl+t", "down"} {
		next, cmd := base.Update(wallKey(key))
		nm := next.(tuiModel)
		if cmd != nil {
			t.Errorf("%s: expected nil cmd, got %v", key, cmd)
		}
		if nm.killRun == nil || nm.killRun.id != 7 {
			t.Errorf("%s: killRun.id changed: %+v", key, nm.killRun)
		}
		if nm.query != "q" || nm.cursor != wantCursor || !nm.marked["remote:lab:mono"] {
			t.Errorf("%s: model mutated: query=%q cursor=%d marked=%v", key, nm.query, nm.cursor, nm.marked)
		}
	}

	next, cmd := base.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	nm := next.(tuiModel)
	if cmd != nil || nm.cursor != wantCursor {
		t.Errorf("wheel handled during kill run: cmd=%v cursor=%d", cmd, nm.cursor)
	}

	next, cmd = base.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 3})
	nm = next.(tuiModel)
	if cmd != nil || nm.cursor != wantCursor {
		t.Errorf("click handled during kill run: cmd=%v cursor=%d", cmd, nm.cursor)
	}
}

// A message from a superseded run (or an idle tick) is dropped.
func TestKillStaleMessagesIgnored(t *testing.T) {
	run := newKillRun([]listItem{killRemoteRow("lab", "mono")})
	base := tuiModel{
		query:   "q",
		killRun: &killRunState{id: 5, run: run, total: 1, label: "lab/mono"},
	}

	for _, msg := range []tea.Msg{
		killProgressMsg{id: 4, progress: killProgress{index: 0, total: 1}},
		killDoneMsg{id: 4, result: killResult{outcome: killRunDone}},
		killTickMsg{id: 4},
	} {
		next, cmd := base.Update(msg)
		nm := next.(tuiModel)
		if cmd != nil {
			t.Errorf("%#v: expected nil cmd for a stale id", msg)
		}
		if nm.killRun == nil || nm.killRun.index != 0 || nm.killRun.id != 5 {
			t.Errorf("%#v: model changed: %+v", msg, nm.killRun)
		}
	}

	idle := tuiModel{}
	next, cmd := idle.Update(killTickMsg{id: 1})
	if cmd != nil {
		t.Error("expected nil cmd for a tick while idle")
	}
	if next.(tuiModel).killRun != nil {
		t.Error("killRun set from a stray tick")
	}
}
