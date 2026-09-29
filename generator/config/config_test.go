package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const minimal = `
platform = "linux"

[tmux]
prefix = "` + "`" + `"
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown key",
			body: minimal + "not_a_key = 1\n",
			want: "unknown key(s): tmux.not_a_key",
		},
		{
			name: "unknown table",
			body: minimal + "\n[nope]\nx = 1\n",
			want: "unknown key(s): nope",
		},
		{
			name: "bad copy_mode_line_numbers",
			body: minimal + "copy_mode_line_numbers = \"sideways\"\n",
			want: `tmux.copy_mode_line_numbers: "sideways" is not one of off, default, absolute, relative, hybrid`,
		},
		{
			name: "empty copy_mode_line_numbers is not absent",
			body: minimal + "copy_mode_line_numbers = \"\"\n",
			want: `tmux.copy_mode_line_numbers: "" is not one of`,
		},
		{
			name: "bad picker layout",
			body: minimal + "\n[picker]\nlayout = \"grid\"\n",
			want: `picker.layout: "grid" is not one of preview, list`,
		},
		{
			name: "bad splash remote",
			body: minimal + "\n[splash]\nremote = \"partial\"\n",
			want: `splash.remote: "partial" is not one of full, static, skip`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.body))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestEnumDefaultsWhenAbsent(t *testing.T) {
	c, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tmux.CopyModeLineNumbers != "off" {
		t.Errorf("copy_mode_line_numbers = %q, want off", c.Tmux.CopyModeLineNumbers)
	}
	if c.Picker.Layout != "preview" {
		t.Errorf("picker.layout = %q, want preview", c.Picker.Layout)
	}
	if c.Splash.Remote != "full" {
		t.Errorf("splash.remote = %q, want full", c.Splash.Remote)
	}
}

func TestDefaultShellAbsentVersusEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *string
	}{
		{"absent", minimal, nil},
		{"empty", minimal + "default_shell = \"\"\n", new("")},
		{"set", minimal + "default_shell = \"/run/current-system/sw/bin/fish\"\n", new("/run/current-system/sw/bin/fish")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(write(t, tt.body))
			if err != nil {
				t.Fatal(err)
			}
			got := c.Tmux.DefaultShell
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("default_shell = %q, want absent", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("default_shell absent, want %q", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Fatalf("default_shell = %q, want %q", *got, *tt.want)
			}
		})
	}
}

func TestTerminalTermAbsentVersusEmpty(t *testing.T) {
	c, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tmux.TerminalTerm != nil {
		t.Fatalf("terminal_term = %q, want absent", *c.Tmux.TerminalTerm)
	}
	c, err = Load(write(t, minimal+"terminal_term = \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tmux.TerminalTerm == nil || *c.Tmux.TerminalTerm != "" {
		t.Fatalf("terminal_term = %v, want present and empty", c.Tmux.TerminalTerm)
	}
}

// A literal '#' is what a user types for an enrich icon override; the doubling
// for tmux format context happens on the Nix side, never here.
func TestLiteralHashSurvivesOverrideIcon(t *testing.T) {
	c, err := Load(write(t, minimal+"\n[enrich.icons]\nconflict = \"#\"\ndraft = \"a#b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Enrich.Icons["conflict"]; got != "#" {
		t.Errorf("conflict = %q, want #", got)
	}
	if got := c.Enrich.Icons["draft"]; got != "a#b" {
		t.Errorf("draft = %q, want a#b", got)
	}
}

func TestVS16Rejection(t *testing.T) {
	body := minimal + "\n[process_icons]\nzed = \"⚡️\"\nclaude = \"\U0001f9e0\"\nabc = \"❤️\"\n"
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("want a VS16 error, got nil")
	}
	want := "process-icons: VS16 emoji (U+FE0F) cause alignment bugs in the picker.\n" +
		"Strip the trailing ️ from: abc, zed\n" +
		"See: charmbracelet/lipgloss#55\n"
	if err.Error() != want {
		t.Fatalf("message =\n%q\nwant\n%q", err.Error(), want)
	}
}

func TestVS16Clean(t *testing.T) {
	if _, err := Load(write(t, minimal+"\n[process_icons]\nclaude = \"\U0001f9e0\"\nnvim = \"\"\n")); err != nil {
		t.Fatal(err)
	}
}

func TestFullConfigDecodes(t *testing.T) {
	body := `
platform = "darwin"

[tmux]
prefix = "b"
default_shell = "/bin/zsh"
focus_follows_mouse = true
copy_mode_line_numbers = "hybrid"
terminal_term = "xterm-ghostty"
sixel_terminals = ["foot", "wezterm"]
extra_config = "set -g mouse on\n"

[picker]
zoxide_exclude = "*/.ssh,/tmp/*"
list_ratio = 40
layout = "list"

[remote]
hosts = "halo mbp"
auth_persist_seconds = 14400

[enrich]
enable = true
providers = ["linear", "github"]
pr_refresh_seconds = 120
pr_check_refresh_seconds = 300

[enrich.icons]
conflict = "#"

[notifications]
enable = false

[agent_usage]
enable = true
refresh_seconds = 120
monthly_threshold = 50

[claude_status]
assume_dead_after = 30

[splash]
enable = true
remote = "static"

[ai_naming]
enable = true

[resume]
claude = true
carousel = false

[process_icons]
claude = "x"
`
	c, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if c.Platform != "darwin" {
		t.Errorf("platform = %q", c.Platform)
	}
	if len(c.Tmux.SixelTerminals) != 2 || c.Tmux.SixelTerminals[0] != "foot" {
		t.Errorf("sixel_terminals = %v", c.Tmux.SixelTerminals)
	}
	if c.Remote.Hosts != "halo mbp" {
		t.Errorf("remote.hosts = %q", c.Remote.Hosts)
	}
	if c.Notifications.Enable {
		t.Error("notifications.enable = true, want false")
	}
	if c.ClaudeStatus.AssumeDeadAfter != 30 {
		t.Errorf("assume_dead_after = %d", c.ClaudeStatus.AssumeDeadAfter)
	}
	if !c.AINaming.Enable || !c.Resume.Claude || c.Resume.Carousel {
		t.Errorf("ai_naming/resume = %v %v %v", c.AINaming.Enable, c.Resume.Claude, c.Resume.Carousel)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Defaults()
	if err != nil {
		t.Fatal(err)
	}
	if c.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", c.Platform, runtime.GOOS)
	}
	if c.Tmux.CopyModeLineNumbers != "off" {
		t.Errorf("copy_mode_line_numbers = %q, want off", c.Tmux.CopyModeLineNumbers)
	}
	if c.Picker.Layout != "preview" {
		t.Errorf("picker.layout = %q, want preview", c.Picker.Layout)
	}
	if c.Splash.Remote != "full" {
		t.Errorf("splash.remote = %q, want full", c.Splash.Remote)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Run("xdg set", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/xdg-home")
		p, err := DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join("/xdg-home", "tmux-og", "config.toml"); p != want {
			t.Errorf("path = %q, want %q", p, want)
		}
	})
	t.Run("xdg unset", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		p, err := DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".config", "tmux-og", "config.toml"); p != want {
			t.Errorf("path = %q, want %q", p, want)
		}
	})
}
