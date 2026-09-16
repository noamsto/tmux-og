package daemon

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

func TestResolveViewIdentity(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		wantOK   bool
		wantTerm string
		wantSix  bool
	}{
		{
			name:   "empty input",
			lines:  nil,
			wantOK: false,
		},
		{
			name:   "blank line only",
			lines:  []string{""},
			wantOK: false,
		},
		{
			name:     "one kitty client",
			lines:    []string{"0|xterm-kitty|sixel|1"},
			wantOK:   true,
			wantTerm: "xterm-kitty",
			wantSix:  true,
		},
		{
			name:     "one foot client",
			lines:    []string{"0|foot||0"},
			wantOK:   true,
			wantTerm: "foot",
			wantSix:  false,
		},
		{
			name: "mixed kitty and foot",
			lines: []string{
				"0|xterm-kitty||0", // kitty-capable, no sixel
				"0|foot|sixel|1",   // not kitty-capable, sixel
			},
			wantOK:   true,
			wantTerm: "foot", // the non-kitty-capable witness
			wantSix:  false,  // xterm-kitty client fails the AND
		},
		{
			name: "two kitty clients, different termnames",
			lines: []string{
				"0|xterm-kitty|sixel|1",
				"0|xterm-ghostty|sixel|1",
			},
			wantOK:   true,
			wantTerm: "xterm-ghostty", // lexicographically smaller of the two
			wantSix:  true,
		},
		{
			name:   "control-mode client alone",
			lines:  []string{"1|xterm-kitty||"},
			wantOK: false,
		},
		{
			name: "control client mixed with a real one",
			lines: []string{
				"1|xterm-kitty||",
				"0|foot|sixel|1",
			},
			wantOK:   true,
			wantTerm: "foot",
			wantSix:  true,
		},
		{
			name: "malformed and short lines skipped",
			lines: []string{
				"0|xterm-kitty",         // short: 2 fields
				"garbage",               // short: 1 field
				"0|foot|sixel|1|extra",  // too many fields
				"0|xterm-kitty|sixel|1", // the one valid row
			},
			wantOK:   true,
			wantTerm: "xterm-kitty",
			wantSix:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveViewIdentity(tt.lines)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Term != tt.wantTerm {
				t.Errorf("Term = %q, want %q", got.Term, tt.wantTerm)
			}
			if got.Relay.Sixel() != tt.wantSix {
				t.Errorf("Relay.Sixel() = %v, want %v", got.Relay.Sixel(), tt.wantSix)
			}
		})
	}
}

// TestResolveViewIdentityDeterministic asserts the result is a pure function
// of the client SET, not of list-clients' reporting order (R2).
func TestResolveViewIdentityDeterministic(t *testing.T) {
	forward := []string{
		"0|xterm-kitty|sixel|1",
		"0|xterm-ghostty|sixel|1",
		"0|foot||0",
	}
	reversed := []string{
		"0|foot||0",
		"0|xterm-ghostty|sixel|1",
		"0|xterm-kitty|sixel|1",
	}

	got1, ok1 := resolveViewIdentity(forward)
	got2, ok2 := resolveViewIdentity(reversed)
	if !ok1 || !ok2 {
		t.Fatalf("expected both resolves to succeed: ok1=%v ok2=%v", ok1, ok2)
	}
	if got1 != got2 {
		t.Fatalf("resolve is order-dependent: forward=%+v reversed=%+v", got1, got2)
	}
}

// TestViewClientFormatDelimiter asserts R12/CLAUDE.md's '|'-delimited rule by
// actually parsing a row built to the format's own shape, rather than relying
// on go test ./tmuxformat/..., which only rejects control bytes on lines
// containing "#{" and would pass a space-delimited format silently.
func TestViewClientFormatDelimiter(t *testing.T) {
	if strings.ContainsAny(viewClientFormat, "\t\n") {
		t.Fatalf("viewClientFormat contains a raw tab or newline: %q", viewClientFormat)
	}
	if !strings.Contains(viewClientFormat, "|") {
		t.Fatalf("viewClientFormat is not '|'-delimited: %q", viewClientFormat)
	}

	// A row tmux would actually emit for this format.
	row := "0|xterm-kitty|bpaste,focus,sixel,title|1"
	fields := strings.Split(row, "|")
	if len(fields) != 4 {
		t.Fatalf("got %d fields parsing %q, want 4 — the '|' delimiter did not survive", len(fields), row)
	}
	if fields[0] != "0" || fields[1] != "xterm-kitty" || fields[2] != "bpaste,focus,sixel,title" || fields[3] != "1" {
		t.Fatalf("fields = %#v, want [0 xterm-kitty bpaste,focus,sixel,title 1]", fields)
	}
}

func TestViewingTermCellsRoundTripAndIndependent(t *testing.T) {
	v := &Viewing{}

	if got := v.Desired(); got != "" {
		t.Fatalf("zero-value Desired() = %q, want empty", got)
	}
	if got := v.Advertised(); got != "" {
		t.Fatalf("zero-value Advertised() = %q, want empty", got)
	}

	v.SetDesired("xterm-kitty")
	if got := v.Desired(); got != "xterm-kitty" {
		t.Fatalf("Desired() = %q, want xterm-kitty", got)
	}
	if got := v.Advertised(); got != "" {
		t.Fatalf("SetDesired moved Advertised(): got %q, want empty", got)
	}

	v.setAdvertised("foot")
	if got := v.Advertised(); got != "foot" {
		t.Fatalf("Advertised() = %q, want foot", got)
	}
	if got := v.Desired(); got != "xterm-kitty" {
		t.Fatalf("setAdvertised moved Desired(): got %q, want xterm-kitty", got)
	}
}

func TestViewingRelayIsThePointerNotACopy(t *testing.T) {
	src := graphics.NewRelaySource(graphics.NewRelayFromClient(true, "sixel"))
	v := &Viewing{Relay: src}

	src.Store(graphics.NewRelayFromClient(false, ""))
	if v.Relay.Load().Sixel() {
		t.Fatal("Viewing.Relay must be the same cell as the source it was given, not a copy")
	}
}

func TestViewingConcurrentAccess(t *testing.T) {
	v := &Viewing{Relay: graphics.NewRelaySource(graphics.Relay{})}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			term := "term"
			if i%2 == 0 {
				term = "other"
			}
			v.SetDesired(term)
			v.setAdvertised(term)
			v.Relay.Store(graphics.NewRelayFromClient(true, "sixel"))
			_ = v.Desired()
			_ = v.Advertised()
			_ = v.Relay.Load()
		}(i)
	}
	wg.Wait()
}

func TestViewingSeedSetsBothCells(t *testing.T) {
	v := &Viewing{}
	v.Seed("xterm-kitty")
	if got := v.Desired(); got != "xterm-kitty" {
		t.Errorf("Desired() = %q, want xterm-kitty", got)
	}
	if got := v.Advertised(); got != "xterm-kitty" {
		t.Errorf("Advertised() = %q, want xterm-kitty — Seed is the one place both move together", got)
	}
}

func TestResolveLocalViewIdentity(t *testing.T) {
	t.Run("nil out is unresolved", func(t *testing.T) {
		if _, ok := ResolveLocalViewIdentity(nil, "sess"); ok {
			t.Fatal("a nil out must resolve to false")
		}
	})

	t.Run("empty session is unresolved", func(t *testing.T) {
		out := func(args ...string) (string, error) { return "0|xterm-kitty|sixel|1\n", nil }
		if _, ok := ResolveLocalViewIdentity(out, ""); ok {
			t.Fatal("an empty session must resolve to false")
		}
	})

	t.Run("exec error is unresolved", func(t *testing.T) {
		out := func(args ...string) (string, error) { return "", errors.New("boom") }
		if _, ok := ResolveLocalViewIdentity(out, "sess"); ok {
			t.Fatal("an exec error must resolve to false, same as an empty client set")
		}
	})

	t.Run("empty output is unresolved", func(t *testing.T) {
		out := func(args ...string) (string, error) { return "", nil }
		if _, ok := ResolveLocalViewIdentity(out, "sess"); ok {
			t.Fatal("empty output must resolve to false")
		}
	})

	t.Run("builds the list-clients argv and parses its rows", func(t *testing.T) {
		var gotArgs []string
		out := func(args ...string) (string, error) {
			gotArgs = args
			return "0|xterm-kitty|bpaste,sixel|1\n", nil
		}
		id, ok := ResolveLocalViewIdentity(out, "host-sess")
		if !ok {
			t.Fatal("want a resolved identity")
		}
		if id.Term != "xterm-kitty" || !id.Relay.Sixel() {
			t.Fatalf("id = %+v, want term xterm-kitty with sixel capability", id)
		}
		want := []string{"list-clients", "-t", "host-sess", "-F", viewClientFormat}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Fatalf("argv = %v, want %v", gotArgs, want)
		}
	})
}
