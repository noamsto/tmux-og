package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestDialConnNilDialUsesCtl pins the mechanism "Dial == nil stays
// single-shot" rests on: with no Dial, dialConn's only source of a connection
// is the already-opened Ctl, so a second one cannot be obtained at all.
func TestDialConnNilDialUsesCtl(t *testing.T) {
	local, peer := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = peer.Close() }()

	c, err := dialConn(Config{Ctl: local})
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	defer c.close()
	if c.rwc != io.ReadWriteCloser(local) {
		t.Error("ctlConn did not wrap the supplied Ctl")
	}
}

// TestDialConnNilDialNilCtlErrors: a Config with neither seam is a caller
// error, not a silent no-op.
func TestDialConnNilDialNilCtlErrors(t *testing.T) {
	if _, err := dialConn(Config{}); err == nil {
		t.Error("dialConn with no Ctl and no Dial should error")
	}
}

// TestDialConnPrefersDialOverCtl: Dial, when set, is the only source of
// connections — Ctl stays wired for the M1 / scripted-test callers that hold
// one connection and cannot make another, but Run never falls back to it once
// Dial is set (see the Config.Dial doc comment).
func TestDialConnPrefersDialOverCtl(t *testing.T) {
	local, peer := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = peer.Close() }()
	dialed, dialedPeer := net.Pipe()
	defer func() { _ = dialed.Close() }()
	defer func() { _ = dialedPeer.Close() }()

	cfg := Config{
		Ctl:  local,
		Dial: func() (io.ReadWriteCloser, error) { return dialed, nil },
	}
	c, err := dialConn(cfg)
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	defer c.close()
	if c.rwc != io.ReadWriteCloser(dialed) {
		t.Error("dialConn used Ctl instead of Dial's connection")
	}
}

// TestDialConnPropagatesDialError: a failed re-dial must surface as an error
// to the caller (reattach's retry loop), not panic or silently wrap nil.
func TestDialConnPropagatesDialError(t *testing.T) {
	boom := errors.New("boom")
	cfg := Config{Dial: func() (io.ReadWriteCloser, error) { return nil, boom }}
	if _, err := dialConn(cfg); !errors.Is(err, boom) {
		t.Errorf("dialConn err = %v, want %v", err, boom)
	}
}

// TestConnHolderEmptySlotFailsClosed pins the "no live connection" contract
// every long-lived capturer (pumpInput, watchLocalClient, the ctl accept loop)
// relies on across a re-dial: send must report false rather than block, and
// roundTrip must yield an immediately-exhausted batch rather than hang.
func TestConnHolderEmptySlotFailsClosed(t *testing.T) {
	h := &connHolder{}
	if h.send("anything") {
		t.Error("send on an empty holder should fail closed")
	}
	next := h.roundTrip("anything")
	if _, ok := next(); ok {
		t.Error("roundTrip on an empty holder should yield an exhausted batch")
	}
}

// TestConnHolderCloseEmptiesTheSlotIdempotently: both the drop path and
// teardown call close(), including after an exhausted retry budget when the
// slot is already empty — it must not panic either way.
func TestConnHolderCloseEmptiesTheSlotIdempotently(t *testing.T) {
	h := &connHolder{}
	h.close() // empty slot: must be a no-op, not a nil-pointer panic

	local, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	h.set(newCtlConn(local))
	h.close()
	if h.get() != nil {
		t.Error("close should empty the slot")
	}
	h.close() // already empty: still must not panic
}

// pipeConn is a ctlConn over a net.Pipe whose far end answers like tmux: each
// og-fanout barrier with its tag, every other command with the next of replies
// (nothing once they run out). The returned writer feeds the control stream.
func pipeConn(t *testing.T, replies ...string) (*ctlConn, func(string)) {
	t.Helper()
	near, far := net.Pipe()
	t.Cleanup(func() { _ = near.Close(); _ = far.Close() })
	write := func(s string) { _, _ = far.Write([]byte(s)) }
	go func() {
		sc := bufio.NewScanner(far)
		for n := 1; sc.Scan(); n++ {
			if tag, ok := strings.CutPrefix(sc.Text(), "display-message -p og-fanout-"); ok {
				write(fmt.Sprintf("%%begin 9 %d 1\nog-fanout-%s\n%%end 9 %d 1\n", n, tag, n))
				continue
			}
			if len(replies) > 0 {
				write(replies[0])
				replies = replies[1:]
			}
		}
	}()
	return newCtlConn(near), write
}

// laterBlock writes a block whose only row is a notification after the version
// switch has been applied, and returns what the pump delivers for it, past any
// barrier reply still in flight.
func laterBlock(t *testing.T, c *ctlConn, write func(string)) controlmode.Line {
	t.Helper()
	go write("%begin 2 2 1\n%window-add @7\n%end 2 2 1\n")
	for {
		l, ok := c.pump.Next()
		if !ok {
			t.Fatal("pump closed before the later block")
		}
		if l.Kind != controlmode.End || !strings.HasPrefix(string(l.Data), "og-fanout-") {
			return l
		}
	}
}

func TestAdoptVersionChoosesLifting(t *testing.T) {
	tests := []struct {
		version  string
		wantKind controlmode.Kind
	}{
		{"next-3.9", controlmode.End},
		{"next-3.8", controlmode.WindowAdd},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			c, write := pipeConn(t)
			c.adoptVersion(tt.version)
			l := laterBlock(t, c, write)
			if l.Kind != tt.wantKind {
				t.Fatalf("kind = %v, want %v", l.Kind, tt.wantKind)
			}
			if tt.wantKind == controlmode.End && string(l.Data) != "%window-add @7" {
				t.Errorf("body = %q, want the row kept as reply body", l.Data)
			}
		})
	}
}

func TestIdentifyAdoptsVersionFromTheReply(t *testing.T) {
	tests := []struct {
		version string
		lifted  bool
	}{
		{"next-3.8", true},
		{"3.7c", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			c, write := pipeConn(t, newLayoutsFlagAck, "%begin 1 1 1\n2151|1788283304|$1|"+tt.version+"\n%end 1 1 1\n")
			id, err := c.identify("A")
			if err != nil {
				t.Fatalf("identify: %v", err)
			}
			if id.version != tt.version {
				t.Errorf("version = %q, want %q", id.version, tt.version)
			}
			l := laterBlock(t, c, write)
			if tt.lifted {
				if l.Kind != controlmode.WindowAdd {
					t.Fatalf("line = %+v, want the row lifted for %s", l, tt.version)
				}
				return
			}
			if l.Kind != controlmode.End || string(l.Data) != "%window-add @7" {
				t.Fatalf("line = %+v, want one End holding the row for %s", l, tt.version)
			}
		})
	}
}

func TestIdentifyFailureLeavesLiftingOff(t *testing.T) {
	c, write := pipeConn(t, newLayoutsFlagAck, "%begin 1 1 1\nboom\n%error 1 1 1\n")
	if _, err := c.identify("A"); err == nil {
		t.Fatal("identify err = nil, want the error reply to fail it")
	}
	l := laterBlock(t, c, write)
	if l.Kind != controlmode.End || string(l.Data) != "%window-add @7" {
		t.Fatalf("line = %+v, want End holding the row: a failed read must leave lifting off", l)
	}
}

func TestFreshConnKeepsInBlockNotificationAsBody(t *testing.T) {
	c, write := pipeConn(t)
	l := laterBlock(t, c, write)
	if l.Kind != controlmode.End || string(l.Data) != "%window-add @7" {
		t.Fatalf("line = %+v, want End holding the row: lifting is off until adoptVersion", l)
	}
}
