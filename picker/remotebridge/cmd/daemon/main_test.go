package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/daemon"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

// TestReflowRunShellArgsSurvivesFormatInjection exercises a #(...)-bearing
// local session name through a real tmux run-shell call. run-shell
// format-expands its whole shell-command string before /bin/sh sees it, so
// concatenating shellQuote(reflowBin)+" --force "+shellQuote(localSess) (the
// pre-#368 construction) let tmux's format scanner consume the embedded
// "#(touch marker)" as a job — the recorder script received "zzx", not the
// literal session name. The argv-equality check below is the load-bearing
// assertion: it deterministically fails against that construction. The
// marker-absence check is best-effort only — run-shell's #(...) job spawns
// asynchronously, so it isn't a reliable pre-fix/post-fix discriminator on
// its own.
func TestReflowRunShellArgsSurvivesFormatInjection(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also adds pkgs.tmux to nativeBuildInputs — so under `nix flake
		// check` a missing tmux means that input was pruned, not that this is a
		// dev machine. Fail instead of silently skipping this regression check.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	record := filepath.Join(dir, "record")

	// Stands in for tmux-reflow-windows: records the argv it was invoked
	// with so the test can assert the malicious session name arrived intact.
	script := filepath.Join(dir, "record.sh")
	scriptBody := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(record) + "\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}

	maliciousSess := "zz#(touch " + marker + ")x"

	// tmux never unlinks a socket file on exit, so a server started in the
	// ambient TMUX_TMPDIR leaves a dead entry in the user's runtime dir on
	// every run. A private short dir (the path is capped at ~108 bytes) keeps
	// the leftover inside what the cleanup removes.
	tmpdir, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	const socket = "s"
	env := append(os.Environ(), "TMUX_TMPDIR="+tmpdir)

	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "t1")
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command("tmux", "-L", socket, "kill-server")
		stop.Env = env
		_ = stop.Run()
	})

	runShell := exec.Command("tmux", append([]string{"-L", socket}, reflowRunShellArgs(script, maliciousSess)...)...)
	runShell.Env = env
	if out, err := runShell.CombinedOutput(); err != nil {
		t.Fatalf("run-shell: %v: %s", err, out)
	}

	// run-shell's job is asynchronous and this waits on a real tmux server, so
	// the budget is a stall detector, not a race to beat: a parallel `go test
	// ./...` on a loaded builder starves it well past 3s (the recorder then
	// never appears and the failure reads as a broken fix, not a slow one).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(record); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("recorder script never ran: %v", err)
	}
	want := "--force\n" + maliciousSess + "\n"
	if string(got) != want {
		t.Fatalf("recorder argv = %q, want %q", got, want)
	}

	// Best-effort: see the doc comment above on why this isn't the
	// discriminating assertion.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("embedded #(...) job executed — format-layer injection not blocked")
	}
}

// The keepalive options are the whole of #471: without them ssh never notices a
// host that died without closing the connection, so the control stream never
// reaches EOF and the daemon never runs the teardown it already has. Nothing
// else in the suite would fail if they were dropped, hence this test.
func TestSSHControlArgsCarryKeepalives(t *testing.T) {
	args := sshControlArgs("/tmp/ctl.sock", "tp-g6", "/run/user/1000", "xterm-kitty", "truecolor", "iTerm.app",
		"tmux-og", []string{"tmux"})
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"ServerAliveInterval=15",
		"ServerAliveCountMax=4",
		"ControlMaster=auto",
		"ControlPath=/tmp/ctl.sock",
		"ControlPersist=no",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}

	// The keepalive options must precede the host: ssh stops parsing options at
	// the first non-option argument, so one placed after it would be handed to
	// the remote shell as a command word instead.
	hostAt := slices.Index(args, "tp-g6")
	if hostAt < 0 {
		t.Fatalf("host missing from %q", joined)
	}
	for i, a := range args {
		if strings.HasPrefix(a, "ServerAlive") && i > hostAt {
			t.Errorf("%s sits after the host at %d; ssh would pass it to the remote", a, hostAt)
		}
	}

	// TERM/COLORTERM/TERM_PROGRAM (#543) are part of the remote `env ...`
	// command, so they must sit after the host (an ssh option there would be
	// misparsed as a remote command word) and before the trailing attach args.
	envAt := slices.Index(args, "env")
	if envAt < 0 || envAt < hostAt {
		t.Fatalf("env token missing or before the host in %q", joined)
	}
	attachAt := slices.Index(args, "-C")
	if attachAt < 0 {
		t.Fatalf("-C attach-session missing from %q", joined)
	}
	for _, tc := range []struct{ name, value string }{
		{"TERM", "xterm-kitty"},
		{"COLORTERM", "truecolor"},
		{"TERM_PROGRAM", "iTerm.app"},
	} {
		// Mirrors sshControlArgs' own "NAME="+shellQuote(value) construction.
		want := tc.name + "=" + shellQuote(tc.value)
		at := slices.Index(args, want)
		if at < 0 {
			t.Errorf("missing %q in %q", want, joined)
			continue
		}
		if at < envAt || at > attachAt {
			t.Errorf("%s at %d, want between env (%d) and -C attach-session (%d)", want, at, envAt, attachAt)
		}
	}

	// The session is the attach target and must stay one token even with spaces.
	if got := args[len(args)-1]; got != shellQuote("tmux-og") {
		t.Errorf("last arg = %q, want the shell-quoted session", got)
	}
}

// TestSSHControlArgsOmitsEmptyColortermAndTermProgram: an empty local value
// (no client, or a terminal that never set it) must not ship a bogus/empty
// env assignment to the remote.
func TestSSHControlArgsOmitsEmptyColortermAndTermProgram(t *testing.T) {
	args := sshControlArgs("/tmp/ctl.sock", "tp-g6", "/run/user/1000", "", "", "", "tmux-og", []string{"tmux"})
	joined := strings.Join(args, " ")
	for _, unwanted := range []string{"TERM=", "COLORTERM=", "TERM_PROGRAM="} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, joined)
		}
	}
}

// TestNewSSHDialCmdReadsDesiredAtDialTime is the regression test for the
// Asserts on the built *exec.Cmd's argv, never on the cell: reading the cell
// alone cannot tell "read every time" from "read once and cached", so an
// assertion there would pass a recipe that captured Desired at build time and
// then carried one termname for the life of the daemon.
func TestNewSSHDialCmdReadsDesiredAtDialTime(t *testing.T) {
	view := &daemon.Viewing{}
	view.Seed("xterm-kitty")

	cmd := newSSHDialCmd("ssh", "tp-g6", "/run/user/1000", "/tmp/ctl-1.sock", "truecolor", "iTerm.app", "tmux-og", []string{"tmux"}, view)
	want := "TERM=" + shellQuote("xterm-kitty")
	if slices.Index(cmd.Args, want) < 0 {
		t.Fatalf("first dial argv %v missing %q", cmd.Args, want)
	}

	view.SetDesired("foot")
	cmd = newSSHDialCmd("ssh", "tp-g6", "/run/user/1000", "/tmp/ctl-2.sock", "truecolor", "iTerm.app", "tmux-og", []string{"tmux"}, view)
	want = "TERM=" + shellQuote("foot")
	if slices.Index(cmd.Args, want) < 0 {
		t.Fatalf("dial after SetDesired argv %v missing %q — still carrying the old termname", cmd.Args, want)
	}
}

// TestLocalCtlCmdEnvCarriesDesiredTermAndInheritsEnvironment covers the
// --test-local / no-ssh branches' equivalent of the ssh branch's argv: TERM
// rides cmd.Env instead, and it must be an append to os.Environ(), never a
// wholesale replacement — a bare assignment would drop PATH/TMUX_TMPDIR/HOME
// and break the offline bats harness these two branches exist to run under.
func TestLocalCtlCmdEnvCarriesDesiredTermAndInheritsEnvironment(t *testing.T) {
	view := &daemon.Viewing{}
	view.Seed("xterm-kitty")

	env := localCtlCmdEnv(view)
	if !slices.Contains(env, "TERM=xterm-kitty") {
		t.Fatalf("env %v missing the seeded TERM", env)
	}
	if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
		t.Fatalf("env %v dropped the inherited PATH — cmd.Env must append, not replace", env)
	}

	view.SetDesired("foot")
	env = localCtlCmdEnv(view)
	if !slices.Contains(env, "TERM=foot") {
		t.Fatalf("env %v did not follow SetDesired", env)
	}
}

// TestNewGraphicsGatesOnRelayOnBothTransportBranches is C7's only guard
// against the trap the plan calls out by name: NewGraphics must build the
// proxy with graphics.NewRelay — never fall back to the relay-off
// graphics.New — on BOTH the ssh branch (ctlSock != "") and the
// --test-local/-ssh "" branch (ctlSock == ""), gated by the same Relay value
// on each. Leaving the ssh branch on plain graphics.New would ship sixel
// relay working only under --test-local, i.e. dead on the only transport a
// real user has, and the bats proof (which runs --test-local exclusively)
// would not catch it. Asserted through Filter's forwarding behaviour, never
// by reaching into the proxy's unexported fields.
func TestNewGraphicsGatesOnRelayOnBothTransportBranches(t *testing.T) {
	const sixel = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
	sixelOn := graphics.NewRelayFromClient(true, "bpaste,sixel")
	sixelOff := graphics.NewRelayFromClient(false, "bpaste")

	for _, tc := range []struct {
		name    string
		ctlSock string // "" is the --test-local/-ssh "" branch; non-empty is the ssh branch
	}{
		{"test-local transport (no ctl socket)", ""},
		{"ssh transport (ctl socket set)", "/tmp/does-not-need-to-exist.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctlSockFn := func() string { return tc.ctlSock }
			newProxy := newGraphics(tc.ctlSock, ctlSockFn, "host", t.TempDir(), 1<<20, graphics.NewRelaySource(sixelOn), graphics.DefaultRasterHold)
			if got := string(newProxy("").Filter([]byte(sixel))); got != sixel {
				t.Errorf("gate on: Filter(sixel) = %q, want the sixel forwarded", got)
			}

			newProxyOff := newGraphics(tc.ctlSock, ctlSockFn, "host", t.TempDir(), 1<<20, graphics.NewRelaySource(sixelOff), graphics.DefaultRasterHold)
			if got := string(newProxyOff("").Filter([]byte(sixel))); got != "" {
				t.Errorf("gate off: Filter(sixel) = %q, want it dropped", got)
			}
		})
	}
}

// TestPasteUploadArgs pins the upload's ssh argv: it must ride the control
// connection's ControlMaster (-S), never allocate a tty (-T), and hand the
// store script to sh -c as ONE quoted element — the remote login shell is
// fish, which would otherwise parse (and mangle) it. The extension rides as
// a separate argv element ($1), never interpolated into the script.
func TestPasteUploadArgs(t *testing.T) {
	args := pasteUploadArgs("ssh", "/tmp/ctl.sock", "tp-g6", "png")
	joined := strings.Join(args, " ")

	for _, want := range []string{"-S /tmp/ctl.sock", "-T tp-g6", "-- sh -c"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	// The script must be single-quoted whole, so fish never parses it.
	scriptAt := slices.Index(args, shellQuote(remoteStoreScript))
	if scriptAt < 0 {
		t.Fatalf("store script not shell-quoted as one element in %q", joined)
	}
	if args[scriptAt+1] != "_" || args[scriptAt+2] != "png" {
		t.Errorf("argv after script = %q, want _ <ext>", args[scriptAt+1:])
	}
	if strings.Contains(remoteStoreScript, "'") {
		t.Error("store script contains a single quote; shellQuote only wraps, it cannot escape one inside sh -c")
	}
}

// TestTransportStartAfterStopEndsTheChild makes transport.start's race
// deterministic: a stop that lands before a child is published must still end
// it, or the ssh child outlives the daemon holding the ControlMaster open.
func TestTransportStartAfterStopEndsTheChild(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	tr.stop() // the handler fires with nothing yet published

	c := startChild(t, tr, exec.Command("sleep", "60"))
	waitEnded(t, c, "a transport started after stop was left running")
}

// TestTransportStopEndsTheCurrentChild is the ordinary path: the handler ends
// whatever is published when the signal arrives.
func TestTransportStopEndsTheCurrentChild(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	c := startChild(t, tr, exec.Command("sleep", "60"))
	tr.stop()
	waitEnded(t, c, "stop left the current transport running")
}

// TestTransportCurrentPathReturnsTheNewestOpenChild is R7's overlap: two
// masters alive at once, and the accessor every consumer reads through must
// serve the new, connected one — never the one a replacement is retiring.
func TestTransportCurrentPathReturnsTheNewestOpenChild(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.sock")
	newPath := filepath.Join(dir, "new.sock")
	startChildAt(t, tr, exec.Command("sleep", "60"), oldPath)
	startChildAt(t, tr, exec.Command("sleep", "60"), newPath)

	if got := tr.currentPath(); got != newPath {
		t.Fatalf("currentPath = %q, want the newer child's path %q", got, newPath)
	}
}

// TestTransportCurrentPathFallsBackAfterAnAbortedDial is R7's abort case: a
// replacement whose identity check fails is closed with the old connection
// never touched. The accessor must fall back to the survivor rather than
// staying on the dead dial — the bug R9 exists to retire, since a consumer
// stuck there passes ssh a -S that no longer exists and multiplexing silently
// stops with no tty to reauthenticate on.
func TestTransportCurrentPathFallsBackAfterAnAbortedDial(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.sock")
	newPath := filepath.Join(dir, "new.sock")
	startChildAt(t, tr, exec.Command("sleep", "60"), oldPath)
	aborted := startChildAt(t, tr, exec.Command("sleep", "60"), newPath)

	aborted.Close()
	waitEnded(t, aborted, "the aborted replacement never ended")

	if got := tr.currentPath(); got != oldPath {
		t.Fatalf("currentPath after an aborted replacement = %q, want the surviving path %q", got, oldPath)
	}
}

// TestTransportStopClosesEveryTrackedChild is #487-shaped: with a single
// tracked slot, an aborted replacement left the old child untracked and a
// later SIGTERM signalled only the dead new one, hanging the daemon on
// detach. stop must reach every child the set is currently holding.
func TestTransportStopClosesEveryTrackedChild(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	a := startChild(t, tr, exec.Command("sleep", "60"))
	b := startChild(t, tr, exec.Command("sleep", "60"))

	tr.stop()

	waitEnded(t, a, "stop left an overlapping child running")
	waitEnded(t, b, "stop left an overlapping child running")
}

// TestTransportStopIsANoOpForAnAlreadyClosedChild covers the other half of
// the set: a child closed individually (the ordinary swap, not a signal) must
// drop out of tracking on its own, and a later stop() must neither panic nor
// double-close it — it should simply have nothing left to do for that child.
func TestTransportStopIsANoOpForAnAlreadyClosedChild(t *testing.T) {
	requireSleep(t)
	tr := &transport{}
	a := startChild(t, tr, exec.Command("sleep", "60"))
	b := startChild(t, tr, exec.Command("sleep", "60"))

	a.Close()
	waitEnded(t, a, "the individually closed child never ended")

	tr.mu.Lock()
	n := len(tr.children)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("tracked children after an individual Close = %d, want 1 (the closed child should have dropped itself)", n)
	}

	tr.stop() // must not panic on the already-closed a, and must still end b
	waitEnded(t, b, "stop left the survivor running after an earlier individual close")
}

// TestChildCloseUnlinksItsSocketPath is R9's other half of R7's cleanup story:
// ControlPersist=no unlinks a ControlPath only on a clean ssh exit, so
// something has to collect the socket of a child ended by SIGKILL. Since
// paths are per-dial now, that collector has to be the child itself, not a
// shared unlink-before-dial.
func TestChildCloseUnlinksItsSocketPath(t *testing.T) {
	requireSleep(t)
	path := filepath.Join(t.TempDir(), "ctl.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed socket file: %v", err)
	}
	c := startChildAt(t, &transport{}, exec.Command("sleep", "60"), path)

	c.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Close left the socket path behind: stat err = %v", err)
	}
}

// TestChildCloseUnblocksParkedReader is the regression net for a Close that
// closed only stdin. ssh reads on until the remote closes its side, so a far
// end that accepts the connection and then answers nothing parks the control
// reader indefinitely — the case the identity deadline exists for.
//
// The helper reproduces that far end honestly: it never reads its stdin, never
// writes its stdout, and ignores SIGTERM. So neither the stdin EOF nor the
// signal can be what frees the reader, and the unblock must not wait out the
// SIGKILL grace either — a reconnect that stalls two seconds per attempt is the
// same wedge in slower clothes.
func TestChildCloseUnblocksParkedReader(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWedgedChild$")
	cmd.Env = append(os.Environ(), wedgedChildEnv+"=1")
	c := startChild(t, &transport{}, cmd)

	parked := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		parked <- err
	}()
	// Without this the test would also pass on a Close that merely raced a Read
	// already on its way back.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-parked:
		t.Fatalf("read returned before the close: %v", err)
	default:
	}

	closed := make(chan struct{})
	go func() { c.Close(); c.Close(); close(closed) }() // twice: the drop path and teardown both close
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case <-parked:
	case <-time.After(time.Second):
		t.Fatal("Close left the reader parked")
	}
	waitEnded(t, c, "Close never ended the wedged child")
}

// TestHelperWedgedChild is the far end TestChildCloseUnblocksParkedReader dials,
// re-executed from this test binary so the case needs no ssh and no fixture on
// PATH.
func TestHelperWedgedChild(t *testing.T) {
	if os.Getenv(wedgedChildEnv) == "" {
		t.Skip("helper process for TestChildCloseUnblocksParkedReader")
	}
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(time.Minute)
}

const wedgedChildEnv = "OG_TEST_WEDGED_CHILD"

func requireSleep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not available")
	}
}

func startChild(t *testing.T, tr *transport, cmd *exec.Cmd) *child {
	t.Helper()
	return startChildAt(t, tr, cmd, "")
}

// startChildAt is startChild for a test that cares about the child's
// ControlPath (R9) — the overlap/accessor tests below.
func startChildAt(t *testing.T, tr *transport, cmd *exec.Cmd, path string) *child {
	t.Helper()
	c, err := newChild(cmd, path)
	if err != nil {
		t.Fatalf("newChild: %v", err)
	}
	if err := tr.start(c); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitEnded(t *testing.T, c *child, msg string) {
	t.Helper()
	select {
	case <-c.ended:
	case <-time.After(transportKillGrace + 5*time.Second):
		c.cmd.Process.Kill()
		t.Fatal(msg)
	}
}

// TestTransportStartFailureReleasesThePipes pins what reconnect needs from a
// failed dial: the child's stdio pipes are gone by the time start returns an
// error. dial() sits inside reattach's retry loop, so a failure that recurs —
// a missing ssh binary, fork/exec under EAGAIN — would otherwise cost two fds
// per attempt across the whole budget.
//
// os/exec supplies it today, which is why there is no cleanup on that path to
// read. The test guards the invariant rather than the code, so it still fails
// if newChild ever moves onto pipes it owns itself.
func TestTransportStartFailureReleasesThePipes(t *testing.T) {
	c, err := newChild(exec.Command(filepath.Join(t.TempDir(), "no-such-binary")), "")
	if err != nil {
		t.Fatalf("newChild: %v", err)
	}
	tr := &transport{}
	if err := tr.start(c); err == nil {
		t.Fatal("transport.start = nil for a binary that does not exist")
	}
	// A closed pipe is the only observable difference, and it is the one that
	// matters: an fd this process still holds would accept the write.
	if _, err := c.in.Write([]byte("x")); err == nil {
		t.Error("stdin pipe still writable after a failed Start; the fd leaked")
	}
	if _, err := c.out.Read(make([]byte, 1)); err == nil {
		t.Error("stdout pipe still readable after a failed Start; the fd leaked")
	}
}

// fakeTmuxScript writes an executable stand-in for tmux whose body is `body`,
// dispatching on "$*". Returns its path, for use as localTmuxArgv[0].
func fakeTmuxScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A mirror whose session has no client of its own must resolve the area from a
// client attached elsewhere on the server, never from #{window_width}:
// FitWindowCmd pins the mirror window to the remote's size (window-size
// manual), so that value is this daemon's own last assertion. Feeding it back
// makes the converger read every resize as "no change", and the mirror keeps
// the stale size until watchLocalClient's 30s fallback poll happens to catch a
// client attached (#532).
func TestLocalAreaPrefersAnotherClientOverTheSessionsOwnPin(t *testing.T) {
	tmux := fakeTmuxScript(t, `
case "$1" in
list-clients)
	case "$*" in
	*"-t mirror"*) exit 0 ;;
	*) echo "200 60 off 100" ;;
	esac
	;;
display-message) echo "138 40" ;;
esac
`)
	w, h := localArea([]string{tmux}, "mirror")
	if w != 200 || h != 60 {
		t.Errorf("localArea = %dx%d, want 200x60 — the attached client, not the 138x40 pin", w, h)
	}
}

// With nothing attached anywhere the pin is the best answer available, and
// asserting it is a no-op. Falling through to the 80x24 default instead would
// actively shrink a remote nobody is watching.
func TestLocalAreaFallsBackToThePinOnlyWhenNoClientExists(t *testing.T) {
	tmux := fakeTmuxScript(t, `
case "$1" in
list-clients) exit 0 ;;
display-message) echo "138 40" ;;
esac
`)
	w, h := localArea([]string{tmux}, "mirror")
	if w != 138 || h != 40 {
		t.Errorf("localArea = %dx%d, want 138x40 — the pin, not the 80x24 default", w, h)
	}
}

// The whole-server fallback measures clients attached to sessions this mirror
// has nothing to do with, so the smallest of them is the wrong answer: a second
// terminal left open at 63 columns capped a mirror the user was about to open
// at 197, and every window of the remote session with it. The client that last
// saw activity is the terminal the user is actually sitting at.
func TestLocalAreaPicksTheMostRecentlyActiveClientNotTheSmallest(t *testing.T) {
	tmux := fakeTmuxScript(t, `
case "$1" in
list-clients)
	case "$*" in
	*"-t mirror"*) exit 0 ;;
	*) printf '63 65 2 100\n197 65 2 200\n' ;;
	esac
	;;
display-message) echo "138 40" ;;
esac
`)
	w, h := localArea([]string{tmux}, "mirror")
	if w != 197 || h != 63 {
		t.Errorf("localArea = %dx%d, want 197x63 — the client last active, not the narrow one", w, h)
	}
}

// client_activity has one-second resolution, so two clients tie routinely. The
// larger wins that tie: the whole point of the fallback is not to let an idle
// narrow terminal decide, and taking the smaller would reinstate it.
func TestLocalAreaBreaksAnActivityTieTowardTheLargerClient(t *testing.T) {
	tmux := fakeTmuxScript(t, `
case "$1" in
list-clients)
	case "$*" in
	*"-t mirror"*) exit 0 ;;
	*) printf '63 65 2 200\n197 65 2 200\n' ;;
	esac
	;;
display-message) echo "138 40" ;;
esac
`)
	w, h := localArea([]string{tmux}, "mirror")
	if w != 197 || h != 63 {
		t.Errorf("localArea = %dx%d, want 197x63 — the larger client on an activity tie", w, h)
	}
}

// The session's OWN clients keep the opposite rule: every one of them is
// displaying this mirror, so its windows have to fit the smallest.
func TestLocalAreaTakesTheSmallestOfTheSessionsOwnClients(t *testing.T) {
	tmux := fakeTmuxScript(t, `
case "$1" in
list-clients) printf '197 65 2 300\n63 65 2 100\n' ;;
display-message) echo "138 40" ;;
esac
`)
	w, h := localArea([]string{tmux}, "mirror")
	if w != 63 || h != 63 {
		t.Errorf("localArea = %dx%d, want 63x63 — the smallest client showing this session", w, h)
	}
}

// TestSeedViewPrefersTheResolvedIdentityOverTheFlags is acceptance 2's "never
// nacked" property (R1/R3): when the mirror session already has a resolvable
// client, its identity wins over the launcher's -term/-termfeatures flags,
// which sample only the invoking client.
func TestSeedViewPrefersTheResolvedIdentityOverTheFlags(t *testing.T) {
	resolve := func() (daemon.ViewIdentity, bool) {
		return daemon.ViewIdentity{Term: "foot", Relay: graphics.NewRelayFromClient(true, "sixel")}, true
	}
	view := seedView(resolve, "xterm-256color", "", "0")

	if got := view.Desired(); got != "foot" {
		t.Errorf("Desired() = %q, want the resolved foot, not the flag seed", got)
	}
	if got := view.Advertised(); got != "foot" {
		t.Errorf("Advertised() = %q, want foot — the very first dial will carry it", got)
	}
	if !view.Relay.Load().Sixel() {
		t.Error("Relay was not seeded from the resolved capability")
	}
}

// TestSeedViewFallsBackToFlagsWhenUnresolvable is R3's other half: the
// startup instant before the launcher has switched a client onto the mirror
// session yet, where the flags are all there is.
func TestSeedViewFallsBackToFlagsWhenUnresolvable(t *testing.T) {
	resolve := func() (daemon.ViewIdentity, bool) { return daemon.ViewIdentity{}, false }
	view := seedView(resolve, "xterm-256color", "bpaste,sixel", "1")

	if got := view.Desired(); got != "xterm-256color" {
		t.Errorf("Desired() = %q, want the flag seed xterm-256color", got)
	}
	if got := view.Advertised(); got != "xterm-256color" {
		t.Errorf("Advertised() = %q, want xterm-256color", got)
	}
	if !view.Relay.Load().Sixel() {
		t.Error("Relay was not seeded from the -sixel flag")
	}
}
