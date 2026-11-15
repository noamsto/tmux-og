package agentstate

import (
	"flag"
	"os"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/claude_priority_state.golden")

const golden = "testdata/claude_priority_state.golden"

func TestShellGolden(t *testing.T) {
	if *update {
		if err := os.WriteFile(golden, []byte(Shell()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := Shell(); got != string(want) {
		t.Errorf("Shell() drifted from %s; run go test ./agentstate -update\n%s", golden, got)
	}
}

func TestPickFollowsOrder(t *testing.T) {
	for i, want := range Order {
		got := Pick(func(s string) bool { return slices.Index(Order, s) >= i })
		if got != want {
			t.Errorf("Pick from index %d = %q, want %q", i, got, want)
		}
	}
	if got := Pick(func(string) bool { return false }); got != "" {
		t.Errorf("Pick(none) = %q, want empty", got)
	}
}

func TestShellArgsCoverOrder(t *testing.T) {
	if len(shellArgs) != len(Order) {
		t.Fatalf("shellArgs has %d entries, Order %d", len(shellArgs), len(Order))
	}
}
