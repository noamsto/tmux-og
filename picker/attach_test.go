package main

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeStatusText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"csi", "\x1b[31mred\x1b[0m", "red"},
		{"osc bel", "a\x1b]0;t\x07b", "ab"},
		{"osc st", "a\x1b]8;;u\x1b\\b", "ab"},
		{"two-byte esc", "a\x1bcb", "ab"},
		{"c1 csi", "a\u009b31mb", "a31mb"},
		{"bidi", "a\u202eb\u2066c", "abc"},
		{"bidi rlm", "a\u200fb", "ab"},
		{"bidi alm", "a\u061cb", "ab"},
		{"bidi lrm", "a\u200eb", "ab"},
		{"whitespace collapse", "a\n\t b", "a b"},
		{"trim", "  a  ", "a"},
		{"cap", strings.Repeat("x", 300), strings.Repeat("x", 200)},
		{"cap counts collapsed spaces", strings.Repeat("ab \n", 100), strings.Repeat("ab ", 66) + "ab"},
		{"plain unicode", "héllo ✓", "héllo ✓"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeStatusText(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeStatusText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > 200 {
				t.Errorf("len = %d runes, want <= 200", n)
			}
		})
	}
}

// shrinkAttachTimings sets a short kill grace and overrides the given phase
// budgets for one test, restoring both afterwards.
func shrinkAttachTimings(t *testing.T, budgets map[attachPhase]time.Duration) {
	t.Helper()
	oldBudgets, oldGrace := attachBudgets, attachKillGrace
	t.Cleanup(func() { attachBudgets, attachKillGrace = oldBudgets, oldGrace })
	attachBudgets = maps.Clone(oldBudgets)
	maps.Copy(attachBudgets, budgets)
	attachKillGrace = 500 * time.Millisecond
}

func fakeLauncher(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "og-remote-open")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeAttachRun(t *testing.T, body string) *attachRun {
	t.Helper()
	return newAttachRun(attachSpec{bin: fakeLauncher(t, body), host: "lab", sess: "mono"})
}

// drain runs r to completion and returns every phase it reported, in order.
func drain(t *testing.T, r *attachRun) ([]attachPhase, attachResult) {
	t.Helper()
	go r.run()
	var phases []attachPhase
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case p := <-r.progress:
			phases = append(phases, p)
		case <-r.done:
			done = true
		case <-deadline:
			t.Fatal("attach run did not finish within 10s")
		}
	}
	for {
		select {
		case p := <-r.progress:
			phases = append(phases, p)
		default:
			return phases, r.result()
		}
	}
}

func awaitProgress(t *testing.T, r *attachRun) attachPhase {
	t.Helper()
	select {
	case p := <-r.progress:
		return p
	case <-r.done:
		t.Fatalf("run finished before any progress: %+v", r.result())
	case <-time.After(10 * time.Second):
		t.Fatal("no progress within 10s")
	}
	return ""
}

func awaitDone(t *testing.T, r *attachRun, within time.Duration) attachResult {
	t.Helper()
	select {
	case <-r.done:
		return r.result()
	case <-time.After(within):
		t.Fatalf("run not done within %v", within)
	}
	return attachResult{}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (err=%v): a launcher descendant outlived the cancel", path, err)
	}
}

func TestAttachReportsPhasesInOrder(t *testing.T) {
	shrinkAttachTimings(t, nil)
	phases, res := drain(t, fakeAttachRun(t, `printf 'connect\n' >&3; printf 'mirror\n' >&3`))
	if want := []attachPhase{phaseConnect, phaseMirror}; !slices.Equal(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
	if res.outcome != attachOK {
		t.Errorf("result = %+v, want attachOK", res)
	}
}

// A line written after the launcher exited (a descendant still holding fd 3)
// is still delivered: run drains the pipe to EOF before finishing.
func TestAttachDrainsLinesAfterExit(t *testing.T) {
	shrinkAttachTimings(t, nil)
	phases, res := drain(t, fakeAttachRun(t, `printf 'connect\n' >&3; (sleep 0.1; printf 'mirror\n' >&3) 2>/dev/null &`))
	if want := []attachPhase{phaseConnect, phaseMirror}; !slices.Equal(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
	if res.outcome != attachOK {
		t.Errorf("result = %+v, want attachOK", res)
	}
}

func TestAttachIgnoresUnknownLines(t *testing.T) {
	shrinkAttachTimings(t, nil)
	phases, res := drain(t, fakeAttachRun(t, `printf 'bogus\nlaunch\nconnect\n' >&3`))
	if want := []attachPhase{phaseConnect}; !slices.Equal(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
	if res.outcome != attachOK {
		t.Errorf("result = %+v, want attachOK", res)
	}
}

// groupChildBody backgrounds a grandchild that writes mark after 1s. It holds
// neither stderr nor fd 3, so nothing but a TERM to the whole group stops it:
// Wait returns as soon as the leader dies, before any KILL escalation.
func groupChildBody(mark string) string {
	return `sh -c "sleep 1; echo x > '` + mark + `'" >/dev/null 2>&1 3>&- & printf 'connect\n' >&3; wait`
}

func TestAttachCancelKillsWholeGroup(t *testing.T) {
	shrinkAttachTimings(t, nil)
	attachKillGrace = 5 * time.Second
	mark := filepath.Join(t.TempDir(), "MARK")
	r := fakeAttachRun(t, groupChildBody(mark))
	go r.run()
	if p := awaitProgress(t, r); p != phaseConnect {
		t.Fatalf("first phase = %q, want connect", p)
	}
	r.cancel()
	res := awaitDone(t, r, 3*time.Second)
	if res.outcome != attachCancelled || res.phase != phaseConnect {
		t.Errorf("result = %+v, want attachCancelled in connect", res)
	}
	time.Sleep(1500 * time.Millisecond)
	assertNoFile(t, mark)
}

// orphanPipeBody leaves a background descendant that ignores TERM and holds the
// phase pipe's write end (fd 3). Its stderr is /dev/null, so cmd.Wait returns
// promptly once the launcher itself dies; only the pipe stays open.
func orphanPipeBody(ready string) string {
	return `printf 'connect\n' >&3; ` +
		`sh -c 'trap "" TERM; touch "` + ready + `"; sleep 5' 2>/dev/null & ` +
		`sleep 30`
}

// A descendant holding the phase pipe must not add a full grace to the done
// message, so a cancel stays bounded by the kill itself.
func TestAttachCancelBoundedByOrphanHoldingPipe(t *testing.T) {
	shrinkAttachTimings(t, nil)
	attachKillGrace = 2 * time.Second
	ready := filepath.Join(t.TempDir(), "READY")
	r := fakeAttachRun(t, orphanPipeBody(ready))
	go r.run()
	if p := awaitProgress(t, r); p != phaseConnect {
		t.Fatalf("first phase = %q, want connect", p)
	}
	waitForFile(t, ready, 5*time.Second)
	start := time.Now()
	r.cancel()
	res := awaitDone(t, r, 2*attachKillGrace)
	if got := time.Since(start); got >= attachKillGrace {
		t.Fatalf("cancel took %v; an orphan holding the pipe must not add a full grace", got)
	}
	if res.outcome != attachCancelled {
		t.Errorf("result = %+v, want attachCancelled", res)
	}
}

func TestAttachCancelEscalatesToKill(t *testing.T) {
	shrinkAttachTimings(t, nil)
	r := fakeAttachRun(t, `trap '' TERM; printf 'connect\n' >&3; while :; do sleep 0.1; done`)
	go r.run()
	awaitProgress(t, r)
	r.cancel()
	if res := awaitDone(t, r, attachKillGrace+2*time.Second); res.outcome != attachCancelled {
		t.Errorf("result = %+v, want attachCancelled", res)
	}
}

func TestAttachTimeoutNamesPhase(t *testing.T) {
	shrinkAttachTimings(t, map[attachPhase]time.Duration{phaseConnect: 200 * time.Millisecond})
	r := fakeAttachRun(t, `printf 'connect\n' >&3; sleep 30`)
	go r.run()
	res := awaitDone(t, r, 3*time.Second)
	if res.outcome != attachTimedOut || res.phase != phaseConnect || res.budget != 200*time.Millisecond {
		t.Errorf("result = %+v, want attachTimedOut in connect after 200ms", res)
	}
}

func TestAttachTimeoutBeforeFirstLine(t *testing.T) {
	shrinkAttachTimings(t, map[attachPhase]time.Duration{phaseLaunch: 200 * time.Millisecond})
	r := fakeAttachRun(t, `sleep 30`)
	go r.run()
	res := awaitDone(t, r, 3*time.Second)
	if res.outcome != attachTimedOut || res.phase != phaseLaunch || res.budget != 200*time.Millisecond {
		t.Errorf("result = %+v, want attachTimedOut in launch after 200ms", res)
	}
}

// A phase missing from the table must still be bounded, never "no timeout".
func TestAttachBudgetFallsBackToLaunch(t *testing.T) {
	shrinkAttachTimings(t, map[attachPhase]time.Duration{phaseLaunch: 200 * time.Millisecond})
	delete(attachBudgets, phaseConnect)
	r := fakeAttachRun(t, `printf 'connect\n' >&3; sleep 30`)
	go r.run()
	res := awaitDone(t, r, 3*time.Second)
	if res.outcome != attachTimedOut || res.phase != phaseConnect || res.budget != 200*time.Millisecond {
		t.Errorf("result = %+v, want attachTimedOut in connect after the 200ms launch budget", res)
	}
}

func TestAttachFailureMessageSanitized(t *testing.T) {
	shrinkAttachTimings(t, nil)
	_, res := drain(t, fakeAttachRun(t, `printf '\033[2Jout\n'; printf 'noise\nog-remote-open: \033]0;x\007bad \033[31mthing\n' >&2; exit 1`))
	if res.outcome != attachFailed || res.msg != "og-remote-open: bad thing" {
		t.Errorf("result = %+v, want attachFailed with %q", res, "og-remote-open: bad thing")
	}
}

func TestAttachFailureWithoutStderrUsesExitError(t *testing.T) {
	shrinkAttachTimings(t, nil)
	_, res := drain(t, fakeAttachRun(t, `exit 3`))
	if res.outcome != attachFailed || res.msg != "exit status 3" {
		t.Errorf("result = %+v, want attachFailed with %q", res, "exit status 3")
	}
}

// Past the launcher's commit point its trap exits 0: a complete mirror exists,
// so a cancel that lost that race is still a success.
func TestAttachExitZeroAfterCancelIsSuccess(t *testing.T) {
	shrinkAttachTimings(t, nil)
	r := fakeAttachRun(t, `trap 'exit 0' TERM; printf 'mirror\n' >&3; sleep 30 & wait`)
	go r.run()
	awaitProgress(t, r)
	r.cancel()
	if res := awaitDone(t, r, 3*time.Second); res.outcome != attachOK {
		t.Errorf("result = %+v, want attachOK", res)
	}
}

func TestAttachCancelBeforeRunNeverForks(t *testing.T) {
	shrinkAttachTimings(t, nil)
	mark := filepath.Join(t.TempDir(), "MARK")
	r := fakeAttachRun(t, `touch '`+mark+`'`)
	r.cancel()
	r.run()
	if res := r.result(); res.outcome != attachCancelled {
		t.Errorf("result = %+v, want attachCancelled", res)
	}
	assertNoFile(t, mark)
}

func TestAttachStartFailure(t *testing.T) {
	shrinkAttachTimings(t, nil)
	r := newAttachRun(attachSpec{bin: filepath.Join(t.TempDir(), "missing"), host: "lab"})
	_, res := drain(t, r)
	if res.outcome != attachFailed || res.msg == "" {
		t.Errorf("result = %+v, want attachFailed with a message", res)
	}
}

func TestBuildAttachCmd(t *testing.T) {
	const grace = 123 * time.Millisecond
	cmd := buildAttachCmd(attachSpec{bin: "/x/og-remote-open", host: "lab", sess: "mono"}, grace)
	if want := []string{"/x/og-remote-open", "lab", "mono"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	if cmd.WaitDelay != grace {
		t.Errorf("WaitDelay = %v, want %v", cmd.WaitDelay, grace)
	}
	for _, kv := range []string{"OG_REMOTE_OPEN_PROGRESS_FD=3", "SSH_ASKPASS_REQUIRE=never"} {
		if !slices.Contains(cmd.Env, kv) {
			t.Errorf("env lacks %s", kv)
		}
	}
	if slices.Contains(cmd.Env, "OG_REMOTE_RESTORE=1") {
		t.Error("env has OG_REMOTE_RESTORE=1 without restore")
	}
	if cmd.Stdout != nil {
		t.Error("stdout is wired; remote output would paint over the popup")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("launcher not started in its own session")
	}

	cmd = buildAttachCmd(attachSpec{bin: "/x/og-remote-open", host: "lab", restore: true}, attachKillGrace)
	if want := []string{"/x/og-remote-open", "lab"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, "OG_REMOTE_RESTORE=1") {
		t.Error("env lacks OG_REMOTE_RESTORE=1 with restore")
	}
}

func TestBuildAttachCmdWindowIndex(t *testing.T) {
	cmd := buildAttachCmd(attachSpec{bin: "/x/og-remote-open", host: "lab", sess: "api", window: 3}, attachKillGrace)
	if want := []string{"/x/og-remote-open", "lab", "api", "3"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	cmd = buildAttachCmd(attachSpec{bin: "/x/og-remote-open", host: "lab", sess: "api"}, attachKillGrace)
	if want := []string{"/x/og-remote-open", "lab", "api"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args without window = %v, want %v", cmd.Args, want)
	}
}

// The launcher never reaches PATH, so only @remote_open_bin resolves it.
func TestRemoteOpenBinUsesOption(t *testing.T) {
	t.Setenv("REMOTE_OPEN_BIN", "")
	if got := remoteOpenBin(map[string]string{"@remote_open_bin": "/x/og-remote-open"}); got != "/x/og-remote-open" {
		t.Errorf("remoteOpenBin = %q, want the option value", got)
	}
}

func TestAttachSupervisorStopCancelsAndWaits(t *testing.T) {
	shrinkAttachTimings(t, nil)
	mark := filepath.Join(t.TempDir(), "MARK")
	r := fakeAttachRun(t, groupChildBody(mark))
	sup := &attachSupervisor{}
	sup.track(r)
	go r.run()
	awaitProgress(t, r)

	start := time.Now()
	sup.stop(3 * time.Second)
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("stop took %v, want < 3s", elapsed)
	}
	select {
	case <-r.done:
	default:
		t.Fatal("stop returned before the run finished")
	}
	if res := r.result(); res.outcome != attachCancelled {
		t.Errorf("result = %+v, want attachCancelled", res)
	}
	time.Sleep(1500 * time.Millisecond)
	assertNoFile(t, mark)
}

// A quit can land before the Cmd goroutine ever calls run: stop must not
// wait out its bound, and the late run must not fork.
func TestAttachSupervisorStopBeforeRun(t *testing.T) {
	shrinkAttachTimings(t, nil)
	mark := filepath.Join(t.TempDir(), "MARK")
	r := fakeAttachRun(t, `touch '`+mark+`'`)
	sup := &attachSupervisor{}
	sup.track(r)

	start := time.Now()
	sup.stop(3 * time.Second)
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("stop took %v for a run that never started", elapsed)
	}
	r.run()
	if res := r.result(); res.outcome != attachCancelled {
		t.Errorf("result = %+v, want attachCancelled", res)
	}
	assertNoFile(t, mark)
}

func TestAttachSupervisorNil(t *testing.T) {
	var nilSup *attachSupervisor
	nilSup.track(newAttachRun(attachSpec{}))
	nilSup.stop(time.Second)

	start := time.Now()
	(&attachSupervisor{}).stop(3 * time.Second)
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("stop with nothing tracked took %v", elapsed)
	}
}
