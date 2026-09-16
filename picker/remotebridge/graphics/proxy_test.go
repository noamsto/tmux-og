package graphics

import (
	"bytes"
	"context"
	"encoding/base64"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/keyneg"
)

func TestProxyRewritesAndWrapsInOnePass(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	out := string(p.Filter([]byte("hello " + bareSeq + " bye")))
	if !strings.HasPrefix(out, "hello ") || !strings.HasSuffix(out, " bye") {
		t.Fatalf("literals lost: %q", out)
	}
	if countSub(out, passStart) != 1 {
		t.Fatalf("want exactly one wrapper: %q", out)
	}
	if !strings.Contains(out, "L2xvY2FsL2EuYmlu") { // base64("/local/a.bin")
		t.Fatalf("payload not localised: %q", out)
	}
}

func TestProxyDropsWhatItCannotLocalise(t *testing.T) {
	var logged int
	p := New(&fakeLocalizer{err: errFake}, func(string, ...any) { logged++ })
	out := string(p.Filter([]byte("x" + bareSeq + "y")))
	if out != "xy" {
		t.Fatalf("out = %q, want the literals only", out)
	}
	if logged == 0 {
		t.Fatal("a dropped sequence must be logged")
	}
}

func TestProxyCarriesPartialSequencesAcrossCalls(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	cut := len(bareSeq) / 2
	if got := string(p.Filter([]byte(bareSeq[:cut]))); got != "" {
		t.Fatalf("emitted a partial sequence: %q", got)
	}
	if got := string(p.Filter([]byte(bareSeq[cut:]))); countSub(got, passStart) != 1 {
		t.Fatalf("second half did not complete the sequence: %q", got)
	}
}

// Close must flush a held partial rather than lose it: the pane can exit with
// a sequence cut mid-stream, and the trailing bytes are still real output.
func TestProxyCloseFlushesAHeldPartial(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	cut := len(bareSeq) / 2
	if got := string(p.Filter([]byte("x" + bareSeq[:cut]))); got != "x" {
		t.Fatalf("literal before the partial was withheld: %q", got)
	}
	if got := string(p.Close()); got != bareSeq[:cut] {
		t.Fatalf("Close() = %q, want the held partial %q", got, bareSeq[:cut])
	}
}

// blockingLocalizer never returns on its own — it exercises the D4 timeout
// path directly by returning only once its context is cancelled.
type blockingLocalizer struct{}

func (blockingLocalizer) Localize(ctx context.Context, remote string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// A fetch that never returns on its own must not freeze the pane forever
// (spec D4): Filter has to come back within its bound, with the store dropped
// down the same path as any other unlocalisable one and the surrounding
// literals still forwarded — the stream resumes, it doesn't just abort.
func TestFilterDropsAFetchThatOutrunsItsDeadline(t *testing.T) {
	var logged int
	p := New(blockingLocalizer{}, func(string, ...any) { logged++ })
	p.timeout = 20 * time.Millisecond

	done := make(chan []byte, 1)
	go func() { done <- p.Filter([]byte("x" + bareSeq + "y")) }()

	select {
	case got := <-done:
		if string(got) != "xy" {
			t.Fatalf("out = %q, want the literals only (store dropped)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Filter did not return within a bounded time")
	}
	if logged == 0 {
		t.Fatal("a timed-out fetch must be logged as a drop")
	}
}

// The localise-or-drop rule (D7) is a security boundary, not just an image
// policy: a t=f payload names a path, and the only paths the local terminal may
// be handed are ones the fetcher wrote. Each input below is a kitty store the
// scanner used to hand on verbatim, carrying the sender's own path straight
// through — so each asserts the payload does not appear in Filter's output, and
// that the localiser was never even consulted (a consulted-and-failed fetch is
// the ordinary drop path, already covered above).
func TestProxyNeverForwardsAnUnlocalisedPath(t *testing.T) {
	// base64("/etc/passwd"), the payload a sender would smuggle.
	const secret = "L2V0Yy9wYXNzd2Q="
	store := "\x1b_Gi=1,a=T,t=f;" + secret + "\x1b\\"

	// wrap doubles the inner ESCs, as a tmux passthrough does, and appends an
	// optional trailer inside the wrapper.
	wrap := func(inner, trailer string) string {
		var b strings.Builder
		b.WriteString("\x1bPtmux;")
		for i := 0; i < len(inner); i++ {
			if inner[i] == 0x1b {
				b.WriteByte(0x1b)
			}
			b.WriteByte(inner[i])
		}
		b.WriteString(trailer)
		b.WriteString("\x1b\\")
		return b.String()
	}

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"trailing byte inside the wrapper", wrap(store, "X")},
		{"inner APC left unterminated", wrap("\x1b_Gi=1,a=T,t=f;"+secret, "")},
		{"duplicate t= key, bare", "\x1b_Gi=1,a=T,t=d,t=f;" + secret + "\x1b\\"},
		{"duplicate t= key, wrapped", wrap("\x1b_Gi=1,a=T,t=d,t=f;"+secret+"\x1b\\", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := &fakeLocalizer{local: "/local/a.bin"}
			p := New(loc, nil)
			out := string(p.Filter([]byte(tc.in)))
			if strings.Contains(out, secret) {
				t.Fatalf("forwarded the sender's own path: %q", out)
			}
			if len(loc.asked) != 0 {
				t.Fatalf("localizer asked %v, want untouched — these drop before any fetch", loc.asked)
			}
		})
	}
}

// The forward-verbatim path must survive: a passthrough carrying an escape that
// is none of ours has no payload to localise, and swallowing it would break
// every non-graphics user of passthrough (OSC 52 and friends).
func TestProxyStillForwardsANonGraphicsPassthrough(t *testing.T) {
	const in = "\x1bPtmux;\x1b\x1b]52;c;aGk=\x1b\x1b\\\x1b\\"
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	if got := string(p.Filter([]byte(in))); got != in {
		t.Fatalf("forwarded %q, want byte-identical %q", got, in)
	}
}

func TestProxyReplayEmptyUntilStoreRetained(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	if got := p.Replay(); len(got) != 0 {
		t.Fatalf("Replay() = %q before any store, want empty", got)
	}
}

func TestProxyRetainsLocalisedStoreForReplay(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	out := p.Filter([]byte(bareSeq))
	if len(out) == 0 {
		t.Fatal("Filter dropped the store")
	}
	replay := p.Replay()
	if len(replay) == 0 {
		t.Fatal("Replay() empty after a localised store passed through")
	}
	if countSub(string(replay), passStart) != 1 {
		t.Fatalf("Replay() = %q, want one wrapped store", replay)
	}
	if !strings.Contains(string(replay), "L2xvY2FsL2EuYmlu") {
		t.Fatalf("Replay() not localised: %q", replay)
	}
}

func TestProxyDeleteEvictsReplayState(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	p.Filter([]byte(bareSeq))
	del := "\x1b_Ga=d,i=31\x1b\\"
	p.Filter([]byte(del))
	if got := p.Replay(); len(got) != 0 {
		t.Fatalf("Replay() = %q after delete, want empty", got)
	}
}

func TestProxyBulkDeleteClearsReplayState(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	p.Filter([]byte("\x1b_Gi=1,a=T,t=f;L3RtcC94LnBuZw==\x1b\\"))
	p.Filter([]byte("\x1b_Gi=2,a=T,t=f;L3RtcC94LnBuZw==\x1b\\"))
	p.Filter([]byte("\x1b_Ga=d,d=A\x1b\\"))
	if got := p.Replay(); len(got) != 0 {
		t.Fatalf("Replay() = %q after bulk delete, want empty", got)
	}
}

func TestProxySameIDKeepsNewestForReplay(t *testing.T) {
	loc := &seqLocalizer{locals: []string{"/local/old.bin", "/local/new.bin"}}
	p := New(loc, nil)
	p.Filter([]byte("\x1b_Gi=7,a=T,t=f;L3RtcC9hLnBuZw==\x1b\\"))
	p.Filter([]byte("\x1b_Gi=7,a=T,t=f;L3RtcC9iLnBuZw==\x1b\\"))
	replay := string(p.Replay())
	if strings.Contains(replay, "L2xvY2FsL29sZC5iaW4=") { // /local/old.bin
		t.Fatalf("kept stale same-id store: %q", replay)
	}
	if !strings.Contains(replay, "L2xvY2FsL25ldy5iaW4=") { // /local/new.bin
		t.Fatalf("missing newest same-id store: %q", replay)
	}
	if countSub(replay, passStart) != 1 {
		t.Fatalf("Replay() = %q, want one retained store for the id", replay)
	}
}

// seqLocalizer returns locals[i] on the i-th Localize call (clamped to last).
type seqLocalizer struct {
	locals []string
	n      int
}

func (s *seqLocalizer) Localize(context.Context, string) (string, error) {
	i := s.n
	if i >= len(s.locals) {
		i = len(s.locals) - 1
	}
	s.n++
	return s.locals[i], nil
}

// batchLocalizer records each LocalizeBatch call's paths and answers
// local="/local/<basename>".
type batchLocalizer struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (b *batchLocalizer) Localize(ctx context.Context, remote string) (string, error) {
	locals, errs := b.LocalizeBatch(ctx, []string{remote})
	return locals[0], errs[0]
}

func (b *batchLocalizer) LocalizeBatch(_ context.Context, remotes []string) ([]string, []error) {
	b.mu.Lock()
	b.calls = append(b.calls, append([]string(nil), remotes...))
	b.mu.Unlock()
	locals := make([]string, len(remotes))
	errs := make([]error, len(remotes))
	for i, r := range remotes {
		if b.err != nil {
			errs[i] = b.err
			continue
		}
		locals[i] = "/local/" + path.Base(r)
	}
	return locals, errs
}

func storeSeq(id, remotePath string) string {
	return "\x1b_Gi=" + id + ",a=T,t=f;" + base64.StdEncoding.EncodeToString([]byte(remotePath)) + "\x1b\\"
}

// The #556 shape: one batch carrying a preview store plus filmstrip
// thumbnails must reach the localizer as ONE batch call holding every
// distinct path, not as N serialized Localize calls.
func TestProxyFetchesABatchInOneCall(t *testing.T) {
	loc := &batchLocalizer{}
	p := New(loc, nil)
	in := "pre " + storeSeq("1", "/tmp/preview.png") + storeSeq("2", "/tmp/thumb1.png") +
		storeSeq("3", "/tmp/thumb2.png") + storeSeq("4", "/tmp/thumb1.png") + " post"
	out := string(p.Filter([]byte(in)))
	if len(loc.calls) != 1 {
		t.Fatalf("LocalizeBatch calls = %d, want 1", len(loc.calls))
	}
	got := loc.calls[0]
	want := []string{"/tmp/preview.png", "/tmp/thumb1.png", "/tmp/thumb2.png"}
	if !slices.Equal(got, want) {
		t.Fatalf("batch paths = %v, want %v (deduped, in order)", got, want)
	}
	if !strings.HasPrefix(out, "pre ") || !strings.HasSuffix(out, " post") {
		t.Fatalf("literals lost: %q", out)
	}
	if countSub(out, passStart) != 4 {
		t.Fatalf("want all four stores forwarded (the dup re-localised): %q", out)
	}
	if !strings.Contains(out, base64.StdEncoding.EncodeToString([]byte("/local/thumb1.png"))) {
		t.Fatalf("payloads not localised: %q", out)
	}
}

// A batch-capable localizer that hangs must still cost the pane only ONE
// timeout for the whole batch, not one per store.
func TestProxyBatchSharesOneDeadline(t *testing.T) {
	loc := &batchLocalizer{err: context.DeadlineExceeded}
	p := New(loc, nil)
	p.timeout = 20 * time.Millisecond
	in := storeSeq("1", "/tmp/a.png") + storeSeq("2", "/tmp/b.png") + storeSeq("3", "/tmp/c.png")
	start := time.Now()
	if out := string(p.Filter([]byte(in))); out != "" {
		t.Fatalf("out = %q, want every store dropped", out)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Filter held the stream %v for one batch, want ~20ms", d)
	}
}

func TestProxyRetentionIsPerInstance(t *testing.T) {
	a := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	b := New(&fakeLocalizer{local: "/local/b.bin"}, nil)
	a.Filter([]byte(bareSeq))
	if len(b.Replay()) != 0 {
		t.Fatal("retention leaked across proxy instances")
	}
	if len(a.Replay()) == 0 {
		t.Fatal("store missing from owning proxy")
	}
}

func TestProxyRetentionCapEvictsOldestID(t *testing.T) {
	p := New(&fakeLocalizer{local: "/local/a.bin"}, nil)
	p.retainCap = 2
	store := func(id string) string {
		return "\x1b_Gi=" + id + ",a=T,t=f;L3RtcC94LnBuZw==\x1b\\"
	}
	p.Filter([]byte(store("1")))
	p.Filter([]byte(store("2")))
	p.Filter([]byte(store("3")))
	replay := string(p.Replay())
	if strings.Contains(replay, "i=1") {
		t.Fatalf("oldest id survived cap: %q", replay)
	}
	if countSub(replay, passStart) != 2 {
		t.Fatalf("Replay() = %q, want two retained stores", replay)
	}
}

// relaySrc builds a RelaySource seeded from termfeatures, sparing every
// caller that only wants a fixed capability from writing NewRelaySource(
// NewRelayFromClient(...)) out in full.
func relaySrc(feats string) *RelaySource {
	return NewRelaySource(NewRelayFromClient(strings.Contains(feats, "sixel"), feats))
}

// The gate on and off against the same input (R1/R6): a complete bare sixel
// forwarded when the local client carries the sixel terminal-feature, dropped
// otherwise.
func TestProxyRelayGateForwardsOrDropsSixel(t *testing.T) {
	const sixel = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
	t.Run("gate on forwards it bare", func(t *testing.T) {
		p := NewRelay(&fakeLocalizer{}, nil, relaySrc("bpaste,sixel"), DefaultRasterHold)
		if got := string(p.Filter([]byte("x" + sixel + "y"))); got != "x"+sixel+"y" {
			t.Fatalf("out = %q, want the sixel forwarded bare", got)
		}
	})
	t.Run("gate off drops it", func(t *testing.T) {
		p := NewRelay(&fakeLocalizer{}, nil, relaySrc("bpaste"), DefaultRasterHold)
		if got := string(p.Filter([]byte("x" + sixel + "y"))); got != "xy" {
			t.Fatalf("out = %q, want the sixel dropped", got)
		}
	})
}

// A raster chunk must never end up in Replay() output (spec non-goal): it is
// cursor-positioned, and replaying one after a reseed would paint it wrong.
func TestProxyRasterNeverRetainedForReplay(t *testing.T) {
	const sixel = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
	p := NewRelay(&fakeLocalizer{}, nil, relaySrc("sixel"), DefaultRasterHold)
	if got := string(p.Filter([]byte(sixel))); got != sixel {
		t.Fatalf("out = %q, want the sixel forwarded", got)
	}
	if got := p.Replay(); len(got) != 0 {
		t.Fatalf("Replay() = %q, want a raster never retained", got)
	}
}

// The relay-off drop log fires once per pane, not once per sequence — a
// viewer repaints at frame rate, so a per-sequence line at multi-MB rates is
// spam.
func TestProxyRelayOffLogsSixelDropOncePerPane(t *testing.T) {
	const sixel = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
	var logged int
	p := NewRelay(&fakeLocalizer{}, func(string, ...any) { logged++ }, nil, 0)
	p.Filter([]byte(sixel + sixel + sixel))
	if logged != 1 {
		t.Fatalf("logged = %d after one batch of 3 sixels, want 1", logged)
	}
	p.Filter([]byte(sixel))
	if logged != 1 {
		t.Fatalf("logged = %d after a second batch, want still 1 (once per pane)", logged)
	}
}

// The capability follows the viewer (R4): the same Proxy relays a sixel once
// its RelaySource flips on, and drops it again once flipped back off — no
// re-dial, no new Proxy, because Filter reads the source live on every call.
func TestProxyFollowsRelaySourceFlips(t *testing.T) {
	const sixel = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
	var logged int
	src := NewRelaySource(Relay{})
	p := NewRelay(&fakeLocalizer{}, func(string, ...any) { logged++ }, src, DefaultRasterHold)

	if got := string(p.Filter([]byte(sixel))); got != "" {
		t.Fatalf("phase 1 (off): out = %q, want the sixel dropped", got)
	}
	if logged != 1 {
		t.Fatalf("phase 1 (off): logged = %d, want 1", logged)
	}

	src.Store(NewRelayFromClient(true, "sixel"))
	if got := string(p.Filter([]byte(sixel))); got != sixel {
		t.Fatalf("phase 2 (on): out = %q, want the sixel relayed byte-identically", got)
	}

	src.Store(Relay{})
	if got := string(p.Filter([]byte(sixel))); got != "" {
		t.Fatalf("phase 3 (off again): out = %q, want the sixel dropped", got)
	}
	// loggedRelayOff is a once-per-pane latch (see the field doc), so this
	// second off-phase logs nothing more — harmless, but newly reachable now
	// that the capability can flip back off within one Proxy's life.
	if logged != 1 {
		t.Fatalf("phase 3 (off again): logged = %d, want still 1 (latch already tripped)", logged)
	}

	// The raster hold followed both flips: a >64 KiB partial sixel is held
	// (not overflow-discarded) only while the capability is on, observed
	// through Filter's forwarding rather than the scanner's private field.
	bigBody := strings.Repeat("~", 70<<10)
	bigSixel := "\x1bPq" + bigBody + st

	src.Store(NewRelayFromClient(true, "sixel"))
	half := len(bigSixel) / 2
	if got := string(p.Filter([]byte(bigSixel[:half]))); got != "" {
		t.Fatalf("big sixel first half (hold applied): out = %q, want it held, not forwarded", got)
	}
	if got := string(p.Filter([]byte(bigSixel[half:]))); got != bigSixel {
		t.Fatalf("big sixel second half (hold applied): out = %q, want the whole sixel relayed", got)
	}

	src.Store(Relay{})
	if got := string(p.Filter([]byte(bigSixel[:half]))); got != "" {
		t.Fatalf("big sixel first half (default hold): out = %q, want the overflow path to hold it too", got)
	}
	// Past the 64 KiB non-relay default, the overflow-discard path drops the
	// partial sixel instead of holding it — the rest of the sequence, fed
	// next, is consumed as discard tail rather than forwarded as text or
	// relayed as an image.
	if got := string(p.Filter([]byte(bigSixel[half:]))); got != "" {
		t.Fatalf("big sixel second half (default hold): out = %q, want the overflowed sixel discarded", got)
	}
}

// Relay-only mode (R7) forwards a well-formed kitty APC byte-identically,
// bare and \ePtmux;-wrapped, and leaves t=s/t=t untouched — no Rewrite, no
// EncodeWrapped, no Coalesce, no retain. A dropMalformed APC is the one
// exception, and is covered separately (it never reaches Filter as a chunk at
// all, in any mode).
func TestRelayOnlyForwardsKittyByteIdentically(t *testing.T) {
	p := NewRelay(nil, nil, nil, 0)

	for _, in := range []string{bareSeq, wrappedSeq} {
		if got := string(p.Filter([]byte(in))); got != in {
			t.Fatalf("relay-only mangled a kitty sequence: got %q, want %q", got, in)
		}
	}

	tShare := "\x1b_Gi=9,a=T,t=s;AAAA\x1b\\"
	if got := string(p.Filter([]byte(tShare))); got != tShare {
		t.Fatalf("t=s mangled in relay-only: got %q, want %q (t=s must not be dropped)", got, tShare)
	}

	tTransmit := "\x1b_Gi=9,a=T,t=t,f=100;L3RtcC94LnBuZw==\x1b\\"
	if got := string(p.Filter([]byte(tTransmit))); got != tTransmit {
		t.Fatalf("t=t rewritten in relay-only: got %q, want %q (must not be rewritten to t=f)", got, tTransmit)
	}
}

// R10/R4: a >64 KiB sixel must survive keyneg.Filter.Feed and then
// graphics.Proxy.Filter byte-identically. keyneg.maxRegion is also 64 KiB, so
// walkRegion leaves the DCS region mid-body and rescans the remainder as
// literal — benign, because a sixel body holds no ESC, but the relay path
// depends on it and nothing pinned it before this test.
func TestLargeSixelSurvivesKeynegThenProxyFilterByteIdentically(t *testing.T) {
	body := bytes.Repeat([]byte("~"), 70<<10) // > keyneg.maxRegion and the non-relay 64 KiB bound
	sixel := "\x1bPq" + string(body) + st

	kf := keyneg.NewFilter()
	stripped := append(kf.Feed([]byte(sixel)), kf.Flush()...)

	p := NewRelay(&fakeLocalizer{}, nil, relaySrc("sixel"), DefaultRasterHold)
	if got := string(p.Filter(stripped)); got != sixel {
		t.Fatalf("byte mismatch after keyneg+Filter: got %d bytes, want %d bytes", len(got), len(sixel))
	}
}
