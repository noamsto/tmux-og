package probekit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeStatusText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"csi", "\x1b[31mred\x1b[0m", "red"},
		{"osc bel", "a\x1b]0;t\x07b", "ab"},
		{"osc st", "a\x1b]8;;u\x1b\\b", "ab"},
		{"two-byte esc", "a\x1bcb", "ab"},
		{"c1 csi", "a\u009b31mb", "a31mb"},
		{"bidi", "a\u202eb\u2066c", "abc"},
		{"bidi rlm", "a\u200fb", "ab"},
		{"bidi alm", "a\u061cb", "ab"},
		{"bidi lrm", "a\u200eb", "ab"},
		{"whitespace collapse", "a\n\t b", "a b"},
		{"trim", "  a  ", "a"},
		{"cap", strings.Repeat("x", 300), strings.Repeat("x", 200)},
		{"cap counts collapsed spaces", strings.Repeat("ab \n", 100), strings.Repeat("ab ", 66) + "ab"},
		{"plain unicode", "héllo ✓", "héllo ✓"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeStatusText(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeStatusText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > 200 {
				t.Errorf("len = %d runes, want <= 200", n)
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	raw := "a\x1b[31mb\x07c\u202ed" + strings.Repeat("x", 100)
	got := DisplayName(raw)
	if want := TruncateCells(SanitizeStatusText(raw), 40); got != want {
		t.Fatalf("DisplayName = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Fatalf("DisplayName left a control in %q", got)
	}
}
