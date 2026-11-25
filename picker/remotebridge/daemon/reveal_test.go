package daemon

import (
	"errors"
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
		"1|ctl|100|@1|focused|5",  // control-mode: skipped
		"",                        // blank: skipped
		"0|bad|only-three",        // malformed (3 fields): skipped
		"0|tty0|200|@7|focused|5", // kept
		"0|tty1|300|@8",           // kept: views read only the first four fields
	}, "\n") + "\n"

	got := clientViews(out)
	want := map[string]string{"tty0|200": "@7", "tty1|300": "@8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clientViews = %v, want %v", got, want)
	}
}

func TestActivityRows(t *testing.T) {
	out := strings.Join([]string{
		"0|tty0|1|@7|focused|100|80|24|attached",
		"0|tty1|2|@7|active,focused,UTF-8|101|80|24|attached",
		"0|tty2|3|@7|active,UTF-8|102|80|24|attached",
		"0|tty3|4|@7|unfocused|103|80|24|attached",
		"1|ctl|5|@7|focused|104|80|24|attached",
		"",
		"0|tty4|6|@7|focused|not-a-number|80|24|attached",
		"0|tty5|7|@7|focused||80|24|attached",
		"0|tty6|8|@7|focused|105|80|24", // eight fields: missing the geometry tail's status (#1266)
		"0|tty7|9|@7|focused|106|80|24|attached",
	}, "\n") + "\n"

	got := activityRows(out)
	want := []activityRow{{true, 100, "@7"}, {true, 101, "@7"}, {false, 102, "@7"}, {false, 103, "@7"}, {true, 106, "@7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activityRows = %v, want %v", got, want)
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
// TestWatchLocalClientReconvergesOnChange drives watchLocalClient: query reads
// from a channel the test controls, and each unbuffered send is a barrier
// proving the previous tick fully completed before the next begins.
func TestWatchRevealQueuesMirrorWindowsAndWakes(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	queryCh := make(chan string)
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
		watchReveal(query, isMirror, false, q, wake, nil, nil, stop, tick)
		close(done)
	}()
	step := func(out string) {
		t.Helper()
		tick <- time.Now()
		queryCh <- out
	}
	drain := func(view string, want []string, wantWakes int) {
		t.Helper()
		// One more tick repeating the current view proves the previous tick's
		// queueing and wake completed, without itself revealing anything.
		tick <- time.Now()
		queryCh <- view
		if got := q.take(); !reflect.DeepEqual(got, want) {
			t.Fatalf("queued = %v, want %v", got, want)
		}
		mu.Lock()
		defer mu.Unlock()
		if wakes != wantWakes {
			t.Fatalf("wakes = %d, want %d", wakes, wantWakes)
		}
	}

	row := "0|tty0|1|@7|focused|5|80|24|attached\n"
	// No clients attached at all: an empty read reveals nothing.
	step("")
	// A control-mode client and a non-mirror window queue nothing and don't wake.
	step(strings.Join([]string{"1|ctl|1|@9|focused|5|80|24|attached", "0|tty0|1|@3|focused|5|80|24|attached"}, "\n") + "\n")
	// The same client switches onto @7, a mirror window: queued once, one wake.
	step(row)
	// Unchanged view set: nothing new.
	step(row)
	drain(row, []string{"@7"}, 1)

	// The client leaves the mirror session and comes back to the same window.
	// No hook need fire for either move: polling every tick sees the view
	// disappear and reappear, which is a reveal.
	step("")
	step(row)
	drain(row, []string{"@7"}, 2)

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReveal did not return after stop was closed")
	}
}

// TestWatchRevealReportsActivity pins that the activity callback gets each
// successful poll's rows with a now taken before the query ran.
func TestWatchRevealReportsActivity(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	var queryAt int64
	query := func() (string, error) {
		queryAt = time.Now().Unix()
		return "0|tty0|1|@7|focused|42|80|24|attached\n", nil
	}
	type call struct {
		rows []activityRow
		now  int64
	}
	calls := make(chan call)
	activity := func(rows []activityRow, now int64) { calls <- call{rows, now} }

	done := make(chan struct{})
	go func() {
		watchReveal(query, func(string) bool { return false }, false, &revealQueue{}, func() bool { return true }, activity, nil, stop, tick)
		close(done)
	}()

	before := time.Now().Unix()
	tick <- time.Now()
	got := <-calls
	if want := []activityRow{{true, 42, "@7"}}; !reflect.DeepEqual(got.rows, want) {
		t.Fatalf("rows = %v, want %v", got.rows, want)
	}
	if got.now < before || got.now > queryAt {
		t.Fatalf("now = %d, want within [%d, query time %d]", got.now, before, queryAt)
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchReveal did not return after stop was closed")
	}
}

// A placed host session holds local windows and other daemons' mirrors too.
// Typing in one of those must not reach size ownership, or every daemon in the
// session would claim the remote's size on any keystroke there (#979). A
// dedicated session's clients all show this daemon's mirrors, so it keeps
// every row.
func TestWatchRevealPlacedActivityOnlyFromMirrors(t *testing.T) {
	out := "0|tty0|1|@3|focused|40|80|24|attached\n0|tty1|2|@7|focused|41|80|24|attached\n0|tty2|3|@9|focused|42|80|24|attached\n"
	isMirror := func(win string) bool { return win == "@7" }
	for _, tc := range []struct {
		name   string
		placed bool
		want   []activityRow
	}{
		{"placed keeps only own mirror rows", true, []activityRow{{true, 41, "@7"}}},
		{"dedicated keeps every row", false, []activityRow{{true, 40, "@3"}, {true, 41, "@7"}, {true, 42, "@9"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tick := make(chan time.Time)
			stop := make(chan struct{})
			calls := make(chan []activityRow)
			activity := func(rows []activityRow, _ int64) { calls <- rows }
			done := make(chan struct{})
			go func() {
				watchReveal(func() (string, error) { return out, nil }, isMirror, tc.placed, &revealQueue{}, func() bool { return true }, activity, nil, stop, tick)
				close(done)
			}()

			tick <- time.Now()
			if got := <-calls; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rows = %v, want %v", got, tc.want)
			}

			close(stop)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("watchReveal did not return after stop was closed")
			}
		})
	}
}

func TestClientGeomsSkipsControlAndMalformed(t *testing.T) {
	out := strings.Join([]string{
		"1|ctl|1|@9|focused|5|80|24|attached",    // control-mode: skipped
		"",                                       // blank: skipped
		"0|bad|only-three",                       // malformed: skipped
		"0|tty0|100|@7|focused|5|80|24|attached", // kept
		"0|tty1|200|@7|focused|5|120|40|detached", // kept
	}, "\n") + "\n"

	got := clientGeoms(out)
	want := map[string]string{
		"tty0|100": "80|24|attached",
		"tty1|200": "120|40|detached",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clientGeoms = %v, want %v", got, want)
	}
}

func TestGeomSignalTakeConsumes(t *testing.T) {
	var g geomSignal
	if g.take() {
		t.Fatal("take on an unmarked signal reported a mark")
	}
	g.mark()
	if !g.take() {
		t.Fatal("take did not report the mark")
	}
	if g.take() {
		t.Fatal("take did not consume the mark")
	}

	var nilGeom *geomSignal
	nilGeom.mark() // must not panic
	if nilGeom.take() {
		t.Fatal("a nil geomSignal reported a mark")
	}
}

// revealHarness drives watchReveal through a channel-backed query and a real
// geomSignal, with the watcher's next query as the barrier after each poll.
type revealHarness struct {
	t           *testing.T
	ticker      chan time.Time
	stop        chan struct{}
	queryCh     chan queryResult
	queryCalled chan struct{}
	done        chan struct{}
	geom        *geomSignal
}

type queryResult struct {
	out string
	err error
}

func newRevealHarness(t *testing.T) *revealHarness {
	t.Helper()
	h := &revealHarness{
		t:           t,
		ticker:      make(chan time.Time),
		stop:        make(chan struct{}),
		queryCh:     make(chan queryResult),
		queryCalled: make(chan struct{}),
		done:        make(chan struct{}),
		geom:        &geomSignal{},
	}
	query := func() (string, error) {
		h.queryCalled <- struct{}{}
		r := <-h.queryCh
		return r.out, r.err
	}
	go func() {
		watchReveal(query, func(string) bool { return false }, false, &revealQueue{}, func() bool { return true }, nil, h.geom, h.stop, h.ticker)
		close(h.done)
	}()
	return h
}

// start begins the first poll and waits for the watcher to ask for its result.
func (h *revealHarness) start() {
	h.t.Helper()
	h.ticker <- time.Now()
	<-h.queryCalled
}

// poll delivers a successful result, then starts the next poll and blocks
// until the watcher has fully processed the result — it has looped back to its
// select and asked for the next query. The watcher is left waiting on that
// query's result, so the caller inspects the signal and then either polls
// again or closes.
func (h *revealHarness) poll(out string) {
	h.t.Helper()
	h.pollErr(out, nil)
}

// pollErr is poll with a failed query, which the watcher skips.
func (h *revealHarness) pollErr(out string, err error) {
	h.t.Helper()
	h.queryCh <- queryResult{out: out, err: err}
	h.ticker <- time.Now()
	<-h.queryCalled
}

func (h *revealHarness) close() {
	h.t.Helper()
	// Release the query the last poll left waiting; the watcher processes the
	// empty result and returns to select, where stop releases it.
	h.queryCh <- queryResult{}
	close(h.stop)
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.t.Fatal("watchReveal did not return after stop was closed")
	}
}

// TestWatchRevealMarksGeometryChanges pins the resize watcher's second
// trigger (#1266): the first successful poll marks, an identical poll does
// not, and a width, height or status change does.
func TestWatchRevealMarksGeometryChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		cur  string
		mark bool
	}{
		{"identical does not mark", "80|24|attached", false},
		{"width marks", "81|24|attached", true},
		{"height marks", "80|25|attached", true},
		{"status marks", "80|24|detached", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRevealHarness(t)
			defer h.close()
			h.start()
			h.poll("0|tty0|1|@7|focused|42|80|24|attached\n")
			if !h.geom.take() {
				t.Fatal("the first successful poll did not mark")
			}
			h.poll("0|tty0|1|@7|focused|42|" + tc.cur + "\n")
			if got := h.geom.take(); got != tc.mark {
				t.Fatalf("mark after the second poll = %v, want %v", got, tc.mark)
			}
		})
	}
}

func TestWatchRevealMarksAttachAndDetach(t *testing.T) {
	rowA := "0|tty0|1|@7|focused|42|80|24|attached\n"
	rowB := "0|tty1|2|@8|focused|43|80|24|attached\n"
	t.Run("attach", func(t *testing.T) {
		h := newRevealHarness(t)
		defer h.close()
		h.start()
		h.poll(rowA)
		h.geom.take()
		h.poll(rowA + rowB)
		if !h.geom.take() {
			t.Fatal("a client attach did not mark")
		}
	})
	t.Run("detach", func(t *testing.T) {
		h := newRevealHarness(t)
		defer h.close()
		h.start()
		h.poll(rowA + rowB)
		h.geom.take()
		h.poll(rowA)
		if !h.geom.take() {
			t.Fatal("a client detach did not mark")
		}
	})
}

// A failed query neither marks nor resets the baseline: the poll after it sees
// the same geometry and stays quiet.
func TestWatchRevealFailedQueryKeepsBaseline(t *testing.T) {
	h := newRevealHarness(t)
	defer h.close()
	row := "0|tty0|1|@7|focused|42|80|24|attached\n"
	h.start()
	h.poll(row)
	if !h.geom.take() {
		t.Fatal("the first successful poll did not mark")
	}
	h.pollErr("", errors.New("boom"))
	if h.geom.take() {
		t.Fatal("a failed query marked")
	}
	h.poll(row)
	if h.geom.take() {
		t.Fatal("a failed query reset the baseline, so the identical poll read as a change")
	}
}

// A control-mode row is not a view, so a change only in its geometry must not
// mark.
func TestWatchRevealIgnoresControlRows(t *testing.T) {
	h := newRevealHarness(t)
	defer h.close()
	h.start()
	h.poll("1|ctl|1|@9|focused|42|80|24|attached\n")
	h.geom.take()
	h.poll("1|ctl|1|@9|focused|42|81|25|detached\n")
	if h.geom.take() {
		t.Fatal("a control-mode row's geometry change marked")
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
		defer func() { _ = local.Close() }()
		defer func() { _ = peer.Close() }()

		p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
		sink := newOutputSink(local, p)
		_, _ = sink.Write(testKittyStore("9"))
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
			"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1",
			"%begin 1 2 1", "FRESH-CAPTURE", "%end 1 2 1",
		}, "\n") + "\n")

		reseedRevealed(reg, router, rt, reveals)

		_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
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
		defer func() { _ = local.Close() }()
		defer func() { _ = peer.Close() }()

		sink := newOutputSink(local, nil) // no graphics proxy: hasImages stays false
		router := NewRouter()
		router.Register("%1", sink)

		reg := newRegistry()
		mw := reg.add("@1", "@7")
		mw.remotePanes = []string{"%1"}

		reveals := &revealQueue{}
		reveals.add("@7")

		rt := func(cmds ...string) replies {
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
