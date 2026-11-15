package remoteopen

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const probeOut = "os=Linux\nuid=1000\ntmux=/usr/bin/tmux\ntmpdir=/run/user/1000\nsess=work\nwin=1\nwant=\n"

// hookRunner checkpoints around every command, as Main wires execRunner.
type hookRunner struct {
	inner runner
	l     *launcher
}

func (h hookRunner) Run(c cmdSpec) (string, int, error) {
	h.l.checkpoint()
	out, status, err := h.inner.Run(c)
	h.l.afterCommand(status)
	return out, status, err
}

type launchCall struct {
	path, log string
	env       []string
}

type harness struct {
	t        *testing.T
	l        *launcher
	rec      *recordingRunner
	env      map[string]string
	tmp      string
	stderr   *bytes.Buffer
	launches []launchCall
	reaped   []int
	// answer handles calls before the defaults below.
	answer func(c cmdSpec, line string) (string, int, bool)
	// onLaunch runs inside the stubbed launch; nil writes a live pidfile.
	onLaunch func(sock string) *daemonProc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, tmp: t.TempDir(), stderr: &bytes.Buffer{}}
	h.env = map[string]string{"TMUX_TMPDIR": h.tmp, "OG_REMOTE_TMPDIR": "/run/user/1000"}
	h.rec = &recordingRunner{answer: func(c cmdSpec) (string, int) {
		line := callLine(c)
		if h.answer != nil {
			if out, status, ok := h.answer(c, line); ok {
				return out, status
			}
		}
		switch {
		case c.Name == "ssh" && c.Stdin != nil && strings.Contains(*c.Stdin, ": og-probe;"):
			return probeOut, 0
		case strings.HasPrefix(line, "tmux list-sessions -F #{session_id}|#{session_name}"):
			return "$3|dispatch", 0
		case strings.HasPrefix(line, "tmux display-message -p -t $3 #{pid}"):
			return "4242", 0
		case c.Name == "tmux" && c.Args[0] == "has-session":
			return "", 1
		case strings.HasSuffix(c.Name, "og-remote-bridge-ctl"):
			return "", 0
		}
		return "", 0
	}}
	l := &launcher{
		getenv:  func(k string) string { return h.env[k] },
		environ: func() []string { return []string{"KEEP=1", "OG_DAEMON_SELECT=inherited", "OG_DAEMON_INTO=inherited"} },
		lookPath: func(name string) (string, error) {
			if name == "timeout" || name == "og-remote-loading" {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		sleep:  func(time.Duration) {},
		pid:    os.Getpid(),
		stderr: h.stderr,
		sig:    newSigWatch(),
		paths:  Paths{Self: "/self", Ctl: "@bridge_ctl@", Daemon: "@bridge_daemon@", Renderer: "@bridge_renderer@", Reflow: "@reflow@", Loading: "/bin/loading"},
	}
	l.r = hookRunner{inner: h.rec, l: l}
	l.reap = func(pid int) { h.reaped = append(h.reaped, pid) }
	l.launch = func(path string, env []string, log string) (*daemonProc, error) {
		h.launches = append(h.launches, launchCall{path: path, log: log, env: env})
		sock := strings.TrimSuffix(log, ".log")
		if h.onLaunch != nil {
			return h.onLaunch(sock), nil
		}
		writeFile(t, sock+".pid", strconv.Itoa(os.Getpid())+"\n")
		return &daemonProc{Pid: 777, done: make(chan struct{})}, nil
	}
	h.l = l
	return h
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) run(args ...string) int { return h.l.main(args) }

func (h *harness) tmuxLines() []string {
	out := []string{}
	for _, line := range h.rec.lines() {
		if rest, ok := strings.CutPrefix(line, "tmux "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// indexOf is the position of the first tmux call starting with prefix, -1
// when none does.
func (h *harness) indexOf(prefix string) int {
	return slices.IndexFunc(h.tmuxLines(), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func fireSignal(l *launcher) { l.sig.deliver(syscall.SIGTERM) }

func envHas(env []string, kv string) bool { return slices.Contains(env, kv) }

func envKey(env []string, key string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") })
}

func TestLauncherDedicatedHappyPath(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(h.tmp, "og-daemon-h-work.sock")
	if code := h.run("h", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	ns := h.indexOf("new-session -d -s h-work -n work -- /bin/loading h work " + sock + ".phase")
	sessOpt := h.indexOf("set-option -t h-work @bridge_session work")
	hostOpt := h.indexOf("set-option -t h-work @bridge_host h")
	sw := h.indexOf("switch-client -t =h-work")
	if ns < 0 || sessOpt <= ns || hostOpt <= sessOpt || sw <= hostOpt {
		t.Fatalf("order new-session=%d session=%d host=%d switch=%d in %q", ns, sessOpt, hostOpt, sw, h.tmuxLines())
	}
	if got, _ := os.ReadFile(sock + ".phase"); string(got) != "connecting to h\n" {
		t.Fatalf("phase file = %q", got)
	}
	if len(h.launches) != 1 {
		t.Fatalf("launches = %d", len(h.launches))
	}
	lc := h.launches[0]
	if lc.path != "/bin/og-remote-bridge-daemon" || lc.log != sock+".log" {
		t.Fatalf("launch = %+v", lc)
	}
	for _, kv := range []string{"OG_DAEMON_REMOTE_OPEN=/self", "OG_BRIDGE_WINDOW=1", "OG_DAEMON_LOCAL_SESS=h-work", "OG_DAEMON_SOCK=" + sock, "OG_DAEMON_INTO=inherited", "OG_DAEMON_RENDERER=/bin/og-remote-bridge-renderer", "OG_DAEMON_REFLOW=/bin/tmux-reflow-windows", "KEEP=1"} {
		if !envHas(lc.env, kv) {
			t.Errorf("daemon env lacks %s: %q", kv, lc.env)
		}
	}
	if envKey(lc.env, "OG_DAEMON_SELECT") {
		t.Errorf("daemon env keeps OG_DAEMON_SELECT: %q", lc.env)
	}
}

func TestLauncherDedupSwitchesWithoutRecreating(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(h.tmp, "og-daemon-h-work.sock")
	writeFile(t, sock+".pid", strconv.Itoa(os.Getpid())+"\n")
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		switch line {
		case "tmux has-session -t =h-work":
			return "", 0, true
		case "tmux show-options -t h-work -qv @bridge_host":
			return "h", 0, true
		case "tmux show-options -t h-work -qv @bridge_session":
			return "work", 0, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	if h.indexOf("switch-client -t =h-work") < 0 || h.indexOf("new-session") >= 0 || h.indexOf("kill-session") >= 0 {
		t.Fatalf("tmux calls %q", h.tmuxLines())
	}
	if len(h.launches) != 0 || len(h.reaped) != 0 {
		t.Fatalf("launches %d, reaped %v", len(h.launches), h.reaped)
	}
}

func TestLauncherPlacedHappyPath(t *testing.T) {
	h := newHarness(t)
	if code := h.run("h", "work", "--into", "dispatch"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	for _, line := range h.tmuxLines() {
		for _, banned := range []string{"new-session", "kill-session", "switch-client", "set-option"} {
			if strings.HasPrefix(line, banned) {
				t.Fatalf("placed open ran tmux %s", line)
			}
		}
	}
	sock := intoSockPath(h.tmp, "h", "work", "4242", "$3", "")
	if _, err := os.Stat(sock + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
	if len(h.launches) != 1 {
		t.Fatalf("launches = %d", len(h.launches))
	}
	env := h.launches[0].env
	for _, kv := range []string{"OG_DAEMON_INTO=$3", "OG_DAEMON_SELECT=", "OG_DAEMON_LOCAL_SESS=dispatch", "OG_DAEMON_SOCK=" + sock, "OG_BRIDGE_SESSION=work", "OG_BRIDGE_WINDOW=1"} {
		if !envHas(env, kv) {
			t.Errorf("daemon env lacks %s: %q", kv, env)
		}
	}
	if envKey(env, "OG_DAEMON_REMOTE_OPEN") {
		t.Errorf("placed env carries OG_DAEMON_REMOTE_OPEN")
	}
}

func TestLauncherSignalAfterAttachIsSuccess(t *testing.T) {
	h := newHarness(t)
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if strings.HasPrefix(line, "tmux switch-client") {
			fireSignal(h.l)
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	ns := h.indexOf("new-session")
	if lines := h.tmuxLines(); ns < 0 || slices.Contains(lines[ns:], "kill-session -t =h-work") {
		t.Fatalf("rolled back after attach: %q", lines)
	}
}

func TestLauncherSignalRollsBackDedicatedMirror(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(h.tmp, "og-daemon-h-work.sock")
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if line == "tmux set-option -t h-work @bridge_session work" {
			fireSignal(h.l)
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 143 {
		t.Fatalf("exit %d, want 143", code)
	}
	ns := h.indexOf("new-session")
	lines := h.tmuxLines()
	if ns < 0 || !slices.Contains(lines[ns:], "kill-session -t =h-work") {
		t.Fatalf("no kill-session after new-session: %q", lines)
	}
	if h.indexOf("switch-client") >= 0 || len(h.launches) != 0 {
		t.Fatalf("went on after the signal: %q, launches %d", lines, len(h.launches))
	}
	for _, p := range []string{sock, sock + ".pid", sock + ".phase"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestLauncherSignalReapsPlacedDaemonAndReleasesLock(t *testing.T) {
	h := newHarness(t)
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if strings.HasSuffix(c.Name, "og-remote-bridge-ctl") {
			fireSignal(h.l)
			return "", 1, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work", "--into", "dispatch"); code != 143 {
		t.Fatalf("exit %d, want 143, stderr %q", code, h.stderr)
	}
	if !slices.Equal(h.reaped, []int{777}) {
		t.Fatalf("reaped %v, want [777]", h.reaped)
	}
	sock := intoSockPath(h.tmp, "h", "work", "4242", "$3", "")
	for _, p := range []string{sock + ".lock", sock + ".pid"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestLauncherSignalBeforeAnythingExits143(t *testing.T) {
	h := newHarness(t)
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if c.Name == "ssh" {
			fireSignal(h.l)
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 143 {
		t.Fatalf("exit %d, want 143", code)
	}
	if h.indexOf("new-session") >= 0 || h.indexOf("kill-session") >= 0 {
		t.Fatalf("tmux calls %q", h.tmuxLines())
	}
}

// A signal os/signal forwards only after the probe returned, with no further
// command to checkpoint: the refusal that follows still yields to it.
func TestLauncherLateSignalBeatsTheRefusal(t *testing.T) {
	h := newHarness(t)
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if c.Name == "ssh" {
			go func() {
				time.Sleep(10 * time.Millisecond)
				fireSignal(h.l)
			}()
			return "", 0, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 143 {
		t.Fatalf("exit %d, want 143, stderr %q", code, h.stderr)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr %q, want none", h.stderr)
	}
}

// fireSoon delivers SIGTERM shortly after the current command returns, as
// os/signal forwards a group signal that also killed the child.
func fireSoon(l *launcher) {
	go func() {
		time.Sleep(10 * time.Millisecond)
		fireSignal(l)
	}()
}

// A group signal kills the ctl ping of a live dedicated daemon: its empty
// reply must not be taken for "no daemon" before the signal is seen.
func TestLauncherSignalKilledPingSparesLiveMirror(t *testing.T) {
	h := newHarness(t)
	sock := filepath.Join(h.tmp, "og-daemon-h-work.sock")
	writeFile(t, sock, "")
	writeFile(t, sock+".pid", strconv.Itoa(os.Getpid())+"\n")
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if strings.HasSuffix(c.Name, "og-remote-bridge-ctl") {
			fireSoon(h.l)
			return "", 143, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 143 {
		t.Fatalf("exit %d, want 143, stderr %q", code, h.stderr)
	}
	if h.indexOf("kill-session") >= 0 || h.indexOf("new-session") >= 0 {
		t.Fatalf("tmux calls %q", h.tmuxLines())
	}
	for _, p := range []string{sock, sock + ".pid"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

// The same at D5: a listening placed daemon keeps its files.
func TestLauncherSignalKilledPingSparesPlacedDaemon(t *testing.T) {
	h := newHarness(t)
	sock := intoSockPath(h.tmp, "h", "work", "4242", "$3", "")
	writeFile(t, sock, "")
	writeFile(t, sock+".pid", strconv.Itoa(os.Getpid())+"\n")
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if strings.HasSuffix(c.Name, "og-remote-bridge-ctl") {
			fireSoon(h.l)
			return "", 143, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work", "--into", "dispatch"); code != 143 {
		t.Fatalf("exit %d, want 143, stderr %q", code, h.stderr)
	}
	if len(h.launches) != 0 || len(h.reaped) != 0 {
		t.Fatalf("launches %d, reaped %v", len(h.launches), h.reaped)
	}
	for _, p := range []string{sock, sock + ".pid"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	if _, err := os.Stat(sock + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// A daemon its Wait already reaped is not signalled by pid again.
func TestLauncherSignalSkipsReapOfExitedDaemon(t *testing.T) {
	h := newHarness(t)
	h.onLaunch = func(sock string) *daemonProc {
		writeFile(t, sock+".pid", strconv.Itoa(os.Getpid())+"\n")
		d := &daemonProc{Pid: 777, done: make(chan struct{})}
		close(d.done)
		return d
	}
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if strings.HasSuffix(c.Name, "og-remote-bridge-ctl") {
			fireSignal(h.l)
			return "", 1, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work", "--into", "dispatch"); code != 143 {
		t.Fatalf("exit %d, want 143, stderr %q", code, h.stderr)
	}
	if len(h.reaped) != 0 {
		t.Fatalf("reaped %v, want none", h.reaped)
	}
	sock := intoSockPath(h.tmp, "h", "work", "4242", "$3", "")
	if _, err := os.Stat(sock + ".pid"); !os.IsNotExist(err) {
		t.Errorf("pidfile left behind: %v", err)
	}
}

func TestSigWatchWaitPending(t *testing.T) {
	w := newSigWatch()
	if w.waitPending(time.Millisecond) {
		t.Fatal("pending with no signal")
	}
	go w.deliver(syscall.SIGHUP)
	if !w.waitPending(5 * time.Second) {
		t.Fatal("delivered signal not seen")
	}
}

func TestLauncherPlacedDaemonExitsBeforeListening(t *testing.T) {
	h := newHarness(t)
	h.onLaunch = func(sock string) *daemonProc {
		var log strings.Builder
		for i := 1; i <= 25; i++ {
			log.WriteString("line " + strconv.Itoa(i) + "\n")
		}
		writeFile(t, sock+".log", log.String())
		d := &daemonProc{Pid: 777, done: make(chan struct{})}
		close(d.done)
		return d
	}
	if code := h.run("h", "work", "--into", "dispatch"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	got := h.stderr.String()
	if !strings.HasPrefix(got, "og-remote-open: the bridge daemon for h:work exited before listening:\nline 6\n") || !strings.HasSuffix(got, "line 25\n") {
		t.Fatalf("stderr %q", got)
	}
}

func TestLauncherPlacedNotListening(t *testing.T) {
	h := newHarness(t)
	h.env["OG_INTO_LISTEN_TIMEOUT"] = "1"
	h.onLaunch = func(string) *daemonProc { return &daemonProc{Pid: 777, done: make(chan struct{})} }
	if code := h.run("h", "work", "--into", "dispatch"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := h.stderr.String(); got != "og-remote-open: the bridge daemon for h:work was not listening after 1s\n" {
		t.Fatalf("stderr %q", got)
	}
	if !slices.Equal(h.reaped, []int{777}) {
		t.Fatalf("reaped %v", h.reaped)
	}
}

func TestLauncherRefusals(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		predict string
		lookup  bool // og-remote-bridge-daemon resolves
		msg     string
	}{
		{"usage", nil, "", true, "og-remote-open: usage: og-remote-open <host> [<sess> [<win>]]"},
		{"A11 before predict", []string{"h", "s", "abc"}, "bad", true, "og-remote-open: window must be a numeric index or an @id, got: abc"},
		{"predict", []string{"h", "s"}, "it's", true, `og-remote-open: @og_bridge_predict must be auto, on or off, got: it\'s`},
		{"no local session", []string{"h", "s", "--into", "nope"}, "", true, "og-remote-open: no local session 'nope'"},
		{"daemon missing", []string{"h", "work"}, "", false, "og-remote-open: og-remote-bridge-daemon not found on PATH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.answer = func(c cmdSpec, line string) (string, int, bool) {
				if line == "tmux show-options -gqv @og_bridge_predict" {
					return tc.predict, 0, true
				}
				return "", 0, false
			}
			lookPath := h.l.lookPath
			h.l.lookPath = func(name string) (string, error) {
				if name == "og-remote-bridge-daemon" && !tc.lookup {
					return "", errors.New("not found")
				}
				return lookPath(name)
			}
			if code := h.run(tc.args...); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if got := h.stderr.String(); got != tc.msg+"\n" {
				t.Fatalf("stderr %q, want %q", got, tc.msg)
			}
		})
	}
}

func TestLauncherProbeStatusIsTheExit(t *testing.T) {
	h := newHarness(t)
	h.answer = func(c cmdSpec, line string) (string, int, bool) {
		if c.Name == "ssh" {
			return "", 255, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 255 {
		t.Fatalf("exit %d, want 255", code)
	}
}

func TestLauncherProgressPhases(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.tmp, "phases")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	h.l.progress = f
	launch := h.l.launch
	h.l.launch = func(p string, env []string, log string) (*daemonProc, error) {
		if h.l.progress != nil {
			t.Error("progress fd still open at launch")
		}
		return launch(p, env, log)
	}
	if code := h.run("h", "work"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.ReadFile(path); string(got) != "connect\nmirror\n" {
		t.Fatalf("phases %q", got)
	}
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ content, want string }{
		{"", ""},
		{"a\nb\nc\n", "b\nc\n"},
		{"a\nb\nc", "b\nc"},
		{"only\n", "only\n"},
	}
	for i, tc := range cases {
		p := filepath.Join(dir, strconv.Itoa(i))
		writeFile(t, p, tc.content)
		if got := tailLines(p, 2); got != tc.want {
			t.Errorf("tailLines(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
	if got := tailLines(filepath.Join(dir, "missing"), 2); got != "" {
		t.Errorf("missing file = %q", got)
	}
}

func TestAdoptProgressFD(t *testing.T) {
	t.Setenv("OG_REMOTE_OPEN_PROGRESS_FD", "987")
	if f := adoptProgressFD(); f != nil {
		t.Fatal("adopted an fd with nothing open there")
	}
	if _, set := os.LookupEnv("OG_REMOTE_OPEN_PROGRESS_FD"); set {
		t.Fatal("variable left set")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck // test pipe
	// A dup: an inherited fd, not close-on-exec, owned by no *os.File.
	fd, err := syscall.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OG_REMOTE_OPEN_PROGRESS_FD", strconv.Itoa(fd))
	f := adoptProgressFD()
	if f == nil {
		t.Fatal("open fd not adopted")
	}
	flags, _, _ := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("adopted fd not close-on-exec")
	}
	l := &launcher{progress: f}
	l.phase("connect")
	l.closeProgress()
	buf := make([]byte, 16)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "connect\n" {
		t.Fatalf("read %q", buf[:n])
	}
}
