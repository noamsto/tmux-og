#!/usr/bin/env bats
# Tests the pane-border-format ternary and pane-{active-,}border-style built by
# scripts/tmux-apply-theme-colors.sh, and the -O/-K/-C flags adopted on the ^o
# remote-picker float (#648).
#
# Runs against a private, config-less tmux server (like tests/float-refit.bats)
# so `display-message -p -F` evaluates the REAL script's output, not a
# hand-copied string that could drift from what ships. Needs the pinned
# next-3.8 tmux (mkTmux in flake.nix) for -O/-K/-C/-B/-X/-Y, which nixpkgs'
# stock tmux only advertises via `list-commands` and then rejects at parse
# time (see float-refit.bats's header comment on the same trap).

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	OG_TMUX_DIR="/tmp/og-pbf-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	# A private TMUX_TMPDIR does not isolate CLAUDE_STATUS_DIR (CLAUDE.md: it
	# defaults to a bare /tmp path shared by every tmux server on the
	# machine) — isolate it anyway, matching every other scratch-server test.
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"

	tmux -f /dev/null new-session -d -s S -x 80 -y 24
	WIN="$(tmux -u display-message -p -t S '#{window_id}')"

	# All ten @thm_* variables the script reads (only bg/overlay_1/mauve/green/
	# peach/red feed pane-border-format, but the script also reads crust/teal/
	# yellow/surface_1 for the @fingers-*-style lines below it — set all ten so
	# hex_to_256 has real input instead of running on empty strings).
	tmux set -g @thm_crust '#11111b'
	tmux set -g @thm_bg '#1e1e2e'
	tmux set -g @thm_overlay_1 '#7f849c'
	tmux set -g @thm_mauve '#cba6f7'
	tmux set -g @thm_green '#a6e3a1'
	tmux set -g @thm_teal '#94e2d5'
	tmux set -g @thm_yellow '#f9e2af'
	tmux set -g @thm_surface_1 '#45475a'
	tmux set -g @thm_peach '#fab387'
	tmux set -g @thm_red '#f38ba8'

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
	tmux -L og-outer kill-server 2>/dev/null || true
	tmux kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

# --- helpers ---

# Strips #[...] style directives so assertions compare visible text only.
plain() {
	sed 's/#\[[^]]*\]//g'
}

# Renders the real $FMT (as evaluated live from the script's setw -g) for a
# given pane/window target, with directives stripped.
render() {
	tmux -u display-message -p -t "$1" -F "$FMT" | plain
}

# Stamps a full window title (id + rest) on $WIN, as tmux-reflow-windows does
# for every window (mirrors included), plus the clipped stamp that makes the
# border show it: a border title only appears once the status label is clipped.
title() {
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long 'feat: full title'
	tmux setw -t "$WIN" @window_label_clipped 1
}

# A fake aeye binary so the pane_start_command detector matches.
fake_aeye() {
	mkdir -p "$OG_TMUX_DIR/bin"
	cat >"$OG_TMUX_DIR/bin/aeye" <<-'EOF'
		#!/bin/sh
		exec sleep 60
	EOF
	chmod +x "$OG_TMUX_DIR/bin/aeye"
}

# A border-style option resolved for one pane.
style() {
	case "$2" in
	active) tmux -u display -p -t "$1" '#{E:pane-active-border-style}' ;;
	inactive) tmux -u display -p -t "$1" '#{E:pane-border-style}' ;;
	esac
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

@test "float label wins over the anchor's title" {
	title
	if ! tmux new-pane -t "$WIN" -O -K -C -x 60% -y 60% -X 20% -Y 20% -B heavy -A sh 2>/dev/null; then
		skip "this tmux advertises -O/-K/-C but rejects them at parse time"
	fi
	float="$(tmux list-panes -t "$WIN" -f '#{pane_floating_flag}' -F '#{pane_id}' | head -n1)"
	[ -n "$float" ] || skip "this tmux does not report pane_floating_flag"
	tmux set -p -t "$float" @pane_label lazygit
	out="$(render "$float")"
	[ "$out" = "━━ lazygit ━━" ]
}

@test "aeye pane detected by start command, anchor keeps its title" {
	tmux setw -t "$WIN" @crew_name coral
	tmux setw -t "$WIN" @window_has_agent 1
	tmux setw -t "$WIN" @crew_color colour99
	title
	fake_aeye
	anchor="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux split-window -h -d -t "$anchor" "$OG_TMUX_DIR/bin/aeye 7"
	aeye_pane="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${anchor}$")"

	out_aeye="$(render "$aeye_pane")"
	[ "$out_aeye" = "━━ aeye ━━" ]

	out_anchor="$(render "$anchor")"
	[ "$out_anchor" = "━━ coral · #858 feat: full title ━━" ]

	style_aeye="$(style "$aeye_pane" inactive)"
	[[ $style_aeye == *"fg=#7f849c"* ]]
	[[ $style_aeye != *"colour99"* ]]

	style_anchor="$(style "$anchor" inactive)"
	[[ $style_anchor == *"colour99"* ]]
}

@test "aeye pane detected by @claude_img_src option" {
	title
	anchor="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux split-window -h -d -t "$anchor"
	aeye_pane="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${anchor}$")"
	tmux set -p -t "$aeye_pane" @claude_img_src '1-%1'

	out="$(render "$aeye_pane")"
	[ "$out" = "━━ aeye ━━" ]
}

@test "mirror role pane renders bold role + glyph + state, no title" {
	tmux setw -t "$WIN" @bridge_win 1
	title
	left="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux split-window -h -d -t "$left"
	right="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${left}$")"
	tmux set -p -t "$right" @bridge_crew_role reviewer
	tmux set -p -t "$right" @bridge_crew_state working

	out="$(render "$right")"
	[ "$out" = "━━ reviewer ● working ━━" ]

	tmux set -p -t "$right" @bridge_crew_role_color colour114
	style_out="$(style "$right" inactive)"
	[[ $style_out == *"fg=colour114"* ]]
}

@test "bridged crew name (no role) renders the codename" {
	tmux setw -t "$WIN" @bridge_win 1
	tmux setw -t "$WIN" @bridge_crew_name coral
	out="$(render "$WIN")"
	[ "$out" = "━━ coral ━━" ]
}

@test "bridged crew name without @bridge_win renders the plain bar" {
	tmux setw -t "$WIN" @bridge_crew_name coral
	out="$(render "$WIN")"
	[ "$out" = "━━━━━" ]
}

@test "a #(...) carried in a codename or title renders as text, never runs" {
	# Wide enough that the title budget is not spent on the codename.
	tmux resize-window -t "$WIN" -x 200 -y 24
	tmux setw -t "$WIN" @bridge_win 1
	tmux setw -t "$WIN" @bridge_crew_name "#(touch $OG_TMUX_DIR/ran-name)"
	tmux setw -t "$WIN" @window_label_rest_long "#(touch $OG_TMUX_DIR/ran-title)"
	tmux setw -t "$WIN" @window_label_clipped 1
	out="$(render "$WIN")"
	[[ $out == *"#(touch $OG_TMUX_DIR/ran-name)"* ]]
	[[ $out == *"#(touch $OG_TMUX_DIR/ran-title)"* ]]
	sleep 0.2
	[ ! -e "$OG_TMUX_DIR/ran-name" ]
	[ ! -e "$OG_TMUX_DIR/ran-title" ]
}

@test "mirror crew name only renders codename + title" {
	tmux setw -t "$WIN" @bridge_win 1
	tmux setw -t "$WIN" @bridge_crew_name coral
	title
	out="$(render "$WIN")"
	[ "$out" = "━━ coral · #858 feat: full title ━━" ]
}

@test "local crew grid: anchor shows codename+state+title, role panes show their own" {
	# Wide session so the narrow side splits below still leave the anchor (A)
	# enough width to render its full title unclipped (W's budget math is
	# covered separately by the long-title test).
	tmux resize-window -t "$WIN" -x 200 -y 30
	tmux setw -t "$WIN" @crew_name coral
	tmux setw -t "$WIN" @window_has_agent 1
	title
	A="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux set -p -t "$A" @crew_role lead
	tmux set -p -t "$A" @crew_state blocked
	tmux set -p -t "$A" @crew_source watchdog
	tmux set -p -t "$A" @crew_detail 'awaiting reply'

	tmux split-window -h -d -p 15 -t "$A"
	B="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${A}$")"
	tmux set -p -t "$B" @crew_role plan-critic
	tmux set -p -t "$B" @crew_state working
	tmux set -p -t "$B" @crew_role_color colour111

	tmux split-window -v -d -t "$B"
	C="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v -e "^${A}$" -e "^${B}$")"
	tmux set -p -t "$C" @crew_role reviewer

	out_a="$(render "$A")"
	[ "$out_a" = "━━ coral · ⚠ blocked (watchdog) · awaiting reply · #858 feat: full title ━━" ]

	out_b="$(render "$B")"
	[ "$out_b" = "━━ plan-critic ● working ━━" ]
	style_b="$(style "$B" inactive)"
	[[ $style_b == *"fg=colour111"* ]]

	out_c="$(render "$C")"
	[ "$out_c" = "━━ reviewer ━━" ]

	titled=0
	for p in "$A" "$B" "$C"; do
		r="$(render "$p")"
		[[ $r == *"#858"* ]] && titled=$((titled + 1))
	done
	[ "$titled" -eq 1 ]
}

@test "mirror lead pane shows codename + state + title, no 'lead' word" {
	tmux setw -t "$WIN" @bridge_win 1
	tmux setw -t "$WIN" @bridge_crew_name coral
	title
	tmux set -p -t "$WIN" @bridge_crew_role lead
	tmux set -p -t "$WIN" @bridge_crew_state idle
	out="$(render "$WIN")"
	[ "$out" = "━━ coral · ○ idle · #858 feat: full title ━━" ]
}

@test "local crew name without @window_has_agent: no codename, title only" {
	tmux setw -t "$WIN" @crew_name coral
	title
	out="$(render "$WIN")"
	[ "$out" = "━━ #858 feat: full title ━━" ]
}

@test "unclipped label: anchor shows codename and state but no title" {
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long 'feat: full title'
	tmux setw -t "$WIN" @window_has_agent 1
	tmux setw -t "$WIN" @crew_name coral
	[ "$(render "$WIN")" = "━━ coral ━━" ]

	tmux set -p -t "$WIN" @crew_state idle
	[ "$(render "$WIN")" = "━━ coral · ○ idle ━━" ]
}

@test "unclipped, no codename or state: plain bar" {
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long 'feat: full title'
	[ "$(render "$WIN")" = "━━━━━" ]
}

@test "state-only anchor keeps its state whether or not the title is clipped" {
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long 'feat: full title'
	tmux set -p -t "$WIN" @crew_state "done"
	[ "$(render "$WIN")" = "━━ ✓ done ━━" ]

	tmux setw -t "$WIN" @window_label_clipped 1
	[ "$(render "$WIN")" = "━━ ✓ done · #858 feat: full title ━━" ]
}

@test "plain single pane with title shows the title alone" {
	title
	out="$(render "$WIN")"
	[ "$out" = "━━ #858 feat: full title ━━" ]
}

@test "active split shows the dot, inactive anchor keeps the title" {
	title
	left="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux split-window -h -t "$WIN"
	right="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${left}$")"
	tmux select-pane -t "$right"

	out_right="$(render "$right")"
	[ "$out_right" = "━━ ● ━━" ]
	out_left="$(render "$left")"
	[ "$out_left" = "━━ #858 feat: full title ━━" ]

	tmux select-pane -t "$left"
	out_left2="$(render "$left")"
	[ "$out_left2" = "━━ #858 feat: full title ━━" ]
	out_right2="$(render "$right")"
	[ "$out_right2" = "━━━━━" ]
}

@test "lead pane with aeye split: title on the lead, aeye shows its own label" {
	title
	A="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	tmux set -p -t "$A" @crew_role lead
	fake_aeye
	tmux split-window -h -d -t "$A" "$OG_TMUX_DIR/bin/aeye 9"
	aeye_pane="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${A}$")"

	out_aeye="$(render "$aeye_pane")"
	[ "$out_aeye" = "━━ aeye ━━" ]
	out_a="$(render "$A")"
	[[ $out_a == *"#858"* ]]
}

@test "aeye as the anchor pane: no pane shows the title" {
	title
	right="$(tmux display-message -p -t "$WIN" -F '#{pane_id}')"
	fake_aeye
	tmux split-window -h -b -d -t "$right" "$OG_TMUX_DIR/bin/aeye 10"
	aeye_pane="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | grep -v "^${right}$")"

	out_aeye="$(render "$aeye_pane")"
	[ "$out_aeye" = "━━ aeye ━━" ]
	out_right="$(render "$right")"
	[[ $out_right != *"#858"* ]]
}

@test "long title is clipped to the width budget" {
	tmux resize-window -t "$WIN" -x 40 -y 24
	rest="$(printf 'a%.0s' $(seq 1 100))"
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long "$rest"
	tmux setw -t "$WIN" @window_label_clipped 1

	expect_a="$(printf 'a%.0s' $(seq 1 26))"
	out="$(render "$WIN")"
	[ "$out" = "━━ #858 ${expect_a}… ━━" ]

	tmux setw -t "$WIN" @window_has_agent 1
	tmux setw -t "$WIN" @crew_name coral
	expect_a2="$(printf 'a%.0s' $(seq 1 18))"
	out2="$(render "$WIN")"
	[ "$out2" = "━━ coral · #858 ${expect_a2}… ━━" ]
}

@test "a dispatcher-set pane-border-format overrides ours until unset" {
	title
	tmux setw -t "$WIN" pane-border-format 'DISPATCH-LEAD'
	resolved="$(tmux -u display-message -p -t "$WIN" -F '#{pane-border-format}')"
	[ "$resolved" = "DISPATCH-LEAD" ]

	tmux set -p -t "$WIN" pane-border-format 'DISPATCH-ROLE'
	resolved="$(tmux -u display-message -p -t "$WIN" -F '#{pane-border-format}')"
	[ "$resolved" = "DISPATCH-ROLE" ]

	tmux set -p -u -t "$WIN" pane-border-format
	tmux setw -u -t "$WIN" pane-border-format
	resolved="$(tmux -u display-message -p -t "$WIN" -F '#{pane-border-format}')"
	[ "$resolved" = "$FMT" ]
}

@test "state glyph vocabulary" {
	tmux set -p -t "$WIN" @crew_role worker
	for pair in "idle:○" "done:✓" "pr_open:✓" "failed:✗" "exited:✗" "bogus:○"; do
		state="${pair%%:*}"
		glyph="${pair##*:}"
		tmux set -p -t "$WIN" @crew_state "$state"
		out="$(render "$WIN")"
		[[ $out == *"$glyph"* ]]
	done
}

@test "drawn border: a real client renders the clipped title with its ellipsis and wrap" {
	# A long rest fills the entire label width, so the line ends exactly at
	# " ━━" with no trailing fill dashes (a short title leaves the row padded
	# with "─" past the format text, which this assertion would also pass by
	# accident).
	rest="$(printf 'a%.0s' $(seq 1 100))"
	tmux setw -t "$WIN" @window_label_id '#858 '
	tmux setw -t "$WIN" @window_label_rest_long "$rest"
	tmux setw -t "$WIN" @window_label_clipped 1
	tmux setw -t "$WIN" @crew_name coral
	tmux setw -t "$WIN" @window_has_agent 1
	tmux resize-window -t "$WIN" -x 40 -y 12
	tmux set -g status off
	tmux setw -t "$WIN" pane-border-status top

	tmux -L og-outer -f /dev/null new-session -d -x 40 -y 12 \
		"env -u TMUX tmux -u -L default attach -t S"
	outer_pane="$(tmux -L og-outer list-panes -a -F '#{pane_id}' | head -n1)"

	found=0
	for _ in $(seq 1 50); do
		if tmux -u list-clients 2>/dev/null | grep -q .; then
			found=1
			break
		fi
		sleep 0.1
	done
	if [ "$found" -ne 1 ]; then
		skip "inner client never attached"
	fi
	echo "# drawn-border ran" >&3

	line1="$(tmux -u -L og-outer capture-pane -p -t "$outer_pane" | sed -n '1p')"
	[[ $line1 == *"…"* ]]
	[[ $line1 == *" ━━" ]]
}

# Resolves the Nix placeholders of the real reflow to repo paths, ASCII enrich
# icons (the border assertions stay ASCII), and a sandbox-resolvable shebang.
build_reflow() {
	local repo tdir=$OG_TMUX_DIR
	repo="$(dirname "$BATS_TEST_DIRNAME")"
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' "$repo/scripts/lib-icons.sh" >"$tdir/lib-icons.sh"
	sed \
		-e 's/@providers@/linear github/g' \
		-e 's/@enrich_icon_linear@/L/g' -e 's/@enrich_icon_github@/G/g' \
		-e 's/@enrich_icon_pending@/P/g' -e 's/@enrich_icon_success@/S/g' \
		-e 's/@enrich_icon_failure@/F/g' -e 's/@enrich_icon_merged@/M/g' \
		-e 's/@enrich_icon_closed@/X/g' -e 's/@enrich_icon_conflict@/C/g' \
		"$repo/scripts/lib-enrich.sh" >"$tdir/lib-enrich.sh"
	sed \
		-e "s|@lib_icons@|$tdir/lib-icons.sh|g" \
		-e "s|@lib_enrich@|$tdir/lib-enrich.sh|g" \
		-e "s|@lib_log@|$repo/scripts/lib-log.sh|g" \
		-e "s|@lib_reflow@|$repo/scripts/lib-reflow.sh|g" \
		-e 's|@MAX_ICONS@|5|g' \
		-e "1s|.*|#!$BASH|" \
		"$repo/scripts/tmux-reflow-windows.sh" >"$tdir/reflow.sh"
	chmod +x "$tdir/reflow.sh"
}

# Polls the first line of outer pane $3 until it does (want $2 = 1) or does
# not (want = 0) contain $1.
line1_has() {
	local needle=$1 want=$2 pane=$3 line
	for _ in $(seq 1 50); do
		line="$(tmux -u -L og-outer capture-pane -p -t "$pane" | sed -n '1p')"
		if [[ $line == *"$needle"* ]]; then
			[ "$want" = 1 ] && return 0
		else
			[ "$want" = 0 ] && return 0
		fi
		sleep 0.1
	done
	echo "# line1 (want=$want of $needle): $line" >&3
	return 1
}

@test "drawn border: the title appears and disappears as reflow flips the clipped stamp" {
	build_reflow
	export TMPDIR="$OG_TMUX_DIR"
	local v
	for v in thm_subtext_0 thm_fg thm_overlay_0; do
		tmux set -g "@$v" '#000000'
	done
	local title="w001-w002-w003-w004-w005-w006-w007-w008-w009-w010-w011-w012-"
	local head="w001-w002-w003-w004-"
	tmux setw -t "$WIN" @branch feat/885-x
	tmux setw -t "$WIN" @issue_branch feat/885-x
	tmux setw -t "$WIN" @issue_provider github
	tmux setw -t "$WIN" @issue_id '#885'
	tmux setw -t "$WIN" @issue_title "$title"
	tmux setw -t "$WIN" pane-border-status top

	tmux -L og-outer -f /dev/null new-session -d -x 100 -y 12 \
		"env -u TMUX tmux -u -L default attach -t S"
	local outer_pane
	outer_pane="$(tmux -L og-outer list-panes -a -F '#{pane_id}' | head -n1)"
	local found=0
	for _ in $(seq 1 50); do
		if tmux -u list-clients 2>/dev/null | grep -q .; then
			found=1
			break
		fi
		sleep 0.1
	done
	if [ "$found" -ne 1 ]; then
		skip "inner client never attached"
	fi

	# Fits whole (88 cells of label in a 95-cell row): plain border.
	tmux resize-window -t "$WIN" -x 100 -y 12
	bash "$OG_TMUX_DIR/reflow.sh" S 100 --force >/dev/null 2>&1
	[ -z "$(tmux -u show -wv -t "$WIN" @window_label_clipped)" ]
	line1_has "$head" 0 "$outer_pane"

	# Narrower than the label: the option flips to 1 after the resize redrew
	# the border, so only the flip itself can put the title there.
	tmux resize-window -t "$WIN" -x 50 -y 12
	bash "$OG_TMUX_DIR/reflow.sh" S 50 --force >/dev/null 2>&1
	[ "$(tmux -u show -wv -t "$WIN" @window_label_clipped)" = 1 ]
	line1_has "$head" 1 "$outer_pane"

	# And back: the unset must clear the title with no other redraw trigger.
	tmux resize-window -t "$WIN" -x 100 -y 12
	bash "$OG_TMUX_DIR/reflow.sh" S 100 --force >/dev/null 2>&1
	[ -z "$(tmux -u show -wv -t "$WIN" @window_label_clipped)" ]
	line1_has "$head" 0 "$outer_pane"
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
