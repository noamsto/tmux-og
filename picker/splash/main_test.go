package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubTmux drops a fake `tmux` binary on PATH that echoes output for
// `show-options -gqv @catppuccin_flavor` and exits with code. Mirrors the
// stub-tmux-on-PATH convention tests/lib-claude-theme.bats already uses for
// the shell twin of this mapping (setup_claude_colors).
func stubTmux(t *testing.T, output string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub tmux script assumes a POSIX shell")
	}
	dir := t.TempDir()
	// Single-quoted shell literal (POSIX-escaped, not Go's %q): the output
	// values here are fixed test literals with no shell metacharacters, but
	// %q's Go-syntax escaping doesn't touch $ or ` — a future caller passing
	// either would get shell expansion instead of the literal string.
	quoted := "'" + strings.ReplaceAll(output, "'", `'\''`) + "'"
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\nexit %d\n", quoted, exitCode)
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDetectTheme(t *testing.T) {
	t.Run("TMUX unset never execs tmux, falls to the themestate file", func(t *testing.T) {
		t.Setenv("TMUX", "")
		// No stub on PATH at all: if detectTheme tried to exec tmux here,
		// the exec would fail and it would still fall through, but pinning
		// the file confirms which path was actually taken.
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		if err := os.WriteFile(filepath.Join(dir, "theme-state.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := detectTheme(); got != "light" {
			t.Errorf("detectTheme() = %q, want %q", got, "light")
		}
	})

	t.Run("TMUX set, live flavor latte", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/fake-socket,1,0")
		stubTmux(t, "latte", 0)
		if got := detectTheme(); got != "light" {
			t.Errorf("detectTheme() = %q, want %q", got, "light")
		}
	})

	t.Run("TMUX set, live flavor mocha", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/fake-socket,1,0")
		stubTmux(t, "mocha", 0)
		if got := detectTheme(); got != "dark" {
			t.Errorf("detectTheme() = %q, want %q", got, "dark")
		}
	})

	t.Run("TMUX set but tmux exits non-zero, falls to the themestate file", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/fake-socket,1,0")
		stubTmux(t, "", 1)
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		// Pinned to "light", distinct from themestate.Detect()'s "dark"
		// default: a mutation that hardcoded the fallback to "dark" instead
		// of actually reading the file would otherwise pass this case too.
		if err := os.WriteFile(filepath.Join(dir, "theme-state.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := detectTheme(); got != "light" {
			t.Errorf("detectTheme() = %q, want %q", got, "light")
		}
	})

	t.Run("TMUX set but tmux prints nothing, falls to the themestate file", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/fake-socket,1,0")
		stubTmux(t, "", 0)
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		if err := os.WriteFile(filepath.Join(dir, "theme-state.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := detectTheme(); got != "light" {
			t.Errorf("detectTheme() = %q, want %q", got, "light")
		}
	})
}
