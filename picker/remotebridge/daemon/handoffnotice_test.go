package daemon

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func TestNotifyClients(t *testing.T) {
	const listing = "0|/dev/pts/1|@1\n1|ctl-client|@1\n0|/dev/pts/2|@9\nmalformed\n0||@1\n"
	ownOne := func(win string) bool { return win == "@1" }
	tests := []struct {
		name    string
		out     string
		outErr  error
		own     func(string) bool
		wantRun [][]string
	}{
		{
			name: "dedicated notifies every terminal client",
			out:  listing,
			wantRun: [][]string{
				{"display-message", "-d", "5000", "-c", "/dev/pts/1", "msg"},
				{"display-message", "-d", "5000", "-c", "/dev/pts/2", "msg"},
			},
		},
		{
			name: "placed notifies only clients on an own window",
			out:  listing,
			own:  ownOne,
			wantRun: [][]string{
				{"display-message", "-d", "5000", "-c", "/dev/pts/1", "msg"},
			},
		},
		{name: "list-clients error runs nothing", out: listing, outErr: errors.New("no server")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotOut, gotRun [][]string
			out := func(a ...string) (string, error) {
				gotOut = append(gotOut, a)
				return tc.out, tc.outErr
			}
			run := func(a ...string) error {
				gotRun = append(gotRun, a)
				return nil
			}
			notifyClients(out, run, "$3", "msg", tc.own)
			wantOut := [][]string{{"list-clients", "-t", "$3", "-F", "#{client_control_mode}|#{client_name}|#{window_id}"}}
			if !reflect.DeepEqual(gotOut, wantOut) {
				t.Errorf("query = %q, want %q", gotOut, wantOut)
			}
			if !reflect.DeepEqual(gotRun, tc.wantRun) {
				t.Errorf("run = %q, want %q", gotRun, tc.wantRun)
			}
		})
	}
}

func TestHandoffWatch(t *testing.T) {
	now := time.Unix(1000, 0)
	w := &handoffWatch{now: func() time.Time { return now }}

	if w.take() {
		t.Error("take on a zero watch = true")
	}
	w.arm()
	if !w.take() {
		t.Error("take after arm = false")
	}
	if w.take() {
		t.Error("take twice = true, want it cleared")
	}

	w.arm()
	w.disarm()
	if w.take() {
		t.Error("take after disarm = true")
	}

	w.arm()
	now = now.Add(handoffWindow + time.Second)
	if w.take() {
		t.Error("take after expiry = true")
	}
	w.arm()
	now = now.Add(handoffWindow)
	if !w.take() {
		t.Error("take at the window edge = false")
	}
}

// One goroutine arms (the ctl handler) while another takes (the main loop);
// the race detector is the assertion, plus each arm yielding at most one take.
func TestHandoffWatchConcurrent(t *testing.T) {
	var w handoffWatch
	const n = 500
	var wg sync.WaitGroup
	var mu sync.Mutex
	taken := 0
	wg.Go(func() {
		for range n {
			w.arm()
		}
	})
	wg.Go(func() {
		for range n {
			if w.take() {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		}
	})
	wg.Wait()
	if w.take() {
		taken++
	}
	if taken < 1 || taken > n {
		t.Errorf("taken = %d, want within [1, %d]", taken, n)
	}
}

func TestParseCtlArmsHandoff(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{"handoff arms", []string{wire.CtlProtocolVersion, "handoff", "%3", "/d", hoPR}, true},
		{"another verb does not", []string{wire.CtlProtocolVersion, "dwindle-toggle", "%3"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := c.parseCtl(tc.argv, "rem")
			if err != nil {
				t.Fatalf("parseCtl: %v", err)
			}
			if req.armsHandoff != tc.want {
				t.Errorf("armsHandoff = %v, want %v", req.armsHandoff, tc.want)
			}
		})
	}
}
