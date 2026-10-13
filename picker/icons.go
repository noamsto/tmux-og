package main

import "github.com/noamsto/tmux-og/picker/ogconfig"

// Built-in defaults for a binary run without $TMUX_OG_CONFIG; the module's
// wrapper always sets it, replacing iconMap, fallbackIcon and maxIconsPicker.
//
// Keep iconMap in sync with config/process-icons.nix when adding entries.

func applyConfig() error {
	cfg, ok, err := ogconfig.Load()
	if !ok {
		return err
	}
	iconMap, fallbackIcon, maxIconsPicker = cfg.ProcessIcons, cfg.FallbackIcon, cfg.MaxIconsPicker
	return nil
}

var iconMap = map[string]string{
	// Custom / project-specific
	"claude":          "🧠",
	"amp":             "⚡",
	"nh":              "❄",
	"process-compose": "⚙",

	// Shells
	"bash": "",
	"nu":   "",
	"zsh":  "",
	"tcsh": "",

	// Editors
	"emacs": "",
	"hx":    "",
	"lvim":  "",
	"nano":  "",
	"nvim":  "",
	"vi":    "",
	"vim":   "",

	// Version control
	"git":     "",
	"gh":      "",
	"gitui":   "",
	"lazygit": "",
	"lazyjj":  "",
	"jj":      "",
	"tig":     "",

	// Languages & runtimes
	"cargo":   "",
	"deno":    "",
	"go":      "",
	"java":    "",
	"node":    "",
	"perl":    "",
	"php":     "",
	"python":  "",
	"python3": "",
	"Python":  "",
	"ruby":    "",
	"rustc":   "",
	"rustup":  "",
	"scala":   "",
	"swift":   "",
	"zig":     "↯",

	// Package managers
	"apt":    "",
	"brew":   "",
	"dnf":    "",
	"nix":    "❄",
	"npm":    "",
	"pacman": "",
	"paru":   "",
	"pip":    "",
	"pip3":   "",
	"yarn":   "",
	"yay":    "",

	// Build tools
	"cmake":  "",
	"gcc":    "",
	"gradle": "",
	"just":   "",
	"make":   "",
	"bazel":  "",

	// Containers & cloud
	"docker":     "",
	"helm":       "󱃾",
	"k9s":        "󱃾",
	"kubectl":    "󱃾",
	"lazydocker": "",
	"terraform":  "",
	"aws":        "",
	"gcloud":     "",

	// System & monitoring
	"btm":       "",
	"btop":      "",
	"htop":      "",
	"top":       "",
	"glances":   "",
	"sudo":      "",
	"systemctl": "",

	// Network
	"curl":  "",
	"ping":  "",
	"ssh":   "󰣀",
	"scp":   "󰣀",
	"wget":  "",
	"gping": "",

	// Databases
	"mongo":  "",
	"mysql":  "",
	"psql":   "",
	"redis":  "",
	"sqlite": "",

	// File managers & tools
	"bat":    "󰭟",
	"lf":     "",
	"ranger": "",
	"yazi":   "",
	"rsync":  "",
	"zip":    "",
	"unzip":  "",

	// Terminals & multiplexers
	"tmux":   "",
	"screen": "",

	// Other
	"gpg":      "",
	"ghostty":  "",
	"topgrade": "",
	"weechat":  "",
}

var fallbackIcon = ""

var maxIconsPicker = 5

// Claude status icons (keep in sync with scripts/lib-claude.sh)
var claudeSpinnerFrames = []string{"󰪞", "󰪟", "󰪠", "󰪡", "󰪢", "󰪣", "󰪤", "󰪥"}
var claudeIconWaiting = "󰔟"
var claudeIconCompacting = "󰡍"
var claudeIconDone = "󰸞"
var claudeIconIdle = "󰒲"
var claudeIconError = "󰅚"
var claudeIconDenied = "󰔟" // same clock as waiting, different color
var claudeIconBG = "󰅐"     // nf-md-timer_sand — background shells still running

// Default UI icons — overridden at runtime by env vars or tmux options.
var iconSession = ""
var iconDir = ""
var iconBranch = ""
var iconHost = ""
var iconProcs = "󰀻"
var iconCPU = ""
var iconMem = ""
var iconRemote = ""
