#!/usr/bin/env bats
# Tests the pane-border-format ternary built by scripts/tmux-apply-theme-colors.sh,
# and the -O/-K/-C flags adopted on the ^o remote-picker float (#648).
#
# Runs against a private, config-less tmux server (like tests/float-refit.bats)
# so `display-message -p -F` evaluates the REAL script's output, not a
# hand-copied string that could drift from what ships. Needs the pinned
# next-3.8 tmux (mkTmux in flake.nix) for -O/-K/-C/-B/-X/-Y, which nixpkgs'
# stock tmux only advertises via `list-commands` and then rejects at parse
# time (see float-refit.bats's header comment on the same trap).

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	export TMUX_TMPDIR="/tmp/og-pbf-$$-${BATS_TEST_NUMBER}"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	# A private TMUX_TMPDIR does not isolate CLAUDE_STATUS_DIR (CLAUDE.md: it
	# defaults to a bare /tmp path shared by every tmux server on the
	# machine) — isolate it anyway, matching every other scratch-server test.
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	tmux -f /dev/null new-session -d -s S -x 80 -y 24
	WIN="$(tmux -u display-message -p -t S '#{window_id}')"

	# All eight @thm_* variables the script reads (only bg/overlay_1/mauve/green
	# feed pane-border-format, but the script also reads crust/teal/yellow/
	# surface_1 for the @fingers-*-style lines below it — set all eight so
	# hex_to_256 has real input instead of running on empty strings).
	tmux set -g @thm_crust '#11111b'
	tmux set -g @thm_bg '#1e1e2e'
	tmux set -g @thm_overlay_1 '#7f849c'
	tmux set -g @thm_mauve '#cba6f7'
	tmux set -g @thm_green '#a6e3a1'
	tmux set -g @thm_teal '#94e2d5'
	tmux set -g @thm_yellow '#f9e2af'
	tmux set -g @thm_surface_1 '#45475a'

	SCRIPT="$(dirname "$BATS_TEST_DIRNAME")/scripts/tmux-apply-theme-colors.sh"
	bash "$SCRIPT"
	# -u matters here too, not just on the later display-message calls: `show
	# -gv` is itself a querying client, and the nix check sandbox's non-UTF-8
	# locale (CLAUDE.md's "tmux `-F` formats" gotcha, which names this sandbox
	# explicitly) makes the SERVER rewrite the option's multi-byte ━/● glyphs
	# to `_` on the way out — baking the corruption into $FMT before any test
	# body runs, so no later `-u` on display-message can undo it.
	FMT="$(tmux -u show -gv pane-border-format)"
	# Every read of $FMT/$BROKEN_FMT below also goes through `tmux -u
	# display-message`, for the same reason.

	# The pre-fix string: the two #[bg=X,fg=Y] occurrences the real script
	# used to emit, reconstructed here only as a negative control so this
	# test cannot silently degrade into a tautology if the split is ever
	# undone. Colors match the @thm_* values set above.
	BROKEN_FMT='#{?@pane_label,#{?pane_active,#[fg=#cba6f7]━━ #{@pane_label} ━━,#[bg=#1e1e2e,fg=#7f849c]━━ #{@pane_label} ━━},#{?@bridge_crew_role,━━ #[bold]#{@bridge_crew_role}#[nobold] #{@bridge_crew_state} ━━,#{?@bridge_crew_name,━━ #[bold]#{@bridge_crew_name}#[nobold] ━━,#{?#{&&:#{pane_active},#{&&:#{>:#{window_panes},1},#{==:#{window_zoomed_flag},0}}},#[fg=#cba6f7]━━ #[fg=#a6e3a1]●#[fg=#cba6f7] ━━,#[bg=#1e1e2e,fg=#7f849c]━━━━━}}}}'
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "$TMUX_TMPDIR"
}

@test "labeled float, active pane: renders the mauve title" {
	tmux set -p -t "$WIN" @pane_label lazygit
	out="$(tmux -u display-message -p -t "$WIN" -F "$FMT")"
	[[ $out == *"lazygit"* ]]
}

@test "labeled float, inactive pane: renders the dim title (regression, was silently empty)" {
	tmux split-window -t "$WIN"
	inactive="$(tmux list-panes -t "$WIN" -F '#{pane_id} #{pane_active}' | awk '$2==0{print $1}')"
	tmux set -p -t "$inactive" @pane_label lazygit
	out="$(tmux -u display-message -p -t "$inactive" -F "$FMT")"
	[[ $out == *"lazygit"* ]]

	# Negative control: the pre-fix string genuinely renders empty here, so
	# this case is provably red-capable, not just a restated assertion.
	broken="$(tmux -u display-message -p -t "$inactive" -F "$BROKEN_FMT")"
	[[ -z $broken ]]
}

@test "bridged crew role renders bold role + state" {
	tmux set -p -t "$WIN" @bridge_crew_role reviewer
	tmux set -p -t "$WIN" @bridge_crew_state working
	out="$(tmux -u display-message -p -t "$WIN" -F "$FMT")"
	[[ $out == *"reviewer"* ]]
	[[ $out == *"working"* ]]
}

@test "bridged crew name (no role) renders the codename" {
	tmux set -p -t "$WIN" @bridge_crew_name coral
	out="$(tmux -u display-message -p -t "$WIN" -F "$FMT")"
	[[ $out == *"coral"* ]]
}

@test "multi-pane active marker renders the green dot" {
	tmux split-window -t "$WIN"
	active="$(tmux list-panes -t "$WIN" -F '#{pane_id} #{pane_active}' | awk '$2==1{print $1}')"
	out="$(tmux -u display-message -p -t "$active" -F "$FMT")"
	[[ $out == *●* ]]
}

@test "true default (single pane, no label/bridge): dim bar, no garbage (regression)" {
	out="$(tmux -u display-message -p -t "$WIN" -F "$FMT")"
	[[ $out == *"━━━━━"* ]]

	broken="$(tmux -u display-message -p -t "$WIN" -F "$BROKEN_FMT")"
	[[ -z $broken ]]
}

@test "script never joins bg+fg into one comma-separated #[] block (regression, #648)" {
	# The actual bug: a single #[bg=X,fg=Y] block is misparsed by tmux's
	# #{?...} comma-splitter as an extra branch boundary. The fix keeps each
	# #[...] block to one attribute; assert the comma-joined pair the old
	# script emitted (see BROKEN_FMT above) never appears in the real one.
	[[ $FMT != *'#[bg=#1e1e2e,fg=#7f849c]'* ]]
}

@test "the ^o float's -O -K -C flags are accepted, and -O enforces one modal per window" {
	if ! tmux new-pane -t "$WIN" -O -K -C -x 60% -y 60% -X 20% -Y 20% -B heavy -A sh 2>/dev/null; then
		skip "this tmux advertises -O/-K/-C but rejects them at parse time"
	fi
	floating="$(tmux list-panes -t "$WIN" -F '#{pane_floating_flag}' | tr -d '\n')"
	case "$floating" in
	*1*) ;;
	*) skip "this tmux does not report pane_floating_flag" ;;
	esac

	# -O's modal enforcement is a command-level check tmux does itself, so
	# this doesn't need a real client/pty to verify: a second -O float in the
	# same window must be refused outright.
	run tmux new-pane -t "$WIN" -O -K -C -x 40% -y 40% -X 30% -Y 30% -B heavy -A sh
	[ "$status" -ne 0 ]
	[[ $output == *"modal"* ]]
}
