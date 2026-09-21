package proctree

import (
	"slices"
	"strings"
	"testing"
)

func TestAggregateResources(t *testing.T) {
	// 100 is the pane's shell; 200 and 300 are its descendants. 999 belongs to
	// nobody and must not be counted anywhere.
	ps := strings.Join([]string{
		"  PID  PPID %CPU   RSS",
		"  100     1  1.0  1024",
		"  200   100  2.5  2048",
		"  300   200  0.5  1024",
		"  400     1  9.0  8192",
		"  999     1  1.0  1024",
	}, "\n")

	got := Aggregate(map[string][]int{"a": {100}, "b": {400}}, ps)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got["a"].CPUPct != 4.0 {
		t.Errorf("a cpu = %v, want 4.0 (whole tree)", got["a"].CPUPct)
	}
	if got["a"].MemMB != 4.0 {
		t.Errorf("a mem = %v MiB, want 4.0", got["a"].MemMB)
	}
	if got["b"].CPUPct != 9.0 {
		t.Errorf("b cpu = %v, want 9.0", got["b"].CPUPct)
	}
}

func TestAggregateResourcesNoRoots(t *testing.T) {
	if got := Aggregate(nil, "  PID  PPID %CPU   RSS\n 100 1 1.0 1024"); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestAggregateAgentCmdsFromTree(t *testing.T) {
	// 100 is a shell whose child is an agent — tmux-remux's restore shape. 400 is
	// the makeWrapper spelling BSD ps prints as a full path.
	ps := strings.Join([]string{
		"  PID  PPID %CPU   RSS COMMAND",
		"  100     1  0.0   512 fish",
		"  200   100  1.0 65536 claude",
		"  300   200  0.0  1024 node",
		"  400     1  0.0   512 /nix/store/x/.claude-wrapped",
		"  500     1  0.0   512 /usr/bin/python3",
	}, "\n")

	got := Aggregate(map[string][]int{"restored": {100}, "wrapped": {400}, "plain": {500}}, ps)
	for sess, want := range map[string][]string{"restored": {"claude"}, "wrapped": {"claude"}, "plain": nil} {
		if !slices.Equal(got[sess].AgentCmds, want) {
			t.Errorf("%s AgentCmds = %v, want %v", sess, got[sess].AgentCmds, want)
		}
	}
}
