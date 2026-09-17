// Package paths resolves every store path the template emits, from either a
// paths.toml Nix wrote or a --prefix DIR laid out by convention. Both modes
// yield one type, so nothing downstream knows which one it came from.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Paths holds final executable paths, verbatim — the template never joins
// segments, so this resolver is the only thing that knows a layout.
type Paths struct {
	Bash    string            `toml:"bash"`
	Scripts map[string]string `toml:"scripts"`
	Bin     map[string]string `toml:"bin"`
	Plugins map[string]string `toml:"plugins"`

	// Optional: nil is absent, which renders a conditional off. Never "" —
	// an empty path would emit a syntactically valid line that runs nothing.
	PersistWireScript *string `toml:"persist_wire_script"`
	CarouselToggle    *string `toml:"carousel_toggle"`
	CarouselAeye      *string `toml:"carousel_aeye"`
	Prdash            *string `toml:"prdash"`
}

// RequiredScripts are the ${script.<name>} sites the template has.
var RequiredScripts = []string{
	"claude-status-update",
	"og-debug",
	"og-notify",
	"og-notify-center",
	"og-remote-auth",
	"og-remote-detach",
	"og-remote-open",
	"og-remote-picker",
	"og-remote-theme",
	"tmux-agent-usage",
	"tmux-apply-theme-colors",
	"tmux-client-theme",
	"tmux-default-size",
	"tmux-float-refit",
	"tmux-issue-stamp",
	"tmux-kill-pane-guard",
	"tmux-pr-enrich",
	"tmux-reap-pane",
	"tmux-reconcile-window",
	"tmux-reflow-windows",
	"tmux-scratchpad",
	"tmux-session-picker",
	"tmux-shell-prompt",
	"tmux-splash-maybe",
	"tmux-update-icons",
	"tmux-which-key",
	"tmux-window-nav",
	"tmux-window-picker",
	"tmux-window-wall",
}

// RequiredBin are the Go binaries the template names directly.
var RequiredBin = []string{
	"og-remote-bridge-ctl",
	"tmux-enrich-card",
	"tmux-splash",
	"tmux-statusline",
}

// pluginEntries maps each plugin to its .tmux entry file, which is what the
// template sources — never the package root.
var pluginEntries = map[string]string{
	"better-mouse-mode":  "scroll_copy_mode.tmux",
	"catppuccin":         "catppuccin.tmux",
	"fingers":            "tmux-fingers.tmux",
	"tmux-fzf":           "main.tmux",
	"vim-tmux-navigator": "vim-tmux-navigator.tmux",
}

// optionalBinNames are the basenames --prefix mode probes for under DIR/bin.
var optionalBinNames = map[string]string{
	"persist_wire_script": "tmux-remux-wire",
	"carousel_toggle":     "tmux-claude-images",
	"carousel_aeye":       "aeye",
	"prdash":              "prdash",
}

// Load decodes a paths.toml.
func Load(path string) (*Paths, error) {
	var p Paths
	if _, err := toml.DecodeFile(path, &p); err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// FromPrefix synthesizes the same map from a conventional install tree.
func FromPrefix(dir string) (*Paths, error) {
	bin := filepath.Join(dir, "bin")
	p := Paths{
		Bash:    filepath.Join(bin, "bash"),
		Scripts: make(map[string]string, len(RequiredScripts)),
		Bin:     make(map[string]string, len(RequiredBin)),
		Plugins: make(map[string]string, len(pluginEntries)),
	}
	for _, n := range RequiredScripts {
		p.Scripts[n] = filepath.Join(bin, n)
	}
	for _, n := range RequiredBin {
		p.Bin[n] = filepath.Join(bin, n)
	}
	for name, entry := range pluginEntries {
		p.Plugins[name] = filepath.Join(dir, "share", "tmux-og", "plugins", name, entry)
	}
	optionals := map[string]**string{
		"persist_wire_script": &p.PersistWireScript,
		"carousel_toggle":     &p.CarouselToggle,
		"carousel_aeye":       &p.CarouselAeye,
		"prdash":              &p.Prdash,
	}
	for key, target := range optionals {
		candidate := filepath.Join(bin, optionalBinNames[key])
		// Only a genuine absence is a disabled feature: an unreadable path
		// would otherwise silently drop the feature it names.
		switch _, err := os.Stat(candidate); {
		case err == nil:
			*target = &candidate
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("paths: %s: %w", key, err)
		}
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// validate reports every missing required key at once — a resolver that failed
// on the first would need one run per missing path to enumerate them.
func (p *Paths) validate() error {
	var missing []string
	if p.Bash == "" {
		missing = append(missing, "bash")
	}
	for _, n := range RequiredScripts {
		if p.Scripts[n] == "" {
			missing = append(missing, "scripts."+n)
		}
	}
	for _, n := range RequiredBin {
		if p.Bin[n] == "" {
			missing = append(missing, "bin."+n)
		}
	}
	for name := range pluginEntries {
		if p.Plugins[name] == "" {
			missing = append(missing, "plugins."+name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("paths: missing required key(s): %s", strings.Join(missing, ", "))
}
