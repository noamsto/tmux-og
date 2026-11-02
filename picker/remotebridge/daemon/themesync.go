package daemon

import (
	"fmt"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// themeForFlavor is og-remote-theme's mapping: latte is light, every other
// flavor dark.
func themeForFlavor(flavor string) string {
	if strings.TrimSpace(flavor) == "latte" {
		return "light"
	}
	return "dark"
}

// shouldSyncTheme: an inconclusive probe still syncs, since the apply script is
// a no-op without theme-toggle.
func shouldSyncTheme(available, checked bool) bool { return available || !checked }

// syncThemeOnConnect applies the local tmux's theme to the remote on a bridge
// connect or reconnect, so a mirror opened after a toggle (or one that missed
// the og-remote-theme fan-out) does not keep the remote's old theme. The remote
// is compared by mapped theme, not raw flavor, and left alone when it already
// matches: several daemons of one host connect at once, and concurrent
// theme-toggle runs on the remote race (#871).
func syncThemeOnConnect(rt roundTrip, cfg Config, available bool) {
	if !available || cfg.LocalTmuxOut == nil {
		return
	}
	flavor, err := cfg.LocalTmuxOut("show-options", "-gv", "@catppuccin_flavor")
	if err != nil || strings.TrimSpace(flavor) == "" {
		return
	}
	theme := themeForFlavor(flavor)

	read := fmt.Sprintf("display-message -p -t %s %s", tmuxQuote(cfg.RemoteSession), tmuxQuote("#{@catppuccin_flavor}"))
	l, ok := one(rt, read)
	if ok && l.Kind != controlmode.Error {
		if remote := strings.TrimSpace(string(l.Data)); remote != "" && themeForFlavor(remote) == theme {
			return
		}
	}

	// A bare run-shell -b resolves its output pane to the client's current pane
	// (the mirrored one, #871); that is safe only because themeApplyScript
	// discards its output and always exits 0.
	one(rt, "run-shell -b "+tmuxQuote("exec /bin/sh -c "+tmuxQuote(themeApplyScript(theme))))
}
