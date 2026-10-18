package daemon

import "testing"

func TestRelayEnvCmd(t *testing.T) {
	want := "set-environment -t 'mysession' OG_RELAY_GRAPHICS 'sixel'"
	if got := RelayEnvCmd("mysession", "sixel"); got != want {
		t.Errorf("RelayEnvCmd = %q, want %q", got, want)
	}
}

// An empty value is the correct overwrite for "no relayable capability" under
// the grammar (RelayEnvVar's doc comment) — it must still produce a
// well-formed set-environment with an empty quoted argument, not a malformed
// line and not a skipped write.
func TestRelayEnvCmdEmptyValue(t *testing.T) {
	want := "set-environment -t 'mysession' OG_RELAY_GRAPHICS ''"
	if got := RelayEnvCmd("mysession", ""); got != want {
		t.Errorf("RelayEnvCmd with empty value = %q, want %q", got, want)
	}
}

func TestRelayEnvCmdQuotesSessionName(t *testing.T) {
	want := `set-environment -t 'my'\''session' OG_RELAY_GRAPHICS 'sixel'`
	if got := RelayEnvCmd("my'session", "sixel"); got != want {
		t.Errorf("RelayEnvCmd with quote in session name = %q, want %q", got, want)
	}
}

func TestRelayEnvUnsetCmd(t *testing.T) {
	want := "set-environment -u -t 'mysession' OG_RELAY_GRAPHICS"
	if got := RelayEnvUnsetCmd("mysession"); got != want {
		t.Errorf("RelayEnvUnsetCmd = %q, want %q", got, want)
	}
}

func TestCellPx(t *testing.T) {
	tests := []struct {
		name, w, h, want string
	}{
		{"typical", "15", "28", "15x28"},
		{"max", "512", "512", "512x512"},
		{"min", "1", "1", "1x1"},
		{"width over max", "513", "28", ""},
		{"height over max", "15", "513", ""},
		{"zero", "0", "0", ""},
		{"zero width", "0", "28", ""},
		{"both empty", "", "", ""},
		{"empty height", "15", "", ""},
		{"empty width", "", "28", ""},
		{"negative", "-1", "8", ""},
		{"decimal", "1.5", "8", ""},
		{"non-numeric", "abc", "8", ""},
		{"leading zero", "015", "28", ""},
		{"plus sign", "+5", "8", ""},
		{"whitespace", " 15", "28", ""},
		{"huge", "99999999999999999999", "28", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CellPx(tt.w, tt.h); got != tt.want {
				t.Errorf("CellPx(%q, %q) = %q, want %q", tt.w, tt.h, got, tt.want)
			}
		})
	}
}

func TestCellEnvCmd(t *testing.T) {
	want := "set-environment -t 'mysession' AEYE_CELL_PX '15x28'"
	if got := CellEnvCmd("mysession", "15x28"); got != want {
		t.Errorf("CellEnvCmd = %q, want %q", got, want)
	}
}

func TestCellEnvCmdQuotesSessionName(t *testing.T) {
	want := `set-environment -t 'my'\''session' AEYE_CELL_PX '15x28'`
	if got := CellEnvCmd("my'session", "15x28"); got != want {
		t.Errorf("CellEnvCmd with quote in session name = %q, want %q", got, want)
	}
}

func TestCellEnvUnsetCmd(t *testing.T) {
	want := "set-environment -u -t 'mysession' AEYE_CELL_PX"
	if got := CellEnvUnsetCmd("mysession"); got != want {
		t.Errorf("CellEnvUnsetCmd = %q, want %q", got, want)
	}
}

// An unknown cell size publishes nothing: the sync command removes the
// variable rather than setting it empty or to a guess.
func TestCellEnvSyncCmd(t *testing.T) {
	if got, want := CellEnvSyncCmd("mysession", "15x28"), CellEnvCmd("mysession", "15x28"); got != want {
		t.Errorf("CellEnvSyncCmd with a size = %q, want %q", got, want)
	}
	if got, want := CellEnvSyncCmd("mysession", ""), CellEnvUnsetCmd("mysession"); got != want {
		t.Errorf("CellEnvSyncCmd with no size = %q, want %q", got, want)
	}
}
