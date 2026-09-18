package render

import (
	"fmt"
	"strings"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
)

// bridgeGate is the format that says "this is a live mirror pane": @bridge_win
// (window) and @bridge_pane (pane) are stamped by the bridge daemon. Requiring
// both means a pane the daemon does not own falls through to the local action.
const bridgeGate = "#{&&:#{@bridge_win},#{@bridge_pane}}"

// bridgeCtl is the ctl invocation every gated bind's remote branch runs.
// @bridge_sock is always in --sock= form, never word-initial.
func bridgeCtl(p *paths.Paths) string {
	return p.Bin["og-remote-bridge-ctl"] + " --display-error=#{q:client_name} --sock=#{q:@bridge_sock}"
}

// floatShape is one float geometry in the three forms the binds need. tmux
// resolves the percentages into absolute cells at creation and never revisits
// them, so stamp carries them forward for tmux-float-refit; both come from one
// place here so they cannot drift.
//
// flags carries -A, which is a Z-ORDER flag — the floating pane stays visible
// above a zoomed pane — and not attach-if-exists: new-pane has no such mode and
// every press creates a pane. The reuse of an already-open float is explicit,
// in floatReuse (#679).
type floatShape struct {
	flags    string
	flagsNoA string
	stamp    string
}

func mkFloat(w, h, x, y string) floatShape {
	base := fmt.Sprintf("-x %s -y %s -X %s -Y %s -B heavy", w, h, x, y)
	return floatShape{
		flags:    base + " -A",
		flagsNoA: base,
		stamp:    fmt.Sprintf("set -p @float_geom '%s %s %s %s' \\; set -p remain-on-exit off", w, h, x, y),
	}
}

var (
	floatFull  = mkFloat("90%", "90%", "5%", "5%")
	floatShort = mkFloat("90%", "85%", "5%", "8%")
	// The enrich card sizes to its contents, not to the client; only its
	// offsets are percentages, and those are what walk off a shrinking window.
	floatCard = mkFloat("64", "18", "20%", "15%")
)

// floatNewPaneGuard picks -A only on a server that has it. String-form
// if-shell, because a brace block parses every branch at source time and an
// older server would reject the unknown flag there (#407).
//
// The escaping is scoped to the inner new-pane command: it becomes the body of
// a double-quoted if-shell argument, while the if-shell wrapper's own quotes
// must stay bare.
func floatNewPaneGuard(f floatShape, prefix, suffix string) string {
	mk := func(flags string) string {
		return strings.ReplaceAll(
			fmt.Sprintf("new-pane %s%s %s \\; %s", prefix, flags, suffix, f.stamp),
			`"`, `\"`)
	}
	return fmt.Sprintf(`if-shell "tmux list-commands new-pane | grep -q -- -A" "%s" "%s"`,
		mk(f.flags), mk(f.flagsNoA))
}

func floatBind(key, note string, f floatShape, prefix, suffix string) string {
	return "bind-key -N '" + note + "' " + key + " " + floatNewPaneGuard(f, prefix, suffix)
}

// floatRegister is the window option a bridged tool press hands the float's pane
// id to its own focus branch through. It is not a second lookup key: the value
// is written by the branch that reads it, in the same command list, and that
// branch is only reached once floatLookup has already matched — so it is never
// consulted across a press and can never be stale. It exists because a pane loop
// nested inside a run-shell argument is a shell-injection shape this repo guards
// against (tests/conf-shell-quoting.bats): every #{...} in a shell string must be
// #{q:NAME} or #{qs:NAME} with a plain body, and `#{q:<option>}` is the one
// legal way to hand an id over. Per tool, so a read can never name another
// tool's float even in principle.
//
// picker/remotebridge/daemon/ctl.go builds the same expression for the remote
// leg, kept in step by hand like the float geometry above: the two modules share
// no code.
func floatRegister(tool string) string { return "@og_float_target_" + tool }

// floatLookup is the pane loop behind one tool press: it expands to the pane id
// of the window's float carrying that tool's @pane_label, or to nothing when
// there is none (which is falsey in if-shell -F, so no #{?:} wrapper is needed).
// pane_floating_flag keeps the predicate honest — @pane_label is a border title,
// and a tiled pane wearing it is not the float to reuse.
func floatLookup(tool string) string {
	return fmt.Sprintf("#{P:#{?#{&&:#{==:#{@pane_label},%s},#{pane_floating_flag}},#{pane_id},}}", tool)
}

// floatReuse wraps the version-guarded new-pane so a press for a tool whose
// float is already open in the window focuses that float instead of stacking
// another one at the same geometry (#679). The lookup key is @pane_label, which
// the create branch already stamps, so there is no new state to keep in sync.
//
// Brace blocks for the branches, unlike floatNewPaneGuard's string form: nothing
// in them is version-gated, so parsing them at source time is safe, and a brace
// block needs no further escaping around the guard's own quoted bodies. The
// guard keeps its string-form if-shell so an older server never sees -A at
// source (#407).
func floatReuse(tool, guard string) string {
	loop, reg := floatLookup(tool), floatRegister(tool)
	return fmt.Sprintf(`if-shell -F "%s" { set -wF %s "%s" ; run-shell "tmux select-pane -t #{q:%s}" } { %s }`,
		loop, reg, loop, reg, guard)
}

// bridgedFloatTool hands the tool to the ctl `tool` verb in a mirror window:
// #{pane_current_path} there expands on the renderer pane, which is the
// daemon's cwd rather than the remote worktree on screen.
//
// @bridge_dir rides along because the remote cannot resolve the cwd either: a
// -c format expands against the client's current pane, not the -t target, so
// the remote leg would open the tool in whichever window the remote is on
// (#643). It is #{qs:}, not #{q:} — the value is a path, and run-shell hands it
// to a shell that would otherwise split it on a space. An unset option quotes
// as an empty argument, which the verb reads as "no cwd".
func bridgedFloatTool(p *paths.Paths, key, note, tool string, f floatShape, prefix, suffix string) string {
	return fmt.Sprintf("bind-key -N '%s' %s if-shell -F '%s' { run-shell \"%s tool #{q:@bridge_pane} %s #{qs:@bridge_dir}\" } { %s }",
		note, key, bridgeGate, bridgeCtl(p), tool, floatReuse(tool, floatNewPaneGuard(f, prefix, suffix)))
}

// carouselBind is empty when the toggle package is not wired in. It carries no
// trailing newline: the template line it sits on supplies one, so an absent
// bind leaves exactly one empty line. Same contract as prdashBind.
func carouselBind(p *paths.Paths) string {
	if p.CarouselToggle == nil {
		return ""
	}
	return fmt.Sprintf("bind -N 'Toggle image carousel' I if-shell -F '%s' { run-shell \"%s carousel #{q:@bridge_pane}\" } { run-shell 'TMUX_PANE=#{q:pane_id} %s' }",
		bridgeGate, bridgeCtl(p), *p.CarouselToggle)
}

func prdashBind(p *paths.Paths) string {
	if p.Prdash == nil {
		return ""
	}
	return bridgedFloatTool(p, "p", "Open PR dashboard", "prdash", floatShort, "-c '#{pane_current_path}' ",
		*p.Prdash+" \\; set -p @pane_label prdash")
}

func lazygitBind(p *paths.Paths) string {
	return bridgedFloatTool(p, "g", "Open lazygit", "lazygit", floatFull, "-c '#{pane_current_path}' ",
		"lazygit \\; set -p @pane_label lazygit")
}

func yaziBind(p *paths.Paths) string {
	return bridgedFloatTool(p, "y", "Open yazi file manager", "yazi", floatShort, "-c '#{pane_current_path}' ",
		"yazi \\; set -p @pane_label yazi")
}

func btopBind() string {
	return floatBind("b", "Open btop", floatFull, "", "btop \\; set -p @pane_label btop")
}

// k9s is reached through PATH only, unlike the binds above: a pkgs.k9s
// fallback dragged k9s + kubectl into every closure for a bind only k8s users
// press.
func k9sBind() string {
	return floatBind("k", "Open k9s", floatFull, "",
		`"command -v k9s >/dev/null 2>&1 && exec k9s || { echo 'k9s not found in PATH — add pkgs.k9s to programs.tmux-og.popupTools'; read -r; }" \; set -p @pane_label k9s`)
}

// enrichIconDefaults are the Nerd Font (Material Design) glyph defaults;
// config.toml carries the user's overrides alone, so the defaults live here.
var enrichIconDefaults = map[string]string{
	"linear":   "󰰍",
	"github":   "󰊤",
	"pending":  "󰦖",
	"success":  "󰗠",
	"failure":  "󰀨",
	"merged":   "󰘭",
	"closed":   "󰅖",
	"conflict": "󰀦",
	"draft":    "",
}

// enrichIconsRaw is the glyph the user typed, un-doubled. The card's stdout is
// not re-parsed as a tmux format, so ##-escaped glyphs must not reach it.
func enrichIconsRaw(cfg *config.Config) map[string]string {
	m := make(map[string]string, len(enrichIconDefaults))
	for k, v := range enrichIconDefaults {
		m[k] = v
	}
	for k, v := range cfg.Enrich.Icons {
		m[k] = v
	}
	return m
}

// enrichCardBind is a plain floatBind, never bridgeGate'd: the card only reads
// local window options, and launching it on the remote would put [o]/[p]'s
// xdg-open on a headless machine. The continuation lines' eight-space indent
// is part of the emitted command, not source formatting.
func enrichCardBind(cfg *config.Config, p *paths.Paths) string {
	i := enrichIconsRaw(cfg)
	suffix := fmt.Sprintf(`"%s \
        --target '#{session_id}:#{window_id}' \
        --pr-enrich-bin '%s' \
        --bridge-ctl-bin '%s' \
        --bridge-sock '#{@bridge_sock}' --bridge-pane '#{@bridge_pane}' \
        --issue-stamp-bin '%s' \
        --thm-fg '#{@thm_fg}' --thm-mauve '#{@thm_mauve}' \
        --thm-red '#{@thm_red}' --thm-green '#{@thm_green}' --thm-peach '#{@thm_peach}' \
        --thm-blue '#{@thm_blue}' --thm-overlay0 '#{@thm_overlay_0}' \
        --thm-subtext0 '#{@thm_subtext_0}' \
        --icon-linear '%s' --icon-github '%s' \
        --icon-pending '%s' --icon-success '%s' \
        --icon-failure '%s' --icon-merged '%s' \
        --icon-closed '%s' --icon-conflict '%s' \
        --icon-draft '%s'" \; set -p @pane_label enrich`,
		p.Bin["tmux-enrich-card"],
		p.Scripts["tmux-pr-enrich"],
		p.Bin["og-remote-bridge-ctl"],
		p.Scripts["tmux-issue-stamp"],
		i["linear"], i["github"],
		i["pending"], i["success"],
		i["failure"], i["merged"],
		i["closed"], i["conflict"],
		i["draft"])
	return floatBind("i", "Show issue/PR enrichment card", floatCard, "", suffix)
}
