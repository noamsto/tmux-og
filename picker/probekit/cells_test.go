package probekit

import "testing"

func TestTruncateCells(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"under", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"over", "hello world", 8, "hello w…"},
		{"tiny", "abcdef", 2, "a…"},
	}
	for _, c := range cases {
		if got := TruncateCells(c.s, c.max); got != c.want {
			t.Errorf("%s: TruncateCells(%q,%d) = %q, want %q", c.name, c.s, c.max, got, c.want)
		}
		if w := IconCellWidth(TruncateCells(c.s, c.max)); w > c.max {
			t.Errorf("%s: result width %d exceeds max %d", c.name, w, c.max)
		}
	}
}
