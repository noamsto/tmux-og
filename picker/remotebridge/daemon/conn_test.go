package daemon

import (
	"errors"
	"io"
	"net"
	"testing"
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
