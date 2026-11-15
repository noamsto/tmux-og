// Package enrichstate is the shared PR-state precedence used by every Go
// renderer (statusline + enrichcard). It returns semantic roles, not output
// strings, because each renderer emits a different format (tmux #[fg=…] vs
// lipgloss). Keeping the precedence here prevents the recurring "fix the same
// rule in N renderers" regressions.
package enrichstate

import (
	"strconv"
	"strings"
)

type ColorRole int

const (
	ColorSuccess        ColorRole = iota // green
	ColorPending                         // peach
	ColorFailure                         // red (failing check OR conflicting)
	ColorMerged                          // mauve
	ColorClosed                          // dim overlay (dead/superseded)
	ColorReviewRequired                  // dim overlay (review still owed)
)

type GlyphRole int

const (
	GlyphSuccess GlyphRole = iota
	GlyphPending
	GlyphFailure
	GlyphConflict
	GlyphMerged
	GlyphClosed
)

// Classify maps a PR's state/check/mergeable to its color and glyph roles.
// merged/closed are terminal and win over check state; conflicting wins the
// glyph over a failing check; then failure → red, pending → peach, else green.
func Classify(state, check, mergeable string) (ColorRole, GlyphRole) {
	var color ColorRole
	switch {
	case state == "merged":
		color = ColorMerged
	case state == "closed":
		color = ColorClosed
	case check == "failure" || mergeable == "conflicting":
		color = ColorFailure
	case check == "pending":
		color = ColorPending
	default:
		color = ColorSuccess
	}

	var glyph GlyphRole
	switch {
	case state == "merged":
		glyph = GlyphMerged
	case state == "closed":
		glyph = GlyphClosed
	case mergeable == "conflicting":
		glyph = GlyphConflict
	case check == "failure":
		glyph = GlyphFailure
	case check == "pending":
		glyph = GlyphPending
	default:
		glyph = GlyphSuccess
	}
	return color, glyph
}

// Draft reports whether the badge should carry the draft marker ahead of the
// state glyph. Draft is orthogonal to check state, so it never wins a role from
// Classify; terminal states carry no marker.
func Draft(state, draft string) bool {
	return draft == "1" && state != "merged" && state != "closed"
}

// Icons are the user's enrich.icons, in the raw dialect: each value is the
// literal glyph text, not a tmux format.
type Icons struct{ Pending, Success, Failure, Merged, Closed, Conflict, Draft string }

// Glyph is the single source of the badge glyph half the poller writes as
// @pr_glyph: the state icon (a pie for pending checks with progress), behind
// the draft marker when Draft applies.
func Glyph(ic Icons, state, check, mergeable, draft, progress string) string {
	_, role := Classify(state, check, mergeable)
	var g string
	switch role {
	case GlyphMerged:
		g = ic.Merged
	case GlyphClosed:
		g = ic.Closed
	case GlyphConflict:
		g = ic.Conflict
	case GlyphFailure:
		g = ic.Failure
	case GlyphPending:
		if g = Pie(progress); g == "" {
			g = ic.Pending
		}
	default:
		g = ic.Success
	}
	if Draft(state, draft) {
		return ic.Draft + " " + g
	}
	return g
}

// ReviewColor is the tint for the badge's #<n> half. ok is false when the number
// keeps Classify's color: no review decision, or a PR that is no longer open.
func ReviewColor(state, review string) (ColorRole, bool) {
	if state != "open" {
		return 0, false
	}
	switch review {
	case "approved":
		return ColorSuccess, true
	case "changes_requested":
		return ColorFailure, true
	case "review_required":
		return ColorReviewRequired, true
	}
	return 0, false
}

// AutoMerge reports whether the badge's #<n> half is underlined.
func AutoMerge(state, autoMerge string) bool {
	return state == "open" && autoMerge == "1"
}

// PieSlices are nf-md-circle_slice_1…8, the same frames as the Claude spinner.
var PieSlices = [8]string{"󰪞", "󰪟", "󰪠", "󰪡", "󰪢", "󰪣", "󰪤", "󰪥"}

// Pie returns the slice filled to the share of checks finished, from a
// "<finished>/<total>" progress value, or "" when progress is not usable.
func Pie(progress string) string {
	f, t, ok := strings.Cut(progress, "/")
	if !ok || !asciiDigits(f) || !asciiDigits(t) {
		return ""
	}
	fin, err1 := strconv.Atoi(f)
	tot, err2 := strconv.Atoi(t)
	if err1 != nil || err2 != nil || tot <= 0 || fin < 0 || fin > tot {
		return ""
	}
	return PieSlices[fin*7/tot]
}

// asciiDigits rejects the sign and whitespace forms strconv.Atoi would accept.
func asciiDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
