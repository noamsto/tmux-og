package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestThemeProbeCmdShape(t *testing.T) {
	got := themeProbeCmd("g6-work")
	// Wrapped in /bin/sh -c: tmux jobs, like run-shell, use the remote's
	// default-shell (fish, not POSIX).
	want := "display-message -p -t 'g6-work' '#(/bin/sh -c '\\''command -v theme-toggle >/dev/null 2>&1 && echo yes || echo no'\\'')'"
	if got != want {
		t.Errorf("themeProbeCmd = %q, want %q", got, want)
	}
}

func TestThemeToggleAvailablePresent(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1",
		"yes",
		"%end 1 1 1",
	}, "\n") + "\n"
	rt, _ := scriptedRT(script)

	available, checked := themeToggleAvailable(rt, "g6-work")
	if !checked || !available {
		t.Errorf("available=%v checked=%v, want true/true", available, checked)
	}
}

func TestThemeToggleAvailableAbsent(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1",
		"no",
		"%end 1 1 1",
	}, "\n") + "\n"
	rt, _ := scriptedRT(script)

	available, checked := themeToggleAvailable(rt, "g6-work")
	if !checked || available {
		t.Errorf("available=%v checked=%v, want false/true", available, checked)
	}
}

// A #() job's first read routinely comes back empty (the job hasn't run yet)
// — themeToggleAvailable must retry rather than reading that as "missing".
func TestThemeToggleAvailableRetriesUntilJobPopulates(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1",
		"%end 1 1 1",
		"%begin 2 1 1",
		"yes",
		"%end 2 1 1",
	}, "\n") + "\n"
	rt, _ := scriptedRT(script)

	var slept []time.Duration
	available, checked := themeToggleAvailableRetry(rt, "g6-work", 6, time.Second, func(d time.Duration) {
		slept = append(slept, d)
	})

	if !checked || !available {
		t.Errorf("available=%v checked=%v, want true/true once the job populates", available, checked)
	}
	if len(slept) != 1 {
		t.Errorf("slept %d times, want 1 (one empty read, then the populated one)", len(slept))
	}
}

// A job that never populates within the retry budget is inconclusive, not
// evidence of a missing binary — it must give up rather than retry forever.
func TestThemeToggleAvailableGivesUpWhenJobNeverPopulates(t *testing.T) {
	var script strings.Builder
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&script, "%%begin %d 1 1\n%%end %d 1 1\n", i, i)
	}
	rt, _ := scriptedRT(script.String())

	var sleeps int
	available, checked := themeToggleAvailableRetry(rt, "g6-work", 6, time.Second, func(time.Duration) { sleeps++ })

	if checked || available {
		t.Errorf("available=%v checked=%v, want false/false when the job never populates", available, checked)
	}
	if sleeps != 5 {
		t.Errorf("slept %d times, want 5 (no sleep after the last, exhausted attempt)", sleeps)
	}
}

// A dropped connection before the reply arrives must not be read as "missing"
// — that would misreport a flaky link as a host with no theme-toggle.
func TestThemeToggleAvailableConnectionDropped(t *testing.T) {
	rt, _ := scriptedRT("")

	available, checked := themeToggleAvailable(rt, "g6-work")
	if checked || available {
		t.Errorf("available=%v checked=%v, want false/false on a dropped connection", available, checked)
	}
}

// A %error reply (the run-shell command itself failed) is inconclusive, not
// evidence of a missing binary.
func TestThemeToggleAvailableErrorReply(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1",
		"%error 1 1 1",
	}, "\n") + "\n"
	rt, _ := scriptedRT(script)

	available, checked := themeToggleAvailable(rt, "g6-work")
	if checked || available {
		t.Errorf("available=%v checked=%v, want false/false on an error reply", available, checked)
	}
}

// retryNotifyLocal is what the launcher's (picker/remoteopen) daemon-then-
// switch-client ordering needs: the mirror session may have no client yet the moment
// Run() calls this, so the loop must keep trying rather than giving up on the
// first empty list-clients.
func TestRetryNotifyLocalStopsOnceAClientAttaches(t *testing.T) {
	var calls int
	attachOnCall := 3
	cfg := Config{
		RemoteHost: "halo",
		LocalSess:  "halo-work",
		LocalTmuxOut: func(args ...string) (string, error) {
			calls++
			if calls < attachOnCall {
				return "", nil
			}
			return "client1\n", nil
		},
		LocalTmux: func(args ...string) error { return nil },
	}

	var slept []time.Duration
	retryNotifyLocal(cfg, "no theme-toggle on halo", 6, time.Second, func(d time.Duration) {
		slept = append(slept, d)
	})

	if calls != attachOnCall {
		t.Errorf("list-clients checked %d times, want %d (stop as soon as a client attaches)", calls, attachOnCall)
	}
	if len(slept) != attachOnCall-1 {
		t.Errorf("slept %d times, want %d (one less than the checks)", len(slept), attachOnCall-1)
	}
}

// A client that never attaches within the retry budget leaves the loop
// best-effort, not stuck — it must give up rather than sleep forever.
func TestRetryNotifyLocalGivesUpAfterRetriesExhausted(t *testing.T) {
	var calls int
	cfg := Config{
		RemoteHost:   "halo",
		LocalSess:    "halo-work",
		LocalTmuxOut: func(args ...string) (string, error) { calls++; return "", nil },
		LocalTmux:    func(args ...string) error { return nil },
	}

	var sleeps int
	retryNotifyLocal(cfg, "no theme-toggle on halo", 6, time.Second, func(time.Duration) { sleeps++ })

	if calls != 6 {
		t.Errorf("list-clients checked %d times, want 6", calls)
	}
	if sleeps != 5 {
		t.Errorf("slept %d times, want 5 (no sleep after the last, exhausted attempt)", sleeps)
	}
}

// Run() must be callable from a test Config that never sets LocalTmuxOut —
// notifyLocal's only caller before this change was the paste path, which is
// off with no ssh transport; Run() is now unconditional.
func TestNotifyLocalNilConfigFuncsIsANoop(t *testing.T) {
	if notifyLocal(Config{}, "no theme-toggle") {
		t.Error("notifyLocal with nil LocalTmuxOut/LocalTmux reported success")
	}
}
