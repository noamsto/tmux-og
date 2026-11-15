package remoteopen

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func newTestRemote(r runner, osName string) (*remote, *[]string) {
	var phases []string
	return &remote{
		r: r, host: "h", tmux: "/usr/bin/tmux", tmpdir: "/run/user/1000", os: osName, uid: "501",
		phase:    func(p string) { phases = append(phases, p) },
		lookPath: func(string) (string, error) { return "/bin/timeout", nil },
	}, &phases
}

const (
	envT       = "env TMUX_TMPDIR=/run/user/1000 /usr/bin/tmux"
	hasWork    = "ssh h " + envT + " has-session -t '=work'"
	listFirst  = "ssh h " + envT + " list-sessions -F '#{session_name}' | head -1"
	startLinux = "ssh h -- systemctl --user restart tmux-startup.service"
)

func equalLines(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("calls =\n%q\nwant\n%q", got, want)
	}
}

func TestRemoteProbe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		placed   bool
		hasTO    bool
		want     string
		wantName string
	}{
		{"plain", false, true, "ssh -T h bash -s", "ssh"},
		{"placed-timeout", true, true, "timeout 7 ssh -T -o ConnectTimeout=7 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 h bash -s", "timeout"},
		{"placed-no-timeout", true, false, "ssh -T -o ConnectTimeout=7 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 h bash -s", "ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &recordingRunner{answer: func(cmdSpec) (string, int) { return "k=v\n", 0 }}
			rm, _ := newTestRemote(rr, "Linux")
			if !tc.hasTO {
				rm.lookPath = func(string) (string, error) { return "", errors.New("none") }
			}
			out, status, err := rm.probe("script", tc.placed, 7)
			if out != "k=v" || status != 0 || err != nil {
				t.Fatalf("got (%q, %d, %v)", out, status, err)
			}
			c := rr.recorded()[0]
			if callLine(c) != tc.want || c.Name != tc.wantName {
				t.Fatalf("call %q, want %q", callLine(c), tc.want)
			}
			if c.Stdin == nil || *c.Stdin != "script\n" || !c.Capture || c.Stderr != stderrInherit {
				t.Fatalf("bad stdin/capture/stderr: %+v", c)
			}
		})
	}
}

func TestRemoteLegs(t *testing.T) {
	rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
		if strings.Contains(callLine(c), "list-") {
			return "work\n", 0
		}
		return "", 0
	}}
	rm, _ := newTestRemote(rr, "Linux")
	if s, st := rm.firstRemoteSession(); s != "work" || st != 0 {
		t.Fatalf("first = %q %d", s, st)
	}
	if !rm.hasSession("work") {
		t.Fatal("hasSession false")
	}
	if w, st := rm.activeWindow("it's"); w != "work" || st != 0 {
		t.Fatalf("win = %q %d", w, st)
	}
	equalLines(t, rr.lines(), []string{
		listFirst,
		hasWork,
		"ssh h " + envT + ` list-windows -t 'it'\''s' -F '#{window_index} #{window_active}' | awk '$2==1{print $1; exit}'`,
	})
	calls := rr.recorded()
	if !calls[0].Capture || calls[1].Stderr != stderrDiscard || calls[1].Capture {
		t.Fatalf("modes: %+v %+v", calls[0], calls[1])
	}
}

func TestRemoteHasSessionAbsent(t *testing.T) {
	rr := &recordingRunner{answer: func(cmdSpec) (string, int) { return "", 1 }}
	rm, _ := newTestRemote(rr, "Linux")
	if rm.hasSession("work") {
		t.Fatal("want false")
	}
}

func TestRemoteStartRemoteServer(t *testing.T) {
	for _, tc := range []struct{ os, call, desc string }{
		{"Linux", startLinux, "tmux-startup.service"},
		{"Darwin", "ssh h -- launchctl kickstart gui/501/org.nix-community.home.tmux-startup", "tmux-startup launchd agent"},
	} {
		t.Run(tc.os, func(t *testing.T) {
			rr := &recordingRunner{}
			rm, phases := newTestRemote(rr, tc.os)
			desc, err := rm.startRemoteServer()
			if desc != tc.desc || err != nil {
				t.Fatalf("got %q %v", desc, err)
			}
			equalLines(t, rr.lines(), []string{tc.call})
			equalLines(t, *phases, []string{"start-server"})

			fail := &recordingRunner{answer: func(cmdSpec) (string, int) { return "", 5 }}
			rm, _ = newTestRemote(fail, tc.os)
			_, err = rm.startRemoteServer()
			var usageErr *UsageError
			want := "og-remote-open: h has no tmux server, and no " + tc.desc + " to start one"
			if !errors.As(err, &usageErr) || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
		})
	}
}

func TestRemoteColdStart(t *testing.T) {
	started := false
	rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
		if callLine(c) == startLinux {
			started = true
		}
		if callLine(c) == listFirst && started {
			return "main\n", 0
		}
		return "", 0
	}}
	rm, _ := newTestRemote(rr, "Linux")
	sess, err := rm.coldStart()
	if sess != "main" || err != nil {
		t.Fatalf("got %q %v", sess, err)
	}
	equalLines(t, rr.lines(), []string{startLinux, listFirst})

	empty := &recordingRunner{}
	rm, _ = newTestRemote(empty, "Linux")
	_, err = rm.coldStart()
	want := "og-remote-open: started tmux-startup.service on h but no session appeared"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

const remuxCmd = `ssh h command -v tmux-remux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux-remux`

func TestRemoteRestore(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		rr := &recordingRunner{}
		rm, phases := newTestRemote(rr, "Linux")
		if err := rm.restore("work"); err != nil {
			t.Fatal(err)
		}
		equalLines(t, rr.lines(), []string{hasWork})
		equalLines(t, *phases, []string{"restore"})
	})
	t.Run("restored", func(t *testing.T) {
		has := 0
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			switch callLine(c) {
			case hasWork:
				has++
				if has == 1 {
					return "", 1
				}
			case listFirst:
				return "other\n", 0
			case remuxCmd:
				return "/usr/bin/tmux-remux\n", 0
			}
			return "", 0
		}}
		rm, phases := newTestRemote(rr, "Linux")
		if err := rm.restore("work"); err != nil {
			t.Fatal(err)
		}
		equalLines(t, rr.lines(), []string{hasWork, listFirst, remuxCmd, "ssh -T h bash -s", hasWork})
		calls := rr.recorded()
		if !calls[2].Capture || calls[2].Stderr != stderrInherit {
			t.Fatalf("remux modes: %+v", calls[2])
		}
		if got := *calls[3].Stdin; got != "env TMUX_TMPDIR=/run/user/1000 PATH=/usr/bin:$PATH /usr/bin/tmux-remux restore\n" {
			t.Fatalf("stdin = %q", got)
		}
		equalLines(t, *phases, []string{"restore"})
	})
	t.Run("cold-start", func(t *testing.T) {
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			switch callLine(c) {
			case hasWork:
				return "", 1
			case remuxCmd:
				return "/x/remux\n", 0
			}
			return "", 0
		}}
		rm, phases := newTestRemote(rr, "Linux")
		err := rm.restore("work")
		if err == nil || !strings.Contains(err.Error(), "was not restored on h") {
			t.Fatalf("err = %v", err)
		}
		equalLines(t, rr.lines(), []string{hasWork, listFirst, startLinux, remuxCmd, "ssh -T h bash -s", hasWork})
		equalLines(t, *phases, []string{"restore", "start-server", "restore"})
	})
	t.Run("filter-skip-message", func(t *testing.T) {
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			if callLine(c) == hasWork {
				return "", 1
			}
			return "x\n", 0
		}}
		rm, _ := newTestRemote(rr, "Linux")
		want := "og-remote-open: session 'work' was not restored on h — tmux-remux's restore filter may have skipped it (idle shells / stale age)"
		if err := rm.restore("work"); err == nil || err.Error() != want {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("restore-fails", func(t *testing.T) {
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			switch callLine(c) {
			case hasWork:
				return "", 1
			case "ssh -T h bash -s":
				return "", 3
			}
			return "x\n", 0
		}}
		rm, _ := newTestRemote(rr, "Linux")
		if err := rm.restore("work"); err == nil || err.Error() != "og-remote-open: tmux-remux restore failed on h" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("remux-lookup-status", func(t *testing.T) {
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			switch callLine(c) {
			case hasWork:
				return "", 1
			case remuxCmd:
				return "", 255
			}
			return "x\n", 0
		}}
		rm, _ := newTestRemote(rr, "Linux")
		var es *exitStatus
		if err := rm.restore("work"); !errors.As(err, &es) || es.code != 255 {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRemoteCreateSession(t *testing.T) {
	newSess := func(size string) string {
		return "ssh h " + envT + " new-session -d -s 'proj' -c '/srv/p'" + size
	}
	hasProj := "ssh h " + envT + " has-session -t '=proj'"
	for _, tc := range []struct {
		name string
		w, h int
		size string
	}{{"no-area", 0, 0, ""}, {"area", 200, 49, " -x 200 -y 49"}} {
		t.Run(tc.name, func(t *testing.T) {
			has := 0
			rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
				switch callLine(c) {
				case hasProj:
					has++
					if has == 1 {
						return "", 1
					}
				case listFirst:
					return "other\n", 0
				}
				return "", 0
			}}
			rm, phases := newTestRemote(rr, "Linux")
			if err := rm.createSession("proj", "/srv/p", tc.w, tc.h); err != nil {
				t.Fatal(err)
			}
			equalLines(t, rr.lines(), []string{hasProj, "ssh h " + envT + " list-sessions -F '#{session_name}' | head -1", newSess(tc.size), hasProj})
			equalLines(t, *phases, []string{"create"})
		})
	}
	t.Run("cold-start-and-failures", func(t *testing.T) {
		rr := &recordingRunner{answer: func(c cmdSpec) (string, int) {
			if strings.Contains(callLine(c), "new-session") {
				return "", 1
			}
			if strings.Contains(callLine(c), "has-session") {
				return "", 1
			}
			return "", 0
		}}
		rm, phases := newTestRemote(rr, "Linux")
		err := rm.createSession("proj", "/srv/p", 0, 0)
		if err == nil || err.Error() != "og-remote-open: could not create session 'proj' in '/srv/p' on h" {
			t.Fatalf("err = %v", err)
		}
		equalLines(t, *phases, []string{"create", "start-server", "create"})

		rr = &recordingRunner{answer: func(c cmdSpec) (string, int) {
			if strings.Contains(callLine(c), "has-session") {
				return "", 1
			}
			return "x\n", 0
		}}
		rm, _ = newTestRemote(rr, "Linux")
		err = rm.createSession("proj", "/srv/p", 0, 0)
		if err == nil || err.Error() != "og-remote-open: session 'proj' was not created on h" {
			t.Fatalf("err = %v", err)
		}
	})
}
