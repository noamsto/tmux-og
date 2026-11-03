package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

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

func TestRelayOwnedCond(t *testing.T) {
	want := "#{?#{@og_bridge_owner},#{m:tok1|*,#{@og_bridge_owner}},1}"
	if got := relayOwnedCond("tok1"); got != want {
		t.Errorf("relayOwnedCond = %q, want %q", got, want)
	}
}

func TestRelayEnvBuilders(t *testing.T) {
	const cond = "'#{?#{@og_bridge_owner},#{m:tok1|*,#{@og_bridge_owner}},1}'"
	tests := []struct {
		name string
		env  relayEnv
		got  func(relayEnv) string
		want string
	}{
		{"unguarded relay", relayEnv{target: "$3"}, func(e relayEnv) string { return e.relay("sixel") },
			"set-environment -t '$3' OG_RELAY_GRAPHICS 'sixel'"},
		{"unguarded relay empty", relayEnv{target: "$3"}, func(e relayEnv) string { return e.relay("") },
			"set-environment -t '$3' OG_RELAY_GRAPHICS ''"},
		{"unguarded cell", relayEnv{target: "$3"}, func(e relayEnv) string { return e.cell("15x28") },
			"set-environment -t '$3' AEYE_CELL_PX '15x28'"},
		{"unguarded cell empty unsets", relayEnv{target: "$3"}, func(e relayEnv) string { return e.cell("") },
			"set-environment -u -t '$3' AEYE_CELL_PX"},
		{"guarded relay", relayEnv{target: "$3", token: "tok1"}, func(e relayEnv) string { return e.relay("sixel") },
			"if-shell -F -t '$3' " + cond + " 'set-environment -t '\\''$3'\\'' OG_RELAY_GRAPHICS '\\''sixel'\\'''"},
		{"guarded cell", relayEnv{target: "$3", token: "tok1"}, func(e relayEnv) string { return e.cell("15x28") },
			"if-shell -F -t '$3' " + cond + " 'set-environment -t '\\''$3'\\'' AEYE_CELL_PX '\\''15x28'\\'''"},
		{"guarded cell empty unsets", relayEnv{target: "$3", token: "tok1"}, func(e relayEnv) string { return e.cell("") },
			"if-shell -F -t '$3' " + cond + " 'set-environment -u -t '\\''$3'\\'' AEYE_CELL_PX'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got(tt.env); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestRelayEnvPublish(t *testing.T) {
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(true, ""))}
	view.SetCell("15x28")

	tests := []struct {
		name string
		env  relayEnv
		want []string
	}{
		{"unguarded", relayEnv{target: "$3"}, []string{
			"set-environment -t '$3' OG_RELAY_GRAPHICS 'sixel'",
			"set-environment -t '$3' AEYE_CELL_PX '15x28'",
		}},
		{"guarded", relayEnv{target: "$3", token: "tok1"}, []string{
			"if-shell -F -t '$3' '#{?#{@og_bridge_owner},#{m:tok1|*,#{@og_bridge_owner}},1}' 'set-environment -t '\\''$3'\\'' OG_RELAY_GRAPHICS '\\''sixel'\\'''",
			"if-shell -F -t '$3' '#{?#{@og_bridge_owner},#{m:tok1|*,#{@og_bridge_owner}},1}' 'set-environment -t '\\''$3'\\'' AEYE_CELL_PX '\\''15x28'\\'''",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.env.publish(view); !slices.Equal(got, tt.want) {
				t.Errorf("publish = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelayEnvTeardownCmd(t *testing.T) {
	want := "if-shell -F -t '$3' '#{==:#{session_attached},2}' 'set-environment -u -t '\\''$3'\\'' OG_RELAY_GRAPHICS ; set-environment -u -t '\\''$3'\\'' AEYE_CELL_PX'"
	if got := relayEnvTeardownCmd("$3", 2); got != want {
		t.Errorf("relayEnvTeardownCmd = %q, want %q", got, want)
	}
}

func pressView(sixel bool, cell string) *Viewing {
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(sixel, ""))}
	view.SetCell(cell)
	return view
}

func TestPressEnvCmds(t *testing.T) {
	relayOnly := []string{"set-environment -t 'S' OG_RELAY_GRAPHICS 'sixel'"}
	tests := []struct {
		name   string
		target string
		view   *Viewing
		want   []string
	}{
		{
			name: "sixel with a cell", target: "S", view: pressView(true, "15x28"),
			want: []string{
				"set-environment -t 'S' OG_RELAY_GRAPHICS 'sixel'",
				"set-environment -t 'S' AEYE_CELL_PX '15x28'",
			},
		},
		{
			name: "non-sixel clears the relay", target: "S", view: pressView(false, "9x18"),
			want: []string{
				"set-environment -t 'S' OG_RELAY_GRAPHICS ''",
				"set-environment -t 'S' AEYE_CELL_PX '9x18'",
			},
		},
		{name: "empty cell", target: "S", view: pressView(true, ""), want: relayOnly},
		{name: "zero width", target: "S", view: pressView(true, "0x28"), want: relayOnly},
		{name: "missing height", target: "S", view: pressView(true, "15x"), want: relayOnly},
		{name: "oversize width", target: "S", view: pressView(true, "513x1"), want: relayOnly},
		{name: "three axes", target: "S", view: pressView(true, "1x2x3"), want: relayOnly},
		{name: "command separator", target: "S", view: pressView(true, "15x28;x"), want: relayOnly},
		{name: "substitution", target: "S", view: pressView(true, "$(id)x1"), want: relayOnly},
		{name: "single quote", target: "S", view: pressView(true, `a'b`), want: relayOnly},
		{name: "nil view", target: "S", view: nil, want: nil},
		{
			name: "target is quoted", target: "it's a #(touch x)", view: pressView(true, "15x28"),
			want: []string{
				`set-environment -t 'it'\''s a #(touch x)' OG_RELAY_GRAPHICS 'sixel'`,
				`set-environment -t 'it'\''s a #(touch x)' AEYE_CELL_PX '15x28'`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pressEnvCmds(tt.target, tt.view)
			if !slices.Equal(got, tt.want) {
				t.Errorf("pressEnvCmds = %q, want %q", got, tt.want)
			}
		})
	}
}

// The target is the one free-form string in the batch, so it must come out as
// exactly tmuxQuote(target), with no raw occurrence outside the quoted token.
func TestPressEnvCmdsQuotesTheTarget(t *testing.T) {
	const target = "it's a #(touch x)"
	quoted := tmuxQuote(target)
	for _, cmd := range pressEnvCmds(target, pressView(true, "15x28")) {
		if !strings.Contains(cmd, quoted) {
			t.Errorf("%q does not carry %q", cmd, quoted)
		}
		if rest := strings.Replace(cmd, quoted, "", 1); strings.Contains(rest, "#(") || strings.Contains(rest, "touch") {
			t.Errorf("%q leaks the raw target outside %q", cmd, quoted)
		}
	}
}

func TestRelayEnvRelinquishCmd(t *testing.T) {
	want := "if-shell -F -t '$3' '#{m:tok1|*,#{@og_bridge_owner}}' 'set-environment -t '\\''$3'\\'' OG_RELAY_GRAPHICS '\\'''\\'' ; set-environment -u -t '\\''$3'\\'' AEYE_CELL_PX'"
	if got := relayEnvRelinquishCmd("$3", "tok1"); got != want {
		t.Errorf("relayEnvRelinquishCmd = %q, want %q", got, want)
	}
}
