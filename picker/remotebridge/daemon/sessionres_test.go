package daemon

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestResShipperFlush walks one shipper through a sequence, because every rule
// it has is about what the PREVIOUS report left behind — the refresh floor, the
// unchanged-row suppression, and what reset forgets are all invisible to a
// case that starts fresh.
func TestResShipperFlush(t *testing.T) {
	// figures is every field but the tick, in the order the daemon writes them
	// around its own receive time.
	const (
		figures = "12.5 340 32 claude"
		row     = "12.5 340 32 1789727000 claude"
	)

	steps := []struct {
		name string
		// queue is the value a notification carried; nothing is queued when
		// silent is set, which is the loop's own tick re-entering.
		queue  string
		silent bool
		// reset stands in for repair, and age for the refresh floor elapsing.
		reset bool
		age   time.Duration
		// stamp is the figures the pass must write, beside the receive time it
		// appends; unset is the -u form. Neither means it must write nothing.
		stamp string
		unset bool
	}{
		{
			name:  "first report writes",
			queue: row,
			stamp: figures,
		},
		{
			// The remote's poller moves its tick every pass by design; that is
			// what makes it observable, and it must cost nothing here.
			name:  "same figures, newer tick, inside the floor",
			queue: "12.5 340 32 1789727005 claude",
		},
		{
			// The agent field is a figure, not a tick: a restored agent appearing
			// in the tree must reach the picker without waiting out the floor.
			name:  "an agent change inside the floor writes",
			queue: "12.5 340 32 1789727005 -",
			stamp: "12.5 340 32 -",
		},
		{
			name:  "and changing back writes again",
			queue: row,
			stamp: figures,
		},
		{
			name:   "nothing pending",
			silent: true,
		},
		{
			name:  "same figures past the floor re-stamp",
			queue: row,
			age:   sessionResRefresh,
			stamp: figures,
		},
		{
			name:  "a letter drops whole",
			queue: "12.5 34O 32 1789727000 claude",
		},
		{
			name:  "a pipe drops whole",
			queue: "12.5|340 32 1789727000 claude",
		},
		{
			// The pre-agent-field shape: nothing this revision emits, and a
			// four-field row the picker would misread.
			name:  "a short row drops whole",
			queue: "12.5 340 32 1789727000",
		},
		{
			name:  "two decimals drop whole",
			queue: "12.55 340 32 1789727000 claude",
		},
		{
			name:  "a markup-bearing agent drops whole",
			queue: "12.5 340 32 1789727000 #[fg=red]",
		},
		{
			name:  "an empty agent in the list drops whole",
			queue: "12.5 340 32 1789727000 claude,",
		},
		{
			// Dropped, not unset: the previous stamp is still the last real
			// reading, and the step below proves it is still what is written.
			name:  "a drop leaves the floor where it was",
			queue: row,
		},
		{
			name:  "reset makes an identical report write again",
			queue: row,
			reset: true,
			stamp: figures,
		},
		{
			name:  "an empty report unsets",
			queue: "",
			unset: true,
		},
		{
			// figures was cleared by the unset, so the same row is new again.
			name:  "a report after an unset writes",
			queue: row,
			stamp: figures,
		},
	}

	var calls [][]string
	cfg := Config{
		LocalSess: "mirror",
		LocalTmux: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
	}
	r := newResShipper()
	var lastStamp int64

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.reset {
				r.reset()
			}
			if !s.silent {
				r.queue(s.queue)
			}
			if s.age > 0 {
				r.lastWrite = r.lastWrite.Add(-s.age)
				// The stamped epoch is the real clock at 1s resolution,
				// so crossing a second boundary is the only way to see it move.
				for start := time.Now().Unix(); time.Now().Unix() == start; {
					time.Sleep(10 * time.Millisecond)
				}
			}
			calls = nil
			r.flush(cfg)

			if s.stamp == "" && !s.unset {
				if len(calls) != 0 {
					t.Fatalf("wrote %v, want nothing", calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("wrote %v, want exactly one command", calls)
			}
			got := calls[0]
			if s.unset {
				want := []string{"set-option", "-u", "-t", "mirror", "@bridge_res"}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("argv = %v, want %v", got, want)
				}
				return
			}
			want := []string{"set-option", "-t", "mirror", "@bridge_res"}
			if len(got) != 5 || !reflect.DeepEqual(got[:4], want) {
				t.Fatalf("argv = %v, want %v + a value", got, want)
			}
			f := strings.Fields(got[4])
			if len(f) != 5 || strings.Join([]string{f[0], f[1], f[2], f[4]}, " ") != s.stamp {
				t.Fatalf("value = %q, want %q around a receive time", got[4], s.stamp)
			}
			now, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				t.Fatalf("receive time %q: %v", f[3], err)
			}
			// The daemon's own clock, never the remote's tick — every fixture
			// above carries a tick far in the future, so a shipper that passed
			// one through fails here.
			if delta := time.Now().Unix() - now; delta < 0 || delta > 5 {
				t.Errorf("receive time %d is %ds off the local clock", now, delta)
			}
			// Strictly newer only where the step waited out a second boundary;
			// two writes inside one second legitimately share a value.
			if s.age > 0 && now <= lastStamp {
				t.Errorf("re-stamp past the floor carries %d, not newer than %d", now, lastStamp)
			}
			lastStamp = now
		})
	}
}

// TestSessionResSubscriptionIsSessionScoped is the one assertion a unit test
// cannot make: cmd_refresh_client_update_subscription silently REMOVES a
// subscription whose spec it cannot parse and returns no %error, so
// sendSubscription's Kind check cannot catch a typo — and this shipper, unlike
// its two siblings, has no poll backstop to mask one. Only a live tmux can say
// the empty `what` really is the session-scoped spelling.
func TestSessionResSubscriptionIsSessionScoped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also puts tmux in nativeBuildInputs — so a missing tmux there
		// means that input was pruned, not that this is a dev machine.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	// A private TMUX_TMPDIR does not isolate CLAUDE_STATUS_DIR, which defaults
	// to a bare /tmp/claude-status every tmux server on the machine shares —
	// and this server's own config-load tick prunes and reaps under it, so
	// omitting this would have the test destroy another server's agent state.
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())

	ctl := tmux("-C", "attach-session", "-t", "w")
	stdin, err := ctl.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := ctl.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.Start(); err != nil {
		t.Fatalf("control client: %v", err)
	}
	t.Cleanup(func() {
		ctl.Process.Kill()
		ctl.Wait()
	})

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	if _, err := fmt.Fprintln(stdin, subscribeCmd(resSubName, "", sessionResFormat)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// A subscription reports once on subscribe, with the empty value of the
	// still-unset option. Waited for, so the set below cannot land before the
	// subscription exists — and so a spec tmux dropped fails here rather than
	// looking like a slow set.
	prefix := "%subscription-changed " + resSubName + " "
	initial := waitForLine(t, lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
	if v, ok := subscriptionValue(controlmode.ParseLine(initial), resSubName); !ok || v != "" {
		t.Fatalf("initial report = %q, %v; want an empty value for the unset option", v, ok)
	}

	const want = "12.5 340 32 1789727000 claude"
	if out, err := tmux("set-option", "-t", "w", "@og_session_res", want).CombinedOutput(); err != nil {
		t.Fatalf("set @og_session_res: %v\n%s", err, out)
	}

	got := waitForLine(t, lines, func(l string) bool {
		return strings.HasPrefix(l, prefix) && strings.HasSuffix(l, want)
	})
	// The session-scoped shape: a session id and three '-' where a window or
	// pane subscription names its object. A wrong `what` breaks exactly this,
	// silently.
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `\$\d+ - - - : `).MatchString(got) {
		t.Errorf("line = %q, want a session-scoped '%s$N - - - : ' report", got, prefix)
	}
	if v, ok := subscriptionValue(controlmode.ParseLine(got), resSubName); !ok || v != want {
		t.Errorf("value = %q, %v; want %q", v, ok, want)
	}
}

// waitForLine returns the first control-stream line matching ok. The budget is
// a stall detector, not a race to beat: a parallel `go test ./...` on a loaded
// builder starves a real tmux server well past a second.
func waitForLine(t *testing.T, lines <-chan string, ok func(string) bool) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case l, more := <-lines:
			if !more {
				t.Fatal("control client closed its stream before the line arrived")
			}
			if ok(l) {
				return l
			}
		case <-deadline:
			t.Fatal("timed out waiting for a subscription-changed line")
		}
	}
}
