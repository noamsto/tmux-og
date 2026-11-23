package probekit

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// testdata/strip-escapes.tsv is shared with remoteopen's safeText. Columns:
// name, input, SanitizeStatusText output, safeText output. Fields are Go
// string literal bodies, so \x1b and \u009b name bytes the file can't show.
func TestStripEscapesFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/strip-escapes.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("row %q: want 4 fields, got %d", line, len(fields))
		}
		in := unquoteFixtureField(t, fields[1])
		want := unquoteFixtureField(t, fields[2])
		t.Run(fields[0], func(t *testing.T) {
			if got := SanitizeStatusText(in); got != want {
				t.Errorf("SanitizeStatusText(%q) = %q, want %q", in, got, want)
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
