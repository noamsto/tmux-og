package enrichstate

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name                    string
		state, check, mergeable string
		wantColor               ColorRole
		wantGlyph               GlyphRole
	}{
		{"merged wins over pending check", "merged", "pending", "", ColorMerged, GlyphMerged},
		{"closed is dim, distinct from merged", "closed", "success", "", ColorClosed, GlyphClosed},
		{"conflicting wins glyph over failure", "open", "failure", "conflicting", ColorFailure, GlyphConflict},
		{"failure check is red", "open", "failure", "", ColorFailure, GlyphFailure},
		{"pending check is peach", "open", "pending", "", ColorPending, GlyphPending},
		{"clean open PR is green success", "open", "success", "mergeable", ColorSuccess, GlyphSuccess},
		{"empty everything defaults to success", "", "", "", ColorSuccess, GlyphSuccess},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotColor, gotGlyph := Classify(c.state, c.check, c.mergeable)
			if gotColor != c.wantColor || gotGlyph != c.wantGlyph {
				t.Errorf("Classify(%q,%q,%q) = (%v,%v), want (%v,%v)",
					c.state, c.check, c.mergeable, gotColor, gotGlyph, c.wantColor, c.wantGlyph)
			}
		})
	}
}

func TestDraft(t *testing.T) {
	cases := []struct {
		name         string
		state, draft string
		want         bool
	}{
		{"open draft is marked", "open", "1", true},
		{"open non-draft is not", "open", "", false},
		{"merged draft drops the marker", "merged", "1", false},
		{"closed draft drops the marker", "closed", "1", false},
		{"unset draft option is not", "open", "0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Draft(c.state, c.draft); got != c.want {
				t.Errorf("Draft(%q,%q) = %v, want %v", c.state, c.draft, got, c.want)
			}
		})
	}
}

func TestPieRejectsSignedHalves(t *testing.T) {
	for _, p := range []string{"+3/5", "3/+5"} {
		if got := Pie(p); got != "" {
			t.Errorf("Pie(%q) = %q, want empty", p, got)
		}
	}
}
