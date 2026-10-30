// Package initcfg builds the commented config.toml template og init writes
// for a fresh, non-Nix install. It holds only the pure text-generation logic;
// the og init CLI (flags, file I/O, prompts) is a separate layer on top.
package initcfg

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/noamsto/tmux-og/generator/config"
)

// defaultPrefix and defaultAuthPersistSeconds are og init's own defaults for
// the two fields generator/config has no code-level fallback for. Unlike the
// enum fields (copy_mode_line_numbers, picker.layout, splash.remote),
// config.Defaults() leaves Tmux.Prefix and Remote.AuthPersistSeconds at their
// Go zero value, which is unsafe to write live: an empty prefix breaks the
// `set-option -g prefix` / `bind send-prefix` lines in config/tmux.conf.tmpl,
// and auth_persist_seconds = 0 feeds `ssh -o ControlPersist=0`, reaping the
// ControlMaster instantly. These two constants are manually kept in sync with
// modules/home-manager.nix's own `prefix` and `remote.authPersistSeconds`
// option defaults — Go cannot evaluate Nix, so nothing else keeps them in
// sync; a change to either Nix default should update the matching constant
// here.
const (
	defaultPrefix             = "`"
	defaultAuthPersistSeconds = 14400
)

// fieldDocs is a one-line description for every leaf field in config.Config,
// keyed by the dotted TOML path a reflection-based test derives from the
// struct's own toml tags. TestRenderCoversEveryField asserts every such path
// has an entry here, and that Render's output contains the path as a TOML
// key (live or commented).
var fieldDocs = map[string]string{
	"platform": "Which OS tmux-og is generating for (linux or darwin); controls platform-specific defaults like the copy-mode clipboard command. " +
		"Detected from this host. og generate re-detects this itself at build time if you remove the line — a config.toml authored on one host and copied to another shouldn't pin the wrong OS.",

	"tmux.prefix": "The tmux prefix key (a literal character) used for every prefix-based keybinding. " +
		"og init's own default — generator/config has no fallback for this field; matches the Nix module's default.",
	"tmux.default_shell":          "Absolute path to the shell tmux spawns panes with; leave unset to inherit the user's own default login shell.",
	"tmux.focus_follows_mouse":    "Whether moving the mouse over a pane focuses it without clicking.",
	"tmux.copy_mode_line_numbers": "Line-number style shown in copy mode: off, default, absolute, relative, or hybrid.",
	"tmux.terminal_term":          "Override for the TERM tmux assumes when picking terminal features; leave unset to use tmux's own detection.",
	"tmux.sixel_terminals":        "TERM values that should be told they support sixel graphics, for image relaying across the remote bridge.",
	"tmux.extra_config":           "Raw tmux config appended verbatim at the end of the generated file, for one-off options this schema doesn't expose.",

	"picker.zoxide_exclude": "Comma-separated glob patterns to exclude from the picker's zoxide directory suggestions.",
	"picker.list_ratio":     "Percentage of the picker popup's width given to the list column versus the preview pane.",
	"picker.layout":         "Picker popup layout: preview (list plus a live preview) or list (list only).",

	"remote.hosts": "Space-separated ssh host aliases the session picker probes for remote tmux sessions to bridge.",
	"remote.auth_persist_seconds": "How long an ssh ControlMaster tmux-og creates survives idle, in seconds; passed to ssh's ControlPersist. " +
		"og init's own default — generator/config has no fallback for this field; matches the Nix module's default.",

	"enrich.enable": "Whether to show per-window Linear/GitHub issue identity and PR check-state in the status line.",
	"enrich.providers": "Issue-tracking providers to check, in priority order (e.g. linear, github) — first non-empty match wins. " +
		"Not yet wired into og generate's output off-Nix (Nix path bakes these into scripts directly) — editing this here currently has no effect.",
	"enrich.pr_refresh_seconds": "How often to poll for PR identity changes, in seconds. " +
		"Not yet wired into og generate's output off-Nix (Nix path bakes these into scripts directly) — editing this here currently has no effect.",
	"enrich.pr_check_refresh_seconds": "How often to poll the more expensive PR CI-check rollup, in seconds. " +
		"Not yet wired into og generate's output off-Nix (Nix path bakes these into scripts directly) — editing this here currently has no effect.",
	"enrich.icons": "Overrides for the enrichment glyphs (linear, github, pending, success, failure, merged, closed, conflict, draft). Keys are icon names.",

	"notifications.enable": "Whether to wire up desktop notifications for tmux bell and activity events.",

	"agent_usage.enable": "Whether to show per-agent (Claude/Codex/Cursor) rate-limit utilization on the status line.",
	"agent_usage.refresh_seconds": "How often to poll each agent CLI's own usage endpoint, in seconds. " +
		"Not yet wired into og generate's output off-Nix (Nix path bakes these into scripts directly) — editing this here currently has no effect.",
	"agent_usage.monthly_threshold": "Utilization percentage at or above which the monthly usage window is shown.",

	"claude_status.assume_dead_after": "Seconds of no activity from an agent pane before its last-known state is withdrawn as stale; 0 disables this. " +
		"Not yet wired into og generate's output off-Nix (Nix path bakes these into scripts directly) — editing this here currently has no effect.",

	"splash.enable": "Whether to show the welcome splash screen once per tmux server, on first attach.",
	"splash.remote": "What the splash does on an ssh attach: full, static (bandwidth-light), or skip.",

	"ai_naming.enable": "Whether Claude Code gets nudged to generate a window title for unnamed fallback windows.",

	"resume.claude":   "Whether a restored pane that was running Claude Code relaunches `claude --resume <uuid>` instead of a bare shell.",
	"resume.carousel": "Whether the image carousel reopens automatically for a restored pane, replaying images from the resumed Claude session's transcript.",

	"dwindle.enable": "Dwindle auto-balance: when a pane closes, even out the split group it leaves. prefix + A toggles it per window.",

	"process_icons": "Per-process-name icon overrides shown in window tabs and the status line. " +
		"Real defaults live only in config/process-icons.nix (Nix-only data) — this stays empty off-Nix; see that file.",
}

// Render builds the full text of a commented config.toml, documenting every
// field in config.Config. Every field is written in exactly one of three
// ways:
//   - platform is written live, detected from this host.
//   - tmux.prefix and remote.auth_persist_seconds are written live, at
//     og init's own hardcoded defaults (see defaultPrefix and
//     defaultAuthPersistSeconds above) — config.Defaults() has no safe
//     fallback for either.
//   - everything else is written commented out, at the value
//     config.Defaults() produces for it, with a one-line description.
//
// Every commented line uses a fixed "# " prefix (never "## ", never a bare
// "#" before a value) — a drift-guard test decomments the file by stripping
// exactly that prefix and re-decoding it, so the prefix must be exact and
// every doc string must ride as a trailing inline comment on its own line
// rather than a standalone comment line: a standalone prose line would
// decode as invalid TOML once decommented.
func Render() (string, error) {
	d, err := config.Defaults()
	if err != nil {
		return "", fmt.Errorf("initcfg: %w", err)
	}

	var b strings.Builder
	b.WriteString("#tmux-og config.toml\n\n")

	writeLive(&b, "platform", strLit(d.Platform), "platform")
	b.WriteString("\n")

	b.WriteString("[tmux]\n")
	writeLive(&b, "prefix", strLit(defaultPrefix), "tmux.prefix")
	writeCommented(&b, "default_shell", optStrLit(d.Tmux.DefaultShell), "tmux.default_shell")
	writeCommented(&b, "focus_follows_mouse", boolLit(d.Tmux.FocusFollowsMouse), "tmux.focus_follows_mouse")
	writeCommented(&b, "copy_mode_line_numbers", strLit(d.Tmux.CopyModeLineNumbers), "tmux.copy_mode_line_numbers")
	writeCommented(&b, "terminal_term", optStrLit(d.Tmux.TerminalTerm), "tmux.terminal_term")
	writeCommented(&b, "sixel_terminals", sliceLit(d.Tmux.SixelTerminals), "tmux.sixel_terminals")
	writeCommented(&b, "extra_config", strLit(d.Tmux.ExtraConfig), "tmux.extra_config")
	b.WriteString("\n")

	b.WriteString("[picker]\n")
	writeCommented(&b, "zoxide_exclude", strLit(d.Picker.ZoxideExclude), "picker.zoxide_exclude")
	writeCommented(&b, "list_ratio", intLit(d.Picker.ListRatio), "picker.list_ratio")
	writeCommented(&b, "layout", strLit(d.Picker.Layout), "picker.layout")
	b.WriteString("\n")

	b.WriteString("[remote]\n")
	writeCommented(&b, "hosts", strLit(d.Remote.Hosts), "remote.hosts")
	writeLive(&b, "auth_persist_seconds", intLit(defaultAuthPersistSeconds), "remote.auth_persist_seconds")
	b.WriteString("\n")

	b.WriteString("[enrich]\n")
	writeCommented(&b, "enable", boolLit(d.Enrich.Enable), "enrich.enable")
	writeCommented(&b, "providers", sliceLit(d.Enrich.Providers), "enrich.providers")
	writeCommented(&b, "pr_refresh_seconds", intLit(d.Enrich.PrRefreshSeconds), "enrich.pr_refresh_seconds")
	writeCommented(&b, "pr_check_refresh_seconds", intLit(d.Enrich.PrCheckRefreshSeconds), "enrich.pr_check_refresh_seconds")
	writeCommentedTable(&b, "[enrich.icons]", "enrich.icons")
	b.WriteString("\n")

	b.WriteString("[notifications]\n")
	writeCommented(&b, "enable", boolLit(d.Notifications.Enable), "notifications.enable")
	b.WriteString("\n")

	b.WriteString("[agent_usage]\n")
	writeCommented(&b, "enable", boolLit(d.AgentUsage.Enable), "agent_usage.enable")
	writeCommented(&b, "refresh_seconds", intLit(d.AgentUsage.RefreshSeconds), "agent_usage.refresh_seconds")
	writeCommented(&b, "monthly_threshold", intLit(d.AgentUsage.MonthlyThreshold), "agent_usage.monthly_threshold")
	b.WriteString("\n")

	b.WriteString("[claude_status]\n")
	writeCommented(&b, "assume_dead_after", intLit(d.ClaudeStatus.AssumeDeadAfter), "claude_status.assume_dead_after")
	b.WriteString("\n")

	b.WriteString("[splash]\n")
	writeCommented(&b, "enable", boolLit(d.Splash.Enable), "splash.enable")
	writeCommented(&b, "remote", strLit(d.Splash.Remote), "splash.remote")
	b.WriteString("\n")

	b.WriteString("[ai_naming]\n")
	writeCommented(&b, "enable", boolLit(d.AINaming.Enable), "ai_naming.enable")
	b.WriteString("\n")

	b.WriteString("[resume]\n")
	writeCommented(&b, "claude", boolLit(d.Resume.Claude), "resume.claude")
	writeCommented(&b, "carousel", boolLit(d.Resume.Carousel), "resume.carousel")
	b.WriteString("\n")

	b.WriteString("[dwindle]\n")
	writeCommented(&b, "enable", boolLit(d.Dwindle.Enable), "dwindle.enable")
	b.WriteString("\n")

	writeCommentedTable(&b, "[process_icons]", "process_icons")

	return b.String(), nil
}

func writeLive(b *strings.Builder, key, literal, path string) {
	fmt.Fprintf(b, "%s = %s  # %s\n", key, literal, fieldDocs[path])
}

func writeCommented(b *strings.Builder, key, literal, path string) {
	fmt.Fprintf(b, "# %s = %s  # %s\n", key, literal, fieldDocs[path])
}

func writeCommentedTable(b *strings.Builder, header, path string) {
	fmt.Fprintf(b, "# %s  # %s\n", header, fieldDocs[path])
}

func strLit(s string) string { return strconv.Quote(s) }

func optStrLit(s *string) string {
	if s == nil {
		return `""`
	}
	return strconv.Quote(*s)
}

func boolLit(v bool) string { return strconv.FormatBool(v) }

func intLit(v int) string { return strconv.Itoa(v) }

func sliceLit(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	quoted := make([]string, len(s))
	for i, v := range s {
		quoted[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
