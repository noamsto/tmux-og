package daemon

import (
	"errors"
	"strings"
	"testing"
)

func TestShouldSyncTheme(t *testing.T) {
	for _, c := range []struct{ available, checked, want bool }{
		{false, false, true},
		{true, true, true},
		{true, false, true},
		{false, true, false},
	} {
		if got := shouldSyncTheme(c.available, c.checked); got != c.want {
			t.Errorf("shouldSyncTheme(%v, %v) = %v, want %v", c.available, c.checked, got, c.want)
		}
	}
}

func TestThemeSyncOnConnect(t *testing.T) {
	reply := func(s string) string { return "%begin 1 1 1\n" + s + "%end 1 1 1\n" }
	local := func(flavor string) func(...string) (string, error) {
		return func(...string) (string, error) { return flavor + "\n", nil }
	}
	tests := []struct {
		name      string
		available bool
		local     func(...string) (string, error)
		script    string
		wantTheme string // "" means no run-shell
	}{
		{"toggle absent", false, local("mocha"), reply("latte\n"), ""},
		{"remote same theme", true, local("latte"), reply("latte\n"), ""},
		{"mapped compare frappe vs mocha", true, local("mocha"), reply("frappe\n"), ""},
		{"differs to light", true, local("latte"), reply("mocha\n"), "light"},
		{"differs to dark", true, local("mocha"), reply("latte\n"), "dark"},
		{"remote reply empty", true, local("mocha"), reply(""), "dark"},
		{"remote reply error", true, local("mocha"), "%begin 1 1 1\nbad\n%error 1 1 1\n", "dark"},
		{"local read error", true, func(...string) (string, error) { return "", errors.New("no server") }, reply("latte\n"), ""},
		{"local read empty", true, local(""), reply("latte\n"), ""},
		{"nil LocalTmuxOut", true, nil, reply("latte\n"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, sent := scriptedRT(tc.script)
			cfg := Config{RemoteSession: "g6-work", LocalTmuxOut: tc.local}
			syncThemeOnConnect(rt, cfg, tc.available)
			got := sent.String()
			if tc.wantTheme == "" {
				if strings.Contains(got, "run-shell") {
					t.Errorf("sent run-shell, want none: %q", got)
				}
				return
			}
			want := "theme-toggle apply " + tc.wantTheme
			if !strings.Contains(got, "run-shell -b") || !strings.Contains(got, want) {
				t.Errorf("sent %q, want run-shell -b containing %q", got, want)
			}
		})
	}
}
