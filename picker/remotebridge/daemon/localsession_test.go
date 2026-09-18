package daemon

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// helperExitEnv carries the exit status TestHelperExitCode should take, so the
// probe's error classification can be exercised against a real *exec.ExitError
// with no tmux and nothing else on PATH — the test binary re-executes itself,
// the same pattern cmd/daemon's wedged-child helper uses.
const helperExitEnv = "OG_DAEMON_TEST_EXIT_CODE"

func TestHelperExitCode(t *testing.T) {
	code := os.Getenv(helperExitEnv)
	if code == "" {
		t.Skip("helper process for TestLocalSessionGone")
	}
	n, err := strconv.Atoi(code)
	if err != nil {
		os.Exit(99)
	}
	os.Exit(n)
}

// exitError runs this test binary as a process that exits with code, returning
// the *exec.ExitError os/exec produces for it.
func exitError(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExitCode$")
	cmd.Env = append(os.Environ(), helperExitEnv+"="+strconv.Itoa(code))
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("helper exit %d: got %v (%T), want *exec.ExitError", code, err, err)
	}
	return err
}

// The probe must act only on tmux's own definite negative: has-session's exit
// status 1. A question that could not be asked — tmux could not be exec'd, or
// answered with any other status — must read as alive, the positive-evidence
// rule localWindowGone already follows (#680).
func TestLocalSessionGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"session there", nil, false},
		{"has-session exit 1 is tmux's definite negative", exitError(t, 1), true},
		{"another exit status is a question that could not be asked", exitError(t, 2), false},
		{"tmux could not be exec'd", errors.New(`exec: "tmux": executable file not found in $PATH`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{LocalSess: "host-sess", LocalTmuxOut: func(...string) (string, error) { return "", tc.err }}
			if got := localSessionGone(cfg); got != tc.want {
				t.Errorf("localSessionGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	// No read seam wired (the daemon's test/degraded paths) and an unnamed
	// session are not evidence either.
	if localSessionGone(Config{LocalSess: "host-sess"}) {
		t.Error("localSessionGone with no LocalTmuxOut = true, want false")
	}
	goneErr := exitError(t, 1)
	if localSessionGone(Config{LocalTmuxOut: func(...string) (string, error) { return "", goneErr }}) {
		t.Error("localSessionGone with an empty LocalSess = true, want false")
	}
}

// Two consecutive definite negatives are required, and one affirmative answer
// clears the count: the probe filters a question that could not be asked, and
// this is what keeps a single spurious negative from tearing a healthy mirror
// down.
func TestSessionGoneTracker(t *testing.T) {
	var tr sessionGoneTracker
	if tr.observe(true) {
		t.Fatal("a single definite negative ended the mirror; want two consecutive")
	}
	if tr.observe(false) {
		t.Fatal("an affirmative answer ended the mirror")
	}
	if tr.observe(true) {
		t.Fatal("the count did not reset on an affirmative answer")
	}
	if !tr.observe(true) {
		t.Error("two consecutive definite negatives did not end the mirror")
	}
}
