package daemon

import (
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func TestClientViewsSkipsControlAndMalformed(t *testing.T) {
	out := strings.Join([]string{
		"1|ctl|100|@1",     // control-mode: skipped
		"",                 // blank: skipped
		"0|bad|only-three", // malformed (3 fields): skipped
		"0|tty0|200|@7",    // kept
	}, "\n") + "\n"

	got := clientViews(out)
	want := map[string]string{"tty0|200": "@7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clientViews = %v, want %v", got, want)
	}
}

func TestRevealedWindows(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur map[string]string
		want      []string
	}{
		{"none to one reveals the view's window", nil, map[string]string{"a|1": "@7"}, []string{"@7"}},
		{"unchanged set reveals nothing", map[string]string{"a|1": "@7"}, map[string]string{"a|1": "@7"}, nil},
		{"same name, new created reveals again", map[string]string{"a|1": "@7"}, map[string]string{"a|2": "@7"}, []string{"@7"}},
		{"window change reveals the new window only", map[string]string{"a|1": "@1"}, map[string]string{"a|1": "@2"}, []string{"@2"}},
		{"view leaving reveals nothing", map[string]string{"a|1": "@7"}, map[string]string{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := revealedWindows(c.prev, c.cur)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("revealedWindows(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
			}
		})
	}
}

// TestWatchRevealQueuesMirrorWindowsAndWakes drives watchReveal the same way
// TestWatchLocalClientReconvergesOnChange drives watchLocalClient: nudged and
// query read from channels the test controls, and each unbuffered send is a
// barrier proving the previous tick fully completed before the next begins.
func TestWatchRevealQueuesMirrorWindowsAndWakes(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudgeCh := make(chan nudgeResult)
	queryCh := make(chan string)
	nudged := func() (time.Time, bool) { n := <-nudgeCh; return n.t, n.ok }
	query := func() (string, error) { return <-queryCh, nil }

	q := &revealQueue{}
	var mu sync.Mutex
	wakes := 0
	wake := func() bool {
		mu.Lock()
		defer mu.Unlock()
		wakes++
		return true
	}
	isMirror := func(win string) bool { return win == "@7" }

	done := make(chan struct{})
	go func() {
		watchReveal(nudged, query, isMirror, q, wake, stop, tick)
		close(done)
	}()

	// Tick 1 — nudge file not yet touched: no due, so query must not be
	// called (nothing sent on queryCh; a wrongly-issued query would deadlock
	// here rather than race silently).
	tick <- time.Now()
	nudgeCh <- nudgeResult{ok: false}

	t1 := time.Now()
	// Tick 2 — first touch, but no clients attached at all: an empty read
	// reveals nothing.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t1, ok: true}
	queryCh <- ""

	t2 := t1.Add(time.Second)
	// Tick 3 — mtime advanced: a control-mode client and a non-mirror window
	// are both present, but neither queues anything or wakes.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t2, ok: true}
	queryCh <- strings.Join([]string{"1|ctl|1|@9", "0|tty0|1|@3"}, "\n") + "\n"

	t3 := t2.Add(time.Second)
	// Tick 4 — the same client switches onto @7, a mirror window: queued
	// once, wake called once.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t3, ok: true}
	queryCh <- "0|tty0|1|@7\n"

	t4 := t3.Add(time.Second)
	// Tick 5 — unchanged view set: nothing new queued, no further wake.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t4, ok: true}
	queryCh <- "0|tty0|1|@7\n"

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReveal did not return after stop was closed")
	}

	if got := q.take(); !reflect.DeepEqual(got, []string{"@7"}) {
		t.Fatalf("queued = %v, want [@7]", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if wakes != 1 {
		t.Fatalf("wakes = %d, want 1", wakes)
	}
}

// TestReseedRevealedReplaysBeforeSeed is the regression test for the user
// symptom (#731): a mirror pane holding a retained kitty store, revealed by a
// local attach/switch, must have its store replayed BEFORE the fresh seed
// that repaints the placeholder over it — otherwise the placeholder resolves
// to nothing and the carousel never repaints.
func TestReseedRevealedReplaysBeforeSeed(t *testing.T) {
	t.Run("retained store replays before the fresh seed", func(t *testing.T) {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()

		p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
		sink := newOutputSink(local, p)
		sink.Write(testKittyStore("9"))
		storeFrame, err := wire.ReadFrame(peer)
		if err != nil {
			t.Fatalf("read store: %v", err)
		}
		if storeFrame.Type != wire.FrameOutput || !strings.Contains(string(storeFrame.Payload), kittyLocalisedMarker) {
			t.Fatalf("store frame = %v %q", storeFrame.Type, storeFrame.Payload)
		}

		router := NewRouter()
		router.Register("%1", sink)

		reg := newRegistry()
		mw := reg.add("@1", "@7")
		mw.remotePanes = []string{"%1"}

		reveals := &revealQueue{}
		reveals.add("@7")

		rt, _ := scriptedRT(strings.Join([]string{
			"%begin 1 1 1", "0 0 0 0", "%end 1 1 1",
			"%begin 1 2 1", "FRESH-CAPTURE", "%end 1 2 1",
		}, "\n") + "\n")

		reseedRevealed(reg, router, rt, reveals)

		peer.SetDeadline(time.Now().Add(5 * time.Second))
		replay, seed := replayThenSeedFrames(t, peer)
		if replay.Type != wire.FrameOutput || !strings.Contains(string(replay.Payload), kittyLocalisedMarker) {
			t.Fatalf("replay = %v %q, want retained localised store", replay.Type, replay.Payload)
		}
		if seed.Type != wire.FrameSeed || !strings.Contains(string(seed.Payload), "FRESH-CAPTURE") {
			t.Fatalf("seed = %v %q", seed.Type, seed.Payload)
		}
	})

	t.Run("no retained store issues no capture", func(t *testing.T) {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()

		sink := newOutputSink(local, nil) // no graphics proxy: hasImages stays false
		router := NewRouter()
		router.Register("%1", sink)

		reg := newRegistry()
		mw := reg.add("@1", "@7")
		mw.remotePanes = []string{"%1"}

		reveals := &revealQueue{}
		reveals.add("@7")

		var rt roundTrip = func(cmds ...string) replies {
			t.Fatal("reseedRevealed issued a round-trip with no retained store")
			return nil
		}

		reseedRevealed(reg, router, rt, reveals)

		// The reader must not be consumed: nothing was ever written to it, so
		// a short quiet deadline is enough to prove that rather than block.
		if got := readAllFrames(t, peer, 200*time.Millisecond); got != "" {
			t.Fatalf("peer read %q, want nothing written", got)
		}
	})
}
