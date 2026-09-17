package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/noamsto/themestate"
)

// detectTheme prefers the live tmux flavor (@catppuccin_flavor) over the
// themestate file, matching the picker package's themeFromOpts — but this
// is a separate binary/package, so the logic is duplicated locally rather
// than shared.
func detectTheme() string {
	if os.Getenv("TMUX") == "" {
		return themestate.Detect()
	}
	out, err := exec.Command("tmux", "show-options", "-gqv", "@catppuccin_flavor").Output()
	flavor := strings.TrimSpace(string(out))
	if err != nil || flavor == "" {
		return themestate.Detect()
	}
	if flavor == "latte" {
		return "light"
	}
	return "dark"
}

func main() {
	// --no-timeout: dismiss on keypress only (for on-demand launch, where the
	// auto-dismiss timeout of the fresh-session welcome would be wrong).
	timeout := splashTimeoutSec
	if slices.Contains(os.Args[1:], "--no-timeout") {
		timeout = 0
	}
	// --static: single already-resolved frame, no periodic redraw (for
	// bandwidth-light remote/SSH attaches).
	static := slices.Contains(os.Args[1:], "--static")
	m := newModel(detectTheme(), splashTips, splashPrefix, timeout, static)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		os.Exit(1)
	}
}
