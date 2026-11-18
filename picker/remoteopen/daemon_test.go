package remoteopen

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is the child body for the daemon tests; it is a no-op in
// a normal test run.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("GO_REMOTEOPEN_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "sleep":
		time.Sleep(time.Minute)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Minute)
	case "pgid":
		pg, _ := syscall.Getpgid(0)
		_ = os.WriteFile(os.Getenv("GO_REMOTEOPEN_OUT"), []byte(strconv.Itoa(os.Getpid())+" "+strconv.Itoa(pg)), 0o600)
	}
	os.Exit(0)
}

// launchHelper runs this test binary as the daemon in the given mode.
func launchHelper(t *testing.T, mode string, extra ...string) *daemonProc {
	t.Helper()
	log := filepath.Join(t.TempDir(), "daemon.log")
	wrapper := filepath.Join(t.TempDir(), "w.sh")
	script := "#!/bin/sh\nexec " + os.Args[0] + " -test.run=TestHelperProcess\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	childEnv := append(append(os.Environ(), "GO_REMOTEOPEN_HELPER="+mode), extra...)
	d, err := launchDaemon(wrapper, childEnv, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(d.Pid, syscall.SIGKILL) })
	return d
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPidFileParsing(t *testing.T) {
	for _, tc := range []struct {
		content string
		pid     int
		ok      bool
	}{
		{"123\n", 123, true},
		{"123", 123, true},
		{"123\n\n", 123, true},
		{"", 0, false},
		{"\n", 0, false},
		{"-1", 0, false},
		{"0", 0, false},
		{"12a", 0, false},
		{"1 2", 0, false},
	} {
		p := filepath.Join(t.TempDir(), "pid")
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		pid, ok := readPidFile(p)
		if pid != tc.pid || ok != tc.ok {
			t.Errorf("readPidFile(%q) = (%d, %v), want (%d, %v)", tc.content, pid, ok, tc.pid, tc.ok)
		}
	}
	if _, ok := readPidFile(filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("missing file reported ok")
	}
}

func TestDaemonAlive(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "d.pid")
	if remoteDaemonAlive(pf) {
		t.Error("missing pidfile alive")
	}
	if err := os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !remoteDaemonAlive(pf) {
		t.Error("own pid not alive")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if remoteDaemonAlive(pf) {
		t.Error("reaped pid alive")
	}
}

func TestDaemonLaunchSetsid(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	d := launchHelper(t, "pgid", "GO_REMOTEOPEN_OUT="+out)
	waitFor(t, "child exit", d.exited)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] != f[1] || f[0] != strconv.Itoa(d.Pid) {
		t.Fatalf("pid/pgid = %q, want both %d", b, d.Pid)
	}
	if processAlive(d.Pid) {
		t.Error("exited daemon still passes kill -0 (zombie)")
	}
}

func TestDaemonLaunchWritesLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "d.log")
	sh := filepath.Join(t.TempDir(), "d.sh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\necho boom >&2\n"), 0o700); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("stale stale stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := launchDaemon(sh, os.Environ(), log)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit", d.exited)
	if b, _ := os.ReadFile(log); string(b) != "boom\n" {
		t.Fatalf("log = %q, want truncated \"boom\\n\"", b)
	}
}

func TestDaemonLaunchMissingBinary(t *testing.T) {
	if _, err := launchDaemon(filepath.Join(t.TempDir(), "nope"), nil, filepath.Join(t.TempDir(), "l")); err == nil {
		t.Fatal("expected error")
	}
}

func TestReapDaemonTerm(t *testing.T) {
	d := launchHelper(t, "sleep")
	waitFor(t, "alive", func() bool { return processAlive(d.Pid) })
	var slept int
	reapDaemon(d.Pid, func(time.Duration) { slept++; time.Sleep(10 * time.Millisecond) })
	waitFor(t, "exit", d.exited)
	if processAlive(d.Pid) {
		t.Error("still alive after reap")
	}
	if slept >= 20 {
		t.Errorf("polled %d times; TERM should have sufficed", slept)
	}
}

func TestReapDaemonEscalatesToKill(t *testing.T) {
	d := launchHelper(t, "ignore-term")
	// Let the child install its TERM handler.
	time.Sleep(500 * time.Millisecond)
	var slept int
	var total time.Duration
	reapDaemon(d.Pid, func(dur time.Duration) { slept++; total += dur; time.Sleep(dur) })
	waitFor(t, "exit", d.exited)
	if processAlive(d.Pid) {
		t.Error("still alive after KILL")
	}
	if slept != 20 || total != 2*time.Second {
		t.Errorf("slept %d times (%v), want 20 (2s)", slept, total)
	}
}

func fakeCtl(t *testing.T, body string) string {
	t.Helper()
	ctl := filepath.Join(t.TempDir(), "ctl")
	if err := os.WriteFile(ctl, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	return ctl
}

func TestDaemonProbe(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"live", "exit 0", "live"},
		{"outdated", "echo 'old daemon — reopen the bridge' >&2; exit 1", "outdated"},
		{"other", "echo nope; exit 1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeDaemon(execRunner{}, fakeCtl(t, tc.body), sock); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDaemonProbeArgv(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{}
	if got := probeDaemon(r, "ctl", sock); got != "live" {
		t.Fatalf("got %q", got)
	}
	calls := r.recorded()
	if len(calls) != 1 || callLine(calls[0]) != "ctl --sock "+sock+" ping _" || calls[0].Stderr != stderrMerge {
		t.Fatalf("calls = %v", r.lines())
	}
}

func TestDaemonProbeSkipsCtlWithoutLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	marker := filepath.Join(dir, "ran")
	ctl := fakeCtl(t, "touch "+marker)
	if got := probeDaemon(execRunner{}, ctl, sock); got != "" {
		t.Errorf("missing pidfile: got %q", got)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(dead.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probeDaemon(execRunner{}, ctl, sock); got != "" {
		t.Errorf("dead pidfile: got %q", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("ctl ran without a live daemon")
	}
}

func countKey(entries []string, key string) int {
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e, key+"=") {
			n++
		}
	}
	return n
}

func testVals() daemonVals {
	return daemonVals{
		Host: "h", Session: "s", Window: "w", Tmux: "/bin/tmux", Tmpdir: "/run/u",
		LocalSess: "h-s", Into: "@5", Sock: "/x.sock", Renderer: "/r", Reflow: "/f",
		Predict: "1", RemoteOpen: "/ro", Term: "xterm", TermFeatures: "f", Sixel: "1",
		CellW: "8", CellH: "16", Colorterm: "truecolor", TermProgram: "kitty",
	}
}

func TestEnvPlaced(t *testing.T) {
	base := []string{"A=1", "OG_BRIDGE_HOST=old", "OG_DAEMON_REMOTE_OPEN=/stale", "OG_BRIDGE_HOST=dup", "Z=2"}
	got := placedEnv(base, testVals())
	if countKey(got, "OG_DAEMON_REMOTE_OPEN") != 0 {
		t.Error("OG_DAEMON_REMOTE_OPEN not removed")
	}
	if !slices.Contains(got, "OG_DAEMON_SELECT=") {
		t.Errorf("empty select must still be set: %v", got)
	}
	for _, k := range []string{"OG_BRIDGE_HOST", "OG_DAEMON_INTO", "OG_BRIDGE_TERM_PROGRAM", "OG_BRIDGE_CELL_H"} {
		if countKey(got, k) != 1 {
			t.Errorf("%s count = %d", k, countKey(got, k))
		}
	}
	if !slices.Contains(got, "OG_BRIDGE_HOST=h") || !slices.Contains(got, "OG_DAEMON_INTO=@5") {
		t.Errorf("values wrong: %v", got)
	}
	if got[0] != "A=1" || got[1] != "OG_BRIDGE_HOST=h" {
		t.Errorf("order not preserved: %v", got[:3])
	}
}

func TestEnvPlacedSelectValue(t *testing.T) {
	v := testVals()
	v.Select = "pick"
	got := placedEnv([]string{"OG_DAEMON_SELECT=old"}, v)
	if !slices.Contains(got, "OG_DAEMON_SELECT=pick") || countKey(got, "OG_DAEMON_SELECT") != 1 {
		t.Errorf("got %v", got)
	}
}

func TestEnvDedicated(t *testing.T) {
	base := []string{"OG_DAEMON_SELECT=x", "OG_DAEMON_INTO=@9", "OG_DAEMON_REMOTE_OPEN=old"}
	got := dedicatedEnv(base, testVals())
	if countKey(got, "OG_DAEMON_SELECT") != 0 {
		t.Error("OG_DAEMON_SELECT not removed")
	}
	if !slices.Contains(got, "OG_DAEMON_INTO=@9") {
		t.Errorf("inherited OG_DAEMON_INTO changed: %v", got)
	}
	if !slices.Contains(got, "OG_DAEMON_REMOTE_OPEN=/ro") || countKey(got, "OG_DAEMON_REMOTE_OPEN") != 1 {
		t.Errorf("remote open wrong: %v", got)
	}
	if countKey(dedicatedEnv(nil, testVals()), "OG_DAEMON_INTO") != 0 {
		t.Error("dedicated must not set OG_DAEMON_INTO")
	}
}

func plantLink(t *testing.T, dir, name string) (link, victim string) {
	t.Helper()
	victim = filepath.Join(dir, "victim-"+name)
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(dir, name)
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	return link, victim
}

func assertVictim(t *testing.T, victim string) {
	t.Helper()
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "precious" {
		t.Fatalf("victim = %q, %v", got, err)
	}
}

func TestCreateFreshReplacesPlantedSymlink(t *testing.T) {
	for _, name := range []string{"x.log", "x.phase"} {
		dir := t.TempDir()
		link, victim := plantLink(t, dir, name)
		f, err := createFresh(link, 0o644)
		if err != nil {
			t.Fatalf("createFresh: %v", err)
		}
		_, _ = f.WriteString("new")
		_ = f.Close()
		fi, err := os.Lstat(link)
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s not a regular file: %v %v", name, fi, err)
		}
		assertVictim(t, victim)
	}
}

func TestCreateFreshRefusesUnremovableSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	link, victim := plantLink(t, dir, "x.log")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := createFresh(link, 0o644); err == nil {
		_ = f.Close()
		t.Fatal("createFresh followed or replaced an unremovable symlink")
	}
	assertVictim(t, victim)
}
