package enrichstate

import "testing"

func TestGlyph(t *testing.T) {
	ic := Icons{Pending: "P", Success: "S", Failure: "F", Merged: "M", Closed: "X", Conflict: "C", Draft: "D"}
	tests := []struct {
		name                                           string
		state, check, mergeable, draft, progress, want string
	}{
		{"failing check, uppercase state", "OPEN", "failure", "", "", "", "F"},
		{"open success", "open", "success", "", "", "", "S"},
		{"merged", "merged", "success", "", "", "", "M"},
		{"merged beats pending rollup", "merged", "pending", "", "", "", "M"},
		{"merged beats failing rollup", "merged", "failure", "", "", "", "M"},
		{"closed", "closed", "success", "", "", "", "X"},
		{"closed beats failing rollup", "closed", "failure", "", "", "", "X"},
		{"conflicting", "open", "success", "conflicting", "", "", "C"},
		{"conflict beats failing checks", "open", "failure", "conflicting", "", "", "C"},
		{"mergeable keeps check glyph", "open", "pending", "mergeable", "", "", "P"},
		{"draft success", "open", "success", "mergeable", "1", "", "D S"},
		{"draft pending", "open", "pending", "mergeable", "1", "", "D P"},
		{"draft beside conflict", "open", "failure", "conflicting", "1", "", "D C"},
		{"merged carries no draft marker", "merged", "success", "mergeable", "1", "", "M"},
		{"closed carries no draft marker", "closed", "success", "mergeable", "1", "", "X"},
		{"non-draft unchanged", "open", "success", "mergeable", "", "", "S"},
		{"pending with progress is a pie", "open", "pending", "mergeable", "", "3/8", PieSlices[2]},
		{"pending without progress", "open", "pending", "mergeable", "", "", "P"},
		{"draft marker ahead of pie", "open", "pending", "mergeable", "1", "5/8", "D " + PieSlices[4]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Glyph(ic, tc.state, tc.check, tc.mergeable, tc.draft, tc.progress); got != tc.want {
				t.Errorf("Glyph = %q, want %q", got, tc.want)
			}
		})
	}
}
