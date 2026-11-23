package remoteopen

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// Shares testdata/strip-escapes.tsv with probekit's SanitizeStatusText; this
// asserts the safeText column. See the probekit copy for the column layout.
func TestStripEscapesFixture(t *testing.T) {
	data, err := os.ReadFile("../probekit/testdata/strip-escapes.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("row %q: want 4 fields, got %d", line, len(fields))
		}
		in := unquoteFixtureField(t, fields[1])
		want := unquoteFixtureField(t, fields[3])
		t.Run(fields[0], func(t *testing.T) {
			if got := safeText(in); got != want {
				t.Errorf("safeText(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func unquoteFixtureField(t *testing.T, s string) string {
	t.Helper()
	v, err := strconv.Unquote(`"` + s + `"`)
	if err != nil {
		t.Fatalf("field %q: %v", s, err)
	}
	return v
}
