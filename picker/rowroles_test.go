package main

import (
	"testing"

	"github.com/noamsto/tmux-og/picker/palette"
)

// TestRowRolesCoverRowBuilders pins that every role the session/window row
// builders, newResourceColors and hostColorFunc resolve is contrast-checked
// in palette.LightPairs via RowRoles.
func TestRowRolesCoverRowBuilders(t *testing.T) {
	used := []palette.Role{
		palette.Primary, palette.Quiet, palette.Mauve, palette.Success,
		palette.Warn, palette.Highlight, palette.Blue, palette.Muted,
		palette.LevelMed, palette.Teal,
	}
	used = append(used, hostLightRoles[:]...)
	for _, u := range used {
		found := false
		for _, rr := range palette.RowRoles {
			if rr.Role == u {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %+v used by row builders is not in palette.RowRoles", u)
		}
	}
}
