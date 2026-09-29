// Package config decodes and validates config.toml — the generator's half of
// the interface config/tmux.conf.nix serializes its arguments into.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors config.toml key for key. Decoding is strict, so a key added on
// the Nix side without a field here fails the build instead of being dropped.
type Config struct {
	Platform string `toml:"platform"`

	Tmux          Tmux          `toml:"tmux"`
	Picker        Picker        `toml:"picker"`
	Remote        Remote        `toml:"remote"`
	Enrich        Enrich        `toml:"enrich"`
	Notifications Notifications `toml:"notifications"`
	AgentUsage    AgentUsage    `toml:"agent_usage"`
	ClaudeStatus  ClaudeStatus  `toml:"claude_status"`
	Splash        Splash        `toml:"splash"`
	AINaming      AINaming      `toml:"ai_naming"`
	Resume        Resume        `toml:"resume"`

	// The merged map (process-icons.nix // extraProcessIcons), never the
	// overrides alone — the defaults have exactly one home, on the Nix side.
	ProcessIcons map[string]string `toml:"process_icons"`
}

type Tmux struct {
	Prefix string `toml:"prefix"`
	// Pointers because absent and empty are different downstream: an absent
	// shell emits no default-shell line, an empty one would emit a broken one.
	DefaultShell        *string  `toml:"default_shell"`
	FocusFollowsMouse   bool     `toml:"focus_follows_mouse"`
	CopyModeLineNumbers string   `toml:"copy_mode_line_numbers"`
	TerminalTerm        *string  `toml:"terminal_term"`
	SixelTerminals      []string `toml:"sixel_terminals"`
	ExtraConfig         string   `toml:"extra_config"`
}

type Picker struct {
	ZoxideExclude string `toml:"zoxide_exclude"`
	ListRatio     int    `toml:"list_ratio"`
	Layout        string `toml:"layout"`
}

type Remote struct {
	Hosts              string `toml:"hosts"`
	AuthPersistSeconds int    `toml:"auth_persist_seconds"`
}

type Enrich struct {
	Enable                bool     `toml:"enable"`
	Providers             []string `toml:"providers"`
	PrRefreshSeconds      int      `toml:"pr_refresh_seconds"`
	PrCheckRefreshSeconds int      `toml:"pr_check_refresh_seconds"`
	// User overrides only, single '#' as typed; the defaults stay in Nix.
	Icons map[string]string `toml:"icons"`
}

type Notifications struct {
	Enable bool `toml:"enable"`
}

type AgentUsage struct {
	Enable           bool `toml:"enable"`
	RefreshSeconds   int  `toml:"refresh_seconds"`
	MonthlyThreshold int  `toml:"monthly_threshold"`
}

type ClaudeStatus struct {
	AssumeDeadAfter int `toml:"assume_dead_after"`
}

type Splash struct {
	Enable bool   `toml:"enable"`
	Remote string `toml:"remote"`
}

type AINaming struct {
	Enable bool `toml:"enable"`
}

type Resume struct {
	Claude   bool `toml:"claude"`
	Carousel bool `toml:"carousel"`
}

var (
	copyModeLineNumbersValues = []string{"off", "default", "absolute", "relative", "hybrid"}
	pickerLayoutValues        = []string{"preview", "list"}
	splashRemoteValues        = []string{"full", "static", "skip"}
)

// enumField pairs an enum key with the config/tmux.conf.nix argument default it
// falls back to when the file omits it. A key that is present but empty stays
// empty and fails validation: only Nix's null-is-absent rule may leave one out.
type enumField struct {
	key      []string
	target   *string
	allowed  []string
	fallback string
}

// Load reads and validates path.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			names = append(names, k.String())
		}
		sort.Strings(names)
		return nil, fmt.Errorf("config: unknown key(s): %s", strings.Join(names, ", "))
	}
	c.applyDefaults(&md)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Defaults returns what applyDefaults would produce for an empty config.toml —
// the enum fallbacks a non-Nix caller (e.g. og init) can offer without reading
// or validating an actual file.
func Defaults() (*Config, error) {
	var c Config
	md, err := toml.Decode("", &c)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults(&md)
	return &c, nil
}

// DefaultPath returns where a non-Nix config.toml should live:
// $XDG_CONFIG_HOME/tmux-og/config.toml, or ~/.config/tmux-og/config.toml when
// XDG_CONFIG_HOME is unset. Deliberately not os.UserConfigDir(), which returns
// a macOS-specific path on darwin — see scripts/lib-log.sh and
// scripts/lib-claude.sh for the same Linux-convention fallback spelled out.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "tmux-og", "config.toml"), nil
}

func (c *Config) applyDefaults(md *toml.MetaData) {
	// Nix always writes platform from pkgs.stdenv.hostPlatform; off Nix the
	// generator runs on the host it is generating for, so GOOS is right there.
	if !md.IsDefined("platform") {
		c.Platform = runtime.GOOS
	}
	for _, f := range c.enumFields() {
		if !md.IsDefined(f.key...) {
			*f.target = f.fallback
		}
	}
}

func (c *Config) enumFields() []enumField {
	return []enumField{
		{[]string{"tmux", "copy_mode_line_numbers"}, &c.Tmux.CopyModeLineNumbers, copyModeLineNumbersValues, "off"},
		{[]string{"picker", "layout"}, &c.Picker.Layout, pickerLayoutValues, "preview"},
		{[]string{"splash", "remote"}, &c.Splash.Remote, splashRemoteValues, "full"},
	}
}

func (c *Config) validate() error {
	for _, f := range c.enumFields() {
		if !contains(f.allowed, *f.target) {
			return fmt.Errorf("config: %s: %q is not one of %s",
				strings.Join(f.key, "."), *f.target, strings.Join(f.allowed, ", "))
		}
	}
	return c.checkVS16()
}

// checkVS16 reproduces the builtins.throw in config/tmux.conf.nix byte for
// byte: a variation-selector-16 in an icon renders one cell wider than the
// picker's width arithmetic assumes (charmbracelet/lipgloss#55).
func (c *Config) checkVS16() error {
	var names []string
	for k, v := range c.ProcessIcons {
		if strings.ContainsRune(v, '️') {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return &VS16Error{Names: names}
}

// VS16Error carries the Nix message verbatim, trailing newline included.
type VS16Error struct {
	Names []string
}

func (e *VS16Error) Error() string {
	return "process-icons: VS16 emoji (U+FE0F) cause alignment bugs in the picker.\n" +
		"Strip the trailing ️ from: " + strings.Join(e.Names, ", ") + "\n" +
		"See: charmbracelet/lipgloss#55\n"
}

func contains(set []string, v string) bool {
	return slices.Contains(set, v)
}
