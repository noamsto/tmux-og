package daemon

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// optsRig records every local tmux call and answers list-windows with out.
type optsRig struct {
	cfg   Config
	calls [][]string
	reads [][]string
	out   string
	err   error
}

func newOptsRig(placed bool) *optsRig {
	r := &optsRig{}
	r.cfg = Config{
		SockPath:  "/s/mine.sock",
		LocalSess: "host-sess",
		LocalTmux: func(args ...string) error {
			r.calls = append(r.calls, append([]string(nil), args...))
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			r.reads = append(r.reads, append([]string(nil), args...))
			return r.out, r.err
		},
		opts: newMirrorOpts(),
	}
	if placed {
		r.cfg.Placed = true
		r.cfg.LocalTarget = "$4"
	}
	return r
}

// Windows @2 and @5 are ours, @3 is a local window and @6 another daemon's.
const optsHostListing = "@2||/s/mine.sock\n@3||\n@5|1|/s/mine.sock\n@6|1|/s/other.sock\n"

func TestMirrorOptsDedicatedKeepsTheSessionWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *mirrorOpts
	}{{"live", newMirrorOpts()}, {"nil receiver", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOptsRig(false)
			tc.opts.set(r.cfg, "@bridge_state", "x")
			tc.opts.unset(r.cfg, "@bridge_state")
			tc.opts.applyTo(r.cfg, "@7")
			want := [][]string{
				{"set-option", "-t", "host-sess", "@bridge_state", "x"},
				{"set-option", "-u", "-t", "host-sess", "@bridge_state"},
			}
			if !reflect.DeepEqual(r.calls, want) {
				t.Errorf("calls = %v, want %v", r.calls, want)
			}
			if len(r.reads) != 0 {
				t.Errorf("a dedicated write listed windows: %v", r.reads)
			}
		})
	}
}

func TestMirrorOptsNilReceiverWritesTheSessionWhenPlaced(t *testing.T) {
	r := newOptsRig(true)
	var o *mirrorOpts
	o.set(r.cfg, "@bridge_state", "x")
	// A nil receiver is a bare test Config: a session write, by name.
	want := [][]string{{"set-option", "-t", "host-sess", "@bridge_state", "x"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
}

func TestMirrorOptsPlacedSetWritesEveryOwnWindowOnly(t *testing.T) {
	r := newOptsRig(true)
	r.out = optsHostListing
	r.cfg.opts.set(r.cfg, "@bridge_state", "x")

	wantRead := []string{"list-windows", "-t", "$4", "-F", hostWindowsFormat}
	if len(r.reads) != 1 || !reflect.DeepEqual(r.reads[0], wantRead) {
		t.Fatalf("reads = %v, want one %v", r.reads, wantRead)
	}
	want := [][]string{{
		"set-option", "-w", "-t", "@2", "@bridge_state", "x", ";",
		"set-option", "-w", "-t", "@5", "@bridge_state", "x",
	}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
}

func TestMirrorOptsPlacedUnsetUsesTheUnsetForm(t *testing.T) {
	r := newOptsRig(true)
	r.out = "@2||/s/mine.sock\n"
	r.cfg.opts.set(r.cfg, "@bridge_state", "x")
	r.calls = nil
	r.cfg.opts.unset(r.cfg, "@bridge_state")
	want := [][]string{{"set-option", "-w", "-u", "-t", "@2", "@bridge_state"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
	// The unset also forgets the value: a later window gets nothing to apply.
	r.calls = nil
	r.cfg.opts.applyTo(r.cfg, "@7")
	if len(r.calls) != 0 {
		t.Errorf("applyTo after unset wrote %v", r.calls)
	}
}

func TestMirrorOptsPlacedWritesNothingWithoutOwnWindows(t *testing.T) {
	for name, tc := range map[string]struct {
		out string
		err error
	}{
		"none own":     {out: "@3||\n@6|1|/s/other.sock\n"},
		"read failure": {err: errors.New("list-windows failed")},
	} {
		t.Run(name, func(t *testing.T) {
			r := newOptsRig(true)
			r.out, r.err = tc.out, tc.err
			r.cfg.opts.set(r.cfg, "@bridge_state", "x")
			if len(r.calls) != 0 {
				t.Errorf("wrote %v", r.calls)
			}
		})
	}
}

func TestMirrorOptsApplyToStampsEveryRecordedValue(t *testing.T) {
	r := newOptsRig(true)
	r.out = "" // no window yet: the values are only recorded
	r.cfg.opts.set(r.cfg, "@bridge_usage", "u")
	r.cfg.opts.set(r.cfg, "@bridge_state", "disconnected")
	r.cfg.opts.set(r.cfg, "@bridge_state", "gone")
	r.cfg.opts.applyTo(r.cfg, "@7")
	want := [][]string{{
		"set-option", "-w", "-t", "@7", "@bridge_state", "gone", ";",
		"set-option", "-w", "-t", "@7", "@bridge_usage", "u",
	}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
}

func TestMirrorOptsApplyToIsANoOpWhenDedicated(t *testing.T) {
	r := newOptsRig(false)
	r.cfg.opts.set(r.cfg, "@bridge_state", "x")
	r.calls = nil
	r.cfg.opts.applyTo(r.cfg, "@7")
	if len(r.calls) != 0 {
		t.Errorf("dedicated applyTo wrote %v", r.calls)
	}
}

func TestNudgePathFor(t *testing.T) {
	dedicated := Config{SockPath: "/run/og/a.sock"}
	if got, want := nudgePathFor(dedicated), "/run/og/a.sock"+resizeNudgeSuffix; got != want {
		t.Errorf("dedicated = %q, want %q", got, want)
	}
	placed := Config{
		SockPath:    "/run/og/og-into-1.sock",
		Placed:      true,
		LocalTarget: "$4",
		local:       localPin{serverPID: "123", id: "$4"},
	}
	if got, want := nudgePathFor(placed), "/run/og/og-nudge-123-4.resize"; got != want {
		t.Errorf("placed = %q, want %q", got, want)
	}
	// Two daemons of one host session agree on the file; another session does not.
	other := placed
	other.SockPath = "/run/og/og-into-2.sock"
	if nudgePathFor(other) != nudgePathFor(placed) {
		t.Error("two daemons in one host session must share the nudge file")
	}
	other.LocalTarget = "$5"
	if nudgePathFor(other) == nudgePathFor(placed) {
		t.Error("another host session must not share the nudge file")
	}
	if !strings.HasSuffix(nudgePathFor(placed), ".resize") {
		t.Error("nudge file must end in .resize")
	}
}
