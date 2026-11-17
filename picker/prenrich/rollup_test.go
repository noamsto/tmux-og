package main

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCollapseRollupFixtures(t *testing.T) {
	cases := []struct{ file, want string }{
		{"rollup-success.json", "success"},
		{"rollup-failure.json", "failure"},
		{"rollup-pending.json", "pending"},
		{"rollup-empty.json", "none"},
		{"rollup-mixed.json", "pending"},
		{"rollup-statuscontext-failure.json", "failure"},
		{"rollup-statuscontext-success.json", "success"},
		{"rollup-cancelled.json", "failure"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			state, _ := collapseRollup(readFixture(t, c.file))
			if state != c.want {
				t.Fatalf("state = %q, want %q", state, c.want)
			}
		})
	}
}

func TestCollapseRollupPendingProgress(t *testing.T) {
	state, progress := collapseRollup(`[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""},{"__typename":"StatusContext","state":"PENDING"}]`)
	if state != "pending" || progress != "1/3" {
		t.Fatalf("got %q, %q; want pending, 1/3", state, progress)
	}
}

func TestCollapseRollupSettledNoProgress(t *testing.T) {
	for _, f := range []string{"rollup-success.json", "rollup-failure.json"} {
		if _, progress := collapseRollup(readFixture(t, f)); progress != "" {
			t.Errorf("%s: progress = %q, want empty", f, progress)
		}
	}
}

func TestCollapseRollupMalformed(t *testing.T) {
	state, progress := collapseRollup("not json")
	if state != "none" || progress != "" {
		t.Fatalf("got %q, %q; want none, empty", state, progress)
	}
}

func TestCollapseRollupSkipped(t *testing.T) {
	skipped := `{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"}`
	success := `{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}`
	failure := `{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}`
	running := `{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}`
	cases := []struct{ name, raw, wantState, wantProgress string }{
		{"all skipped", "[" + skipped + "]", "pending", ""},
		{"several skipped", "[" + skipped + "," + skipped + "]", "pending", ""},
		{"skipped and success", "[" + skipped + "," + success + "]", "success", ""},
		{"skipped and failure", "[" + skipped + "," + failure + "]", "failure", ""},
		{"skipped and running", "[" + skipped + "," + running + "]", "pending", "1/2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, progress := collapseRollup(c.raw)
			if state != c.wantState || progress != c.wantProgress {
				t.Fatalf("got %q, %q; want %q, %q", state, progress, c.wantState, c.wantProgress)
			}
		})
	}
}
