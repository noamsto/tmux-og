package daemon

import (
	"io"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// A scripted control stream for: the if-shell, a fan-out of branch replies, then
// a layout read. Block bodies are what tmux prints; the barrier's is the tag
// stampAll gave it, which for the first command written is og-fanout-1.
func fanoutStream(branch ...string) string {
	lines := []string{"%begin 1 1 1", "%end 1 1 1"} // the if-shell itself
	for i, b := range branch {
		n := string(rune('2' + i))
		lines = append(lines, "%begin 1 "+n+" 1")
		if b != "" {
			lines = append(lines, b)
		}
		lines = append(lines, "%end 1 "+n+" 1")
	}
	lines = append(lines,
		"%begin 1 8 1", "og-fanout-1", "%end 1 8 1", // the barrier
		"%begin 1 9 1", "@0-layout %2 0", "%end 1 9 1", // the layout read
	)
	return strings.Join(lines, "\n") + "\n"
}

// #715: the tool verb's if-shell gate runs its branch as further commands of the
// same control client, each with a client-flagged reply block of its own. The
// next round-trip must still get its own reply, however many blocks the branch
// produced.
func TestIfShellBranchRepliesDoNotDesyncRoundTrips(t *testing.T) {
	for name, branch := range map[string][]string{
		"branch of two commands": {"", ""},
		"failing first command":  {""}, // an error aborts the rest of its list
		"no branch replies":      {},
		"long branch":            {"", "", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := newStream(io.Discard)
			rt := newRoundTrip(controlmode.NewReader(strings.NewReader(fanoutStream(branch...))),
				NewRouter(), &asyncQueue{}, st)

			if !st.send("if-shell -F 1 'set -p @x 1 ; set -p @y 2' ''") {
				t.Fatal("send failed")
			}
			l, ok := one(rt, "display-message -p layout")
			if !ok {
				t.Fatal("round-trip after the if-shell got no reply")
			}
			if got := string(l.Data); got != "@0-layout %2 0" {
				t.Fatalf("round-trip read %q, want its own reply: the branch's blocks were counted as replies", got)
			}
		})
	}
}

func TestExpandsReplies(t *testing.T) {
	for cmd, want := range map[string]bool{
		"if-shell -t %1 -F 1 'a' 'b'": true,
		"if -F 1 'a'":                 true,
		"display-message -p x":        false,
		"run-shell -b 'if-shell'":     false,
	} {
		if got := expandsReplies(cmd); got != want {
			t.Errorf("expandsReplies(%q) = %v, want %v", cmd, got, want)
		}
	}
}
