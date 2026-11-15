package remoteopen

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const pairFormat = "#{session_id}|#{?#{@bridge_placed},,#{@bridge_host}}|#{?#{@bridge_placed},,#{@bridge_session}}"

// tmuxFake answers a call by its rendered argv line with status 0; any other
// call answers ("", 1).
func tmuxFake(answers map[string]string) *recordingRunner {
	return &recordingRunner{answer: func(c cmdSpec) (string, int) {
		if out, ok := answers[callLine(c)]; ok {
			return out, 0
		}
		return "", 1
	}}
}

func newLocal(r runner) *localTmux {
	return &localTmux{r: r, ctl: "ctl", sleep: func(time.Duration) {}}
}

func TestLocalResolveInto(t *testing.T) {
	list := "$1|main\n$2|a|b\n$3|mirror"
	cases := []struct {
		name    string
		answers map[string]string
		into    string
		wantID  string
		wantErr string
	}{
		{"exact with pipe", map[string]string{"tmux list-sessions -F #{session_id}|#{session_name}": list}, "a|b", "$2", ""},
		{"prefix is not a match", map[string]string{"tmux list-sessions -F #{session_id}|#{session_name}": list}, "mai", "", "og-remote-open: no local session 'mai'"},
		{"mirror by host", map[string]string{
			"tmux list-sessions -F #{session_id}|#{session_name}": list,
			"tmux show-options -qv -t $3 @bridge_host":            "h",
		}, "mirror", "", "og-remote-open: 'mirror' is a bridge mirror session; --into needs a local one"},
		{"mirror by sock", map[string]string{
			"tmux list-sessions -F #{session_id}|#{session_name}": list,
			"tmux show-options -qv -t $3 @bridge_sock":            "/s",
		}, "mirror", "", "og-remote-open: 'mirror' is a bridge mirror session; --into needs a local one"},
		{"local", map[string]string{"tmux list-sessions -F #{session_id}|#{session_name}": list}, "main", "$1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := newLocal(tmuxFake(tc.answers)).resolveInto(tc.into)
			if tc.wantErr != "" {
				var usageErr *UsageError
				if !errors.As(err, &usageErr) || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("got (%q, %v), want %q", id, err, tc.wantID)
			}
		})
	}
}

func TestLocalParseMirrorArea(t *testing.T) {
	cases := []struct {
		raw  string
		w, h int
		ok   bool
	}{
		{"200 50 on", 200, 49, true},
		{"200 50 off", 200, 50, true},
		{"200 50 2", 200, 48, true},
		{"200 50 weird", 200, 49, true},
		{"200 50 on extra", 200, 49, true},
		{"  200   50  on ", 200, 49, true},
		{"200 1 on", 0, 0, false},
		{"0 50 on", 0, 0, false},
		{"200 05 on", 0, 0, false},
		{"", 0, 0, false},
		{"200", 0, 0, false},
		{"200 50", 200, 49, true},
	}
	for _, tc := range cases {
		w, h, ok := parseMirrorArea(tc.raw)
		if w != tc.w || h != tc.h || ok != tc.ok {
			t.Errorf("parseMirrorArea(%q) = (%d, %d, %v), want (%d, %d, %v)", tc.raw, w, h, ok, tc.w, tc.h, tc.ok)
		}
	}
}

func TestLocalInitialMirrorAreaArgv(t *testing.T) {
	const format = "#{client_width} #{client_height} #{status}"
	for _, pane := range []string{"", "%7"} {
		r := tmuxFake(map[string]string{
			"tmux display-message -p " + format:       "100 30 on",
			"tmux display-message -p -t %7 " + format: "100 30 off",
		})
		l := newLocal(r)
		l.pane = pane
		w, h, ok := l.initialMirrorArea()
		wantH := 29
		if pane != "" {
			wantH = 30
		}
		if w != 100 || h != wantH || !ok {
			t.Errorf("pane %q: got (%d, %d, %v)", pane, w, h, ok)
		}
		if c := r.recorded()[0]; c.Stderr != stderrDiscard || !c.Capture {
			t.Errorf("pane %q: want captured stderr-discarded call, got %+v", pane, c)
		}
	}
	// A failing tmux is an empty answer.
	if _, _, ok := newLocal(tmuxFake(nil)).initialMirrorArea(); ok {
		t.Error("failing tmux: want not ok")
	}
}

func TestLocalSplitTermIdentity(t *testing.T) {
	cases := []struct {
		raw  string
		want [5]string
	}{
		{"a|b|c|d|z|", [5]string{"a", "b", "c", "d", "z"}},
		{"a|b|c|d||", [5]string{"a", "b", "c", "d", ""}},
		{"a|b|c|d|z|w|", [5]string{"a", "b", "c", "d", "z|w|"}},
		{"a|b|c|d|z||", [5]string{"a", "b", "c", "d", "z||"}},
		{"a|b|c|d|z", [5]string{"a", "b", "c", "d", "z"}},
		{"a|b|c", [5]string{"a", "b", "c"}},
		{"", [5]string{}},
		{"a|b|c|d|e\nsecond|line|x|y|z", [5]string{"a", "b", "c", "d", "e"}},
	}
	for _, tc := range cases {
		if got := splitTermIdentity(tc.raw); got != tc.want {
			t.Errorf("splitTermIdentity(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestLocalTermIdentityAndCurSession(t *testing.T) {
	r := tmuxFake(map[string]string{
		"tmux display-message -p -t %3 #{client_termname}|#{client_termfeatures}|#{I/f:sixel}|#{client_cell_width}|#{client_cell_height}": "xterm|f1,f2|1|9|18",
		"tmux display-message -p -t %3 #{session_name}": "cur",
	})
	l := newLocal(r)
	l.pane = "%3"
	if got, want := l.termIdentity(), [5]string{"xterm", "f1,f2", "1", "9", "18"}; got != want {
		t.Errorf("termIdentity = %q, want %q", got, want)
	}
	if got := l.curSession(); got != "cur" {
		t.Errorf("curSession = %q", got)
	}
}

func TestLocalSessionEnv(t *testing.T) {
	r := tmuxFake(map[string]string{
		"tmux show-environment -t s COLORTERM":    "COLORTERM=truecolor",
		"tmux show-environment -t s EMPTYV":       "EMPTYV=",
		"tmux show-environment -t s REMOVED":      "-REMOVED",
		"tmux show-environment -t s TERM_PROGRAM": "TERM_PROGRAM=a=b",
	})
	l := newLocal(r)
	cases := []struct {
		sess, name, want string
		ok               bool
	}{
		{"s", "COLORTERM", "truecolor", true},
		{"s", "EMPTYV", "", true},
		{"s", "REMOVED", "", false},
		{"s", "TERM_PROGRAM", "a=b", true},
		{"s", "UNSET", "", false}, // non-zero status
	}
	for _, tc := range cases {
		got, ok := l.readSessionEnv(tc.sess, tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("readSessionEnv(%q, %q) = (%q, %v), want (%q, %v)", tc.sess, tc.name, got, ok, tc.want, tc.ok)
		}
	}
	n := len(r.recorded())
	if _, ok := l.readSessionEnv("", "COLORTERM"); ok || len(r.recorded()) != n {
		t.Error("empty session: want a miss with no tmux call")
	}
}

func TestLocalSplitPairLine(t *testing.T) {
	cases := []struct{ line, id, host, sess string }{
		{"$1|h|x|", "$1", "h", "x|"},
		{"$1|h|x|y", "$1", "h", "x|y"},
		{"$1||", "$1", "", ""},
		{"$1|h|", "$1", "h", ""},
		{"$1", "$1", "", ""},
	}
	for _, tc := range cases {
		id, host, sess := splitPairLine(tc.line)
		if id != tc.id || host != tc.host || sess != tc.sess {
			t.Errorf("splitPairLine(%q) = (%q, %q, %q)", tc.line, id, host, sess)
		}
	}
}

func TestLocalPairLookup(t *testing.T) {
	answers := map[string]string{
		"tmux list-sessions -F " + pairFormat: strings.Join([]string{
			"$1|h|work",  // verified, adoptable: wins
			"$2|h|work",  // verified, adoptable too: loop continues, first stays
			"$3|h|work",  // verified, name needs sanitizing: retired
			"$4|h|other", // different pair
			"$5|h|work",  // forged: own options do not claim the pair
			"$6|h|work|", // different session (trailing pipe)
		}, "\n"),
		"tmux display-message -p -t $1 #{session_name}": "h-work",
		"tmux display-message -p -t $2 #{session_name}": "h-work-2",
		"tmux display-message -p -t $3 #{session_name}": "h.work",
	}
	for _, id := range []string{"$1", "$2", "$3"} {
		answers["tmux show-options -qv -t "+id+" @bridge_host"] = "h"
		answers["tmux show-options -qv -t "+id+" @bridge_session"] = "work"
	}
	answers["tmux show-options -qv -t $5 @bridge_host"] = "other-host"
	answers["tmux show-options -qv -t $5 @bridge_session"] = "work"
	r := tmuxFake(answers)
	if got := newLocal(r).pairLookup("h", "work"); got != "h-work" {
		t.Fatalf("pairLookup = %q, want h-work", got)
	}
	lines := r.lines()
	if !slices.Contains(lines, "tmux kill-session -t $3") {
		t.Errorf("stale $3 not retired: %q", lines)
	}
	for _, id := range []string{"$1", "$2", "$4", "$5", "$6"} {
		if slices.Contains(lines, "tmux kill-session -t "+id) {
			t.Errorf("%s retired: %q", id, lines)
		}
	}
	if slices.Contains(lines, "tmux display-message -p -t $5 #{session_name}") {
		t.Error("forged row's name was read before its own options claimed the pair")
	}
}

func TestLocalPairLookupNone(t *testing.T) {
	if got := newLocal(tmuxFake(nil)).pairLookup("h", "s"); got != "" {
		t.Errorf("pairLookup = %q, want empty", got)
	}
}

func TestLocalRetireMirrorLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "og-daemon-x.sock")
	for _, p := range []string{sock, sock + ".phase"} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A live pid (the test's own) makes probeDaemon reach the ctl ping; reap
	// is injected so the test process is not signalled.
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{answer: func(c cmdSpec) (string, int) {
		switch callLine(c) {
		case "tmux show-options -qv -t $9 @bridge_sock":
			return sock, 0
		case "ctl --sock " + sock + " ping _":
			return "pong", 0
		}
		return "", 0
	}}
	var reaped []int
	l := newLocal(r)
	l.reap = func(pid int) { reaped = append(reaped, pid) }
	l.retireMirror("$9")

	want := []string{
		"tmux show-options -qv -t $9 @bridge_sock",
		"ctl --sock " + sock + " ping _",
		"tmux kill-session -t $9",
	}
	if got := r.lines(); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if !slices.Equal(reaped, []int{os.Getpid()}) {
		t.Errorf("reaped = %v", reaped)
	}
	for _, p := range []string{sock, sock + ".pid", sock + ".phase"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survives: %v", p, err)
		}
	}
}

func TestLocalRetireMirrorNoSock(t *testing.T) {
	r := tmuxFake(nil)
	newLocal(r).retireMirror("$9")
	want := []string{"tmux show-options -qv -t $9 @bridge_sock", "tmux kill-session -t $9"}
	if got := r.lines(); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestLocalCollisionWalk(t *testing.T) {
	type sess struct{ host, sess string }
	cases := []struct {
		name  string
		base  string
		host  string
		s     string
		held  map[string]sess
		want  string
		calls int
	}{
		{"free", "h-w", "h", "w", nil, "h-w", 1},
		{"own pair", "h-w", "h", "w", map[string]sess{"h-w": {"h", "w"}}, "h-w", 3},
		{"other host", "h-w", "h", "w", map[string]sess{"h-w": {"x", "w"}}, "h-w-remote", 4},
		{"two held", "h-w", "h", "w", map[string]sess{"h-w": {"x", "w"}, "h-w-remote": {"h", "z"}}, "h-w-remote-2", 7},
		{"legacy adoption", "h-w", "h", "w", map[string]sess{"h-w": {"h", ""}}, "h-w", 3},
		{"legacy needs base == host-sess", "h_w", "h", "w", map[string]sess{"h_w": {"h", ""}}, "h_w-remote", 4},
		{"legacy only on base name", "h-w", "h", "w", map[string]sess{"h-w": {"x", "w"}, "h-w-remote": {"h", ""}}, "h-w-remote-2", 7},
		{"non-session held by non-bridge", "h-w", "h", "w", map[string]sess{"h-w": {"", ""}}, "h-w-remote", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{answer: func(c cmdSpec) (string, int) {
				a := c.Args
				switch a[0] {
				case "has-session":
					_, ok := tc.held[strings.TrimPrefix(a[2], "=")]
					if !ok {
						return "", 1
					}
				case "show-options":
					s := tc.held[a[2]]
					if a[4] == "@bridge_host" {
						return s.host, 0
					}
					return s.sess, 0
				}
				return "", 0
			}}
			if got := newLocal(r).collisionWalk(tc.base, tc.host, tc.s); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if got := len(r.recorded()); got != tc.calls {
				t.Errorf("calls = %d, want %d: %q", got, tc.calls, r.lines())
			}
		})
	}
}

// The recorded-argv guards: each replaces a grep over the bash source that
// the old bats suites ran.
func TestLocalGuardRecordedArgv(t *testing.T) {
	answers := map[string]string{
		"tmux list-sessions -F " + pairFormat:                 "$1|h|w",
		"tmux list-sessions -F #{session_id}|#{session_name}": "$1|main",
		"tmux show-options -qv -t $1 @bridge_host":            "h",
		"tmux show-options -qv -t $1 @bridge_session":         "w",
		"tmux display-message -p -t $1 #{session_name}":       "h-w",
		"tmux has-session -t =h-w":                            "",
		"tmux show-options -t h-w -qv @bridge_host":           "x",
		"tmux show-options -t h-w -qv @bridge_session":        "",
		"tmux has-session -t =h-w-remote":                     "",
		"tmux show-options -t h-w-remote -qv @bridge_host":    "h",
		"tmux show-options -t h-w-remote -qv @bridge_session": "w",
	}
	r := tmuxFake(answers)
	l := newLocal(r)
	l.pane = "%1"
	_, _ = l.resolveInto("main")
	l.pairLookup("h", "w")
	l.collisionWalk("h-w", "h", "w")
	l.initialMirrorArea()
	l.termIdentity()
	l.curSession()
	l.readSessionEnv("main", "COLORTERM")
	l.retireMirror("$1")

	lines := r.lines()
	// (1) the walk's show-options use the bare name, never `=`.
	for _, want := range []string{
		"tmux show-options -t h-w -qv @bridge_host",
		"tmux show-options -t h-w -qv @bridge_session",
		"tmux show-options -t h-w-remote -qv @bridge_host",
		"tmux show-options -t h-w-remote -qv @bridge_session",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing %q in %q", want, lines)
		}
	}
	// (2) the pair lookup's -F argument is one exact element.
	var sawPair bool
	for _, c := range r.recorded() {
		if c.Name != "tmux" {
			continue
		}
		if len(c.Args) == 3 && c.Args[0] == "list-sessions" && c.Args[2] == pairFormat {
			sawPair = true
		}
		line := callLine(c)
		if c.Args[0] == "show-options" && strings.Contains(line, "-t =") {
			t.Errorf("show-options with an = target: %q", line)
		}
		// (3) @bridge_ is read through show-options, never display-message.
		if c.Args[0] == "display-message" && strings.Contains(line, "@bridge_") {
			t.Errorf("display-message reads @bridge_: %q", line)
		}
	}
	if !sawPair {
		t.Errorf("no list-sessions call with the exact pair format: %q", lines)
	}
}
