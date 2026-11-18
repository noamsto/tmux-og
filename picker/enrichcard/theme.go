package main

import (
	"os/exec"
	"strings"

	"github.com/noamsto/tmux-og/picker/palette"
)

// readGlobalOptions runs one `tmux show-options -g` and parses it into a
// name→value map; any error yields an empty map (the card then keeps its flags).
func readGlobalOptions() map[string]string {
	out, err := exec.Command("tmux", "show-options", "-g").Output() //nolint:gosec // argv is fixed and exec'd directly, no shell
	if err != nil {
		return nil
	}
	opts := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		opts[name] = unquote(strings.TrimSpace(val))
	}
	return opts
}

// lightCfg replaces the card's colours with Latte palette roles when the
// flavor is latte; dark keeps the launch flags unchanged.
func lightCfg(c cfg, opts map[string]string) cfg {
	if opts["@catppuccin_flavor"] != "latte" {
		return c
	}
	role := func(r palette.Role) string { return palette.ResolveTheme(opts, true, r) }
	c.fg = role(palette.Text)
	c.mauve = role(palette.Mauve)
	c.red = role(palette.Warn)
	c.green = role(palette.Success)
	c.peach = role(palette.Highlight)
	c.blue = role(palette.Accent)
	c.overlay0 = role(palette.Muted)
	c.subtext0 = role(palette.Primary)
	return c
}
