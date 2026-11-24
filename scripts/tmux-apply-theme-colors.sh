#!/usr/bin/env bash
# Apply theme-dependent colors after catppuccin loads
# Handles: pane borders, tmux-fingers hints
# Runs at config load and on theme-toggle (via config re-source)

# Convert #rrggbb hex to closest xterm-256 colour index.
# tmux-fingers doesn't support hex colors, only "colourN" format.
hex_to_256() {
	local hex="${1#\#}"
	local r=$((16#${hex:0:2})) g=$((16#${hex:2:2})) b=$((16#${hex:4:2}))

	# 6x6x6 color cube (indices 16-231)
	local ri=$(((r > 47) ? (r - 35) / 40 : 0))
	local gi=$(((g > 47) ? (g - 35) / 40 : 0))
	local bi=$(((b > 47) ? (b - 35) / 40 : 0))
	local cube_idx=$((16 + 36 * ri + 6 * gi + bi))
	# Reconstruct the cube color's actual RGB for distance check
	local cube_r=$((ri ? 55 + ri * 40 : 0))
	local cube_g=$((gi ? 55 + gi * 40 : 0))
	local cube_b=$((bi ? 55 + bi * 40 : 0))
	local cube_dist=$(((r - cube_r) ** 2 + (g - cube_g) ** 2 + (b - cube_b) ** 2))

	# Greyscale ramp (232-255): 24 shades from #080808 to #eeeeee
	local avg=$(((r + g + b) / 3))
	local grey_idx=$(((avg - 8) * 24 / 247 + 232))
	((grey_idx < 232)) && grey_idx=232
	((grey_idx > 255)) && grey_idx=255
	local grey_val=$((8 + (grey_idx - 232) * 10))
	local grey_dist=$(((r - grey_val) ** 2 + (g - grey_val) ** 2 + (b - grey_val) ** 2))

	# Pick whichever is closer
	if ((grey_dist < cube_dist)); then
		echo "$grey_idx"
	else
		echo "$cube_idx"
	fi
}

# Read catppuccin color variables
thm_crust=$(tmux show -gv @thm_crust 2>/dev/null | tr -d '"')
thm_bg=$(tmux show -gv @thm_bg 2>/dev/null | tr -d '"')
thm_overlay_1=$(tmux show -gv @thm_overlay_1 2>/dev/null | tr -d '"')
thm_mauve=$(tmux show -gv @thm_mauve 2>/dev/null | tr -d '"')
thm_green=$(tmux show -gv @thm_green 2>/dev/null | tr -d '"')
thm_teal=$(tmux show -gv @thm_teal 2>/dev/null | tr -d '"')
thm_blue=$(tmux show -gv @thm_blue 2>/dev/null | tr -d '"')
thm_surface_1=$(tmux show -gv @thm_surface_1 2>/dev/null | tr -d '"')
thm_peach=$(tmux show -gv @thm_peach 2>/dev/null | tr -d '"')
thm_red=$(tmux show -gv @thm_red 2>/dev/null | tr -d '"')

# Bail if catppuccin hasn't loaded yet
[[ -z $thm_mauve || -z $thm_bg ]] && exit 0

# --- Status-bar roles ---
# picker_generate owns the role table (palette.StatusRoles): per flavor each role
# is a @thm_* option or a literal hex. Stamp @og_c_<name> so every status format
# reads one option that follows the flavor. Skipped for a raw-script test copy
# (placeholder unsubstituted). Otherwise the old stamps are cleared first, so an
# empty or failed call leaves them unset, never stale from the previous flavor.
gen="@picker_generate@"
if [[ $gen != @* ]]; then
	flavor=$(tmux show -gv @catppuccin_flavor 2>/dev/null | tr -d '"')
	while read -r opt _; do
		[[ $opt == @og_c_* ]] && tmux set -gu "$opt"
	done < <(tmux show -g 2>/dev/null)
	roles=$("$gen" status-roles "$flavor" 2>/dev/null || true)
	while read -r name value; do
		[[ $name =~ ^[a-z_]+$ ]] || continue
		if [[ $value =~ ^@thm_[a-z0-9_]+$ ]]; then
			tmux set -gF "@og_c_$name" "#{$value}"
		elif [[ $value =~ ^#[0-9a-fA-F]{6}$ ]]; then
			tmux set -g "@og_c_$name" "$value"
		fi
	done <<<"$roles"
fi
# pane-border-style cannot hold a nested format, so read the stamped value back.
og_dim=$(tmux show -gv @og_c_dim 2>/dev/null | tr -d '"')
[[ -n $og_dim ]] || og_dim=$thm_overlay_1
og_float=$(tmux show -gv @og_c_float 2>/dev/null | tr -d '"')
[[ -n $og_float ]] || og_float=$thm_teal

# --- Pane borders ---
# Nested #{@thm_*} inside #[] don't expand at render time, so we interpolate here.
# Branch order, first match wins: any float (own label and accent) → tiled
# @pane_label label → aeye carousel → role pane →
# the window's anchor (top-left, non-floating) pane, which alone carries the
# full title → multi-pane ● / dim bar. A "lead" role falls through to the
# anchor branch; the codename already names it. Text carries no fg of its own:
# the crew/role colour comes from the border styles below.
# Each #[...] block carries at most one style attribute (bg OR fg): the #{?...}
# comma-splitter (format_choose/format_skip1) tracks nesting depth for #{...}
# but not #[...], so a joined #[bg=X,fg=Y] is misparsed as an extra branch
# boundary (#648).

# ROLE_NAME/STATE share one bridge-vs-local selector so a bridged pane's role
# and state always come from the same side. STATE_GLYPH mirrors the
# dispatcher's state_glyph vocabulary/colours; STATE_GLYPH_PLAIN is the same
# glyph with no #[...] directive, so its width measures correctly below.
ROLE_NAME='#{?@bridge_crew_role,#{@bridge_crew_role},#{@crew_role}}'
STATE='#{?@bridge_crew_role,#{@bridge_crew_state},#{@crew_state}}'
STATE_GLYPH="#{?#{==:${STATE},working},#[fg=${thm_green}]●#[default],#{?#{==:${STATE},idle},#[fg=${og_dim}]○#[default],#{?#{==:${STATE},blocked},#[fg=${thm_peach}]⚠#[default],#{?#{||:#{==:${STATE},done},#{==:${STATE},pr_open}},#[fg=${thm_green}]✓#[default],#{?#{||:#{==:${STATE},failed},#{==:${STATE},exited}},#[fg=${thm_red}]✗#[default],#{?${STATE},#[fg=${og_dim}]○#[default],}}}}}}"
STATE_GLYPH_PLAIN="#{?#{==:${STATE},working},●,#{?#{==:${STATE},idle},○,#{?#{==:${STATE},blocked},⚠,#{?#{||:#{==:${STATE},done},#{==:${STATE},pr_open}},✓,#{?#{||:#{==:${STATE},failed},#{==:${STATE},exited}},✗,#{?${STATE},○,}}}}}}"

# aeye's own image-carousel pane option, the bridge-shipped marker for a
# viewer mirrored from a remote, or (a remux-relaunched viewer) its start
# command — the carousel restore command isn't bin/aeye.
AEYE='#{||:#{||:#{@claude_img_src},#{@bridge_img_src}},#{m:*/bin/aeye *,#{pane_start_command}}}'

# Anchor: the top-left, non-floating pane. A zoomed pane is at-top/at-left too,
# so it still shows the title; hidden panes in a grid are simply never drawn.
ANCHOR='#{&&:#{&&:#{pane_at_top},#{pane_at_left}},#{!:#{pane_floating_flag}}}'
TITLE_RAW='#{@window_label_id}#{@window_label_rest_long}'
# The status label already spells the title out; the border repeats it only
# when reflow stamped the label as clipped.
TITLE_SHOWN="#{?@window_label_clipped,${TITLE_RAW},}"
CODENAME='#{?@bridge_win,#{@bridge_crew_name},#{?@window_has_agent,#{@crew_name},}}'

WATCHDOG="#{?#{==:#{@crew_source},watchdog}, (watchdog),}"
DETAIL="#{?@crew_detail, · #{@crew_detail},}"
LEAD_STATE="#{?${STATE},${STATE_GLYPH} ${STATE}${WATCHDOG}${DETAIL},}"
LEAD_STATE_PLAIN="#{?${STATE},${STATE_GLYPH_PLAIN} ${STATE}${WATCHDOG}${DETAIL},}"

# Remote host of a mirror window (placed or dedicated). Gated on @bridge_win:
# a dedicated mirror's session-scoped @bridge_host is also visible from the
# session's local windows. HOST_SEG colours it with the per-host colour reflow
# stamps for placed mirrors; HOST_PLAIN is the same text for width measuring.
HOST_PLAIN='#{?@bridge_win,#{?@bridge_host,⇄ #{@bridge_host},},}'
HOST_SEG="#{?${HOST_PLAIN},#{?@window_host_color,#[fg=#{E:@window_host_color}]${HOST_PLAIN}#[default],${HOST_PLAIN}},}"
HOST_W="#{?${HOST_PLAIN},#{e|+|:#{w:${HOST_PLAIN}},3},0}"

# W: how many cells the title gets. The drawn label area is pane_width-2; 7
# more cells go to the "━━ "/" ━━" wrapping and the "…" tmux appends beyond
# the truncation limit, so pane_width-9 — minus the host/codename/state
# segments (each + 3 for their " · " separator) when present. Floored at 1 so
# #{=/N/…:} never sees N=0 (no clip) or negative (keeps the tail) once the
# other segments alone fill the border.
CN_W="#{?${CODENAME},#{e|+|:#{w:${CODENAME}},3},0}"
ST_W="#{?${LEAD_STATE_PLAIN},#{e|+|:#{w:${LEAD_STATE_PLAIN}},3},0}"
W_RAW="#{e|-|:#{e|-|:#{e|-|:#{e|-|:#{pane_width},9},${HOST_W}},${CN_W}},${ST_W}}"
W="#{?#{e|<|:${W_RAW},1},1,${W_RAW}}"

# Segments join with " · ", each present only when non-empty.
NAME_SEG="${HOST_SEG}#{?${CODENAME},#{?${HOST_PLAIN}, · ,}#[bold]${CODENAME}#[nobold],}"
STATE_SEG="#{?${LEAD_STATE},#{?#{||:${HOST_PLAIN},${CODENAME}}, · ,}${LEAD_STATE},}"
TITLE_SEG="#{?${TITLE_SHOWN},#{?#{||:#{||:${HOST_PLAIN},${CODENAME}},${LEAD_STATE}}, · ,}#{=/${W}/…:${TITLE_SHOWN}},}"

FLOAT_LABEL="#{?@pane_label,#{@pane_label},#{?${AEYE},aeye,#{?@bridge_proc,#{@bridge_proc},#{pane_current_command}}}}"
FLOAT_BRANCH="#{?pane_active,#[fg=${og_float}]━━ ${FLOAT_LABEL} ━━,#[bg=${thm_bg}]#[fg=${og_dim}]━━ ${FLOAT_LABEL} ━━}"
LABEL_BRANCH="#{?pane_active,#[fg=${thm_mauve}]━━ #{@pane_label} ━━,#[bg=${thm_bg}]#[fg=${og_dim}]━━ #{@pane_label} ━━}"
AEYE_BRANCH="#{?pane_active,#[fg=${thm_mauve}]━━ aeye ━━,#[bg=${thm_bg}]#[fg=${og_dim}]━━ aeye ━━}"
ROLE_BRANCH="━━ #[bold]${ROLE_NAME}#[nobold]#{?${STATE}, ${STATE_GLYPH} ${STATE},} ━━"
ANCHOR_BRANCH="━━ ${NAME_SEG}${STATE_SEG}${TITLE_SEG} ━━"
# A sole or zoomed active pane gets no label, so its border stays one unbroken
# line in the active colour; grey dashes there would break that line.
PLAIN_BRANCH="#{?pane_active,#{?#{&&:#{>:#{window_panes},1},#{==:#{window_zoomed_flag},0}},#[fg=${thm_mauve}]━━ #[fg=${thm_green}]●#[fg=${thm_mauve}] ━━,},#[bg=${thm_bg}]#[fg=${og_dim}]━━━━━}"

ROLE_COND="#{&&:#{!=:${ROLE_NAME},},#{!=:${ROLE_NAME},lead}}"
ANCHOR_COND="#{&&:${ANCHOR},#{||:#{!=:${TITLE_SHOWN},},#{||:#{!=:${HOST_PLAIN},},#{||:#{!=:${CODENAME},},#{&&:#{!=:${TITLE_RAW},},#{!=:${LEAD_STATE},}}}}}}"

tmux setw -g pane-border-format \
	"#{?pane_floating_flag,${FLOAT_BRANCH},#{?@pane_label,${LABEL_BRANCH},#{?${AEYE},${AEYE_BRANCH},#{?${ROLE_COND},${ROLE_BRANCH},#{?${ANCHOR_COND},${ANCHOR_BRANCH},${PLAIN_BRANCH}}}}}}"

# Border colour: aeye never takes a crew colour; everything else falls through
# role colour (bridged, then local) → window agent colour (bridged, then local
# while occupied, the status bar's #671 gate) → mauve/overlay_1.
ROLE_COLOR_CHAIN_MAUVE="#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@crew_role_color,#{@crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{?@window_has_agent,#{?@crew_color,#{@crew_color},${thm_mauve}},${thm_mauve}}}}}"
ROLE_COLOR_CHAIN_OVERLAY="#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@crew_role_color,#{@crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{?@window_has_agent,#{?@crew_color,#{@crew_color},${og_dim}},${og_dim}}}}}"

# Floats carry their own accent ahead of every crew arm. The float-stamp hook
# points each float's pane-level options at these globals by indirection, so
# re-running this script (theme switch) refreshes them.
tmux set -g @og_float_fmt "${FLOAT_BRANCH}"
tmux set -g @og_float_astyle "bg=${thm_bg},fg=${og_float}"
tmux set -g @og_float_style "bg=${thm_bg},fg=${og_dim}"
tmux setw -g pane-active-border-style \
	"bg=${thm_bg},fg=#{?pane_floating_flag,${og_float},#{?${AEYE},${thm_mauve},${ROLE_COLOR_CHAIN_MAUVE}}}"
tmux setw -g pane-border-style \
	"bg=${thm_bg},fg=#{?pane_floating_flag,${og_dim},#{?${AEYE},${og_dim},${ROLE_COLOR_CHAIN_OVERLAY}}}"

# --- tmux-fingers hints (requires colourN format, not hex) ---
tmux set -g @fingers-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_mauve"),bold"
# Blue, no bg: a per-row bg merges every match into one uniform block, and
# blue quantizes to ~5:1 contrast on latte and mocha alike (latte yellow is
# ~2.5:1).
tmux set -g @fingers-highlight-style "fg=colour$(hex_to_256 "$thm_blue")"
tmux set -g @fingers-selected-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_green"),bold"
tmux set -g @fingers-selected-highlight-style "fg=colour$(hex_to_256 "$thm_teal"),bg=colour$(hex_to_256 "$thm_surface_1")"
