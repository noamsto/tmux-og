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
thm_yellow=$(tmux show -gv @thm_yellow 2>/dev/null | tr -d '"')
thm_surface_1=$(tmux show -gv @thm_surface_1 2>/dev/null | tr -d '"')
thm_peach=$(tmux show -gv @thm_peach 2>/dev/null | tr -d '"')
thm_red=$(tmux show -gv @thm_red 2>/dev/null | tr -d '"')

# Bail if catppuccin hasn't loaded yet
[[ -z $thm_mauve || -z $thm_bg ]] && exit 0

# --- Pane borders (#858) ---
# Nested #{@thm_*} inside #[] don't expand at render time, so we interpolate here.
# Branch order, first match wins: float label (@pane_label) → aeye carousel →
# a role pane's own role/state (dispatcher #640, or local @crew_role) → the
# window's anchor pane (top-left, non-floating), which gets the full window
# title → plain multi-pane ● / dim bar. A lead pane (@crew_role/@bridge_crew_role
# "lead") falls through the role branch into the anchor branch, since the word
# "lead" is never shown — the anchor's codename already names it.
# No branch here sets an fg on the text itself: the crew/role colour reaches
# the border through pane-active-border-style/pane-border-style below, as on
# the remote. Each #[...] block carries at most one style attribute (bg OR
# fg), never a comma-joined pair: pane-border-format's #{?...} ternary
# comma-splitter (format_choose/format_skip1) tracks nesting depth for
# #{...} but not #[...], so a joined #[bg=X,fg=Y] is silently misparsed as an
# extra branch boundary (#648).

# ROLE_NAME/STATE share one bridge-vs-local selector so a bridged pane's role
# and state always come from the same side. STATE_GLYPH mirrors the
# dispatcher's state_glyph vocabulary/colours; STATE_GLYPH_PLAIN is the same
# glyph with no #[...] directive, so its width measures correctly below.
ROLE_NAME='#{?@bridge_crew_role,#{@bridge_crew_role},#{@crew_role}}'
STATE='#{?@bridge_crew_role,#{@bridge_crew_state},#{@crew_state}}'
STATE_GLYPH="#{?#{==:${STATE},working},#[fg=${thm_green}]●#[default],#{?#{==:${STATE},idle},#[fg=${thm_overlay_1}]○#[default],#{?#{==:${STATE},blocked},#[fg=${thm_peach}]⚠#[default],#{?#{||:#{==:${STATE},done},#{==:${STATE},pr_open}},#[fg=${thm_green}]✓#[default],#{?#{||:#{==:${STATE},failed},#{==:${STATE},exited}},#[fg=${thm_red}]✗#[default],#{?${STATE},#[fg=${thm_overlay_1}]○#[default],}}}}}}"
STATE_GLYPH_PLAIN="#{?#{==:${STATE},working},●,#{?#{==:${STATE},idle},○,#{?#{==:${STATE},blocked},⚠,#{?#{||:#{==:${STATE},done},#{==:${STATE},pr_open}},✓,#{?#{||:#{==:${STATE},failed},#{==:${STATE},exited}},✗,#{?${STATE},○,}}}}}}"

# aeye's own image-carousel pane option, or (a remux-relaunched viewer) its
# start command — the carousel restore command isn't bin/aeye.
AEYE='#{||:#{@claude_img_src},#{m:*/bin/aeye *,#{pane_start_command}}}'

# Anchor: the top-left, non-floating pane. A zoomed pane is at-top/at-left too,
# so it still shows the title; hidden panes in a grid are simply never drawn.
ANCHOR='#{&&:#{&&:#{pane_at_top},#{pane_at_left}},#{!:#{pane_floating_flag}}}'
TITLE_RAW='#{@window_label_id}#{@window_label_rest_long}'
CODENAME='#{?@bridge_win,#{@bridge_crew_name},#{?@window_has_agent,#{@crew_name},}}'

WATCHDOG="#{?#{==:#{@crew_source},watchdog}, (watchdog),}"
DETAIL="#{?@crew_detail, · #{@crew_detail},}"
LEAD_STATE="#{?${STATE},${STATE_GLYPH} ${STATE}${WATCHDOG}${DETAIL},}"
LEAD_STATE_PLAIN="#{?${STATE},${STATE_GLYPH_PLAIN} ${STATE}${WATCHDOG}${DETAIL},}"

# W: how many cells the title gets. The drawn label area is pane_width-2; 7
# more cells go to the "━━ "/" ━━" wrapping and the "…" tmux appends beyond
# the truncation limit, so pane_width-9 — minus the codename/state segments
# (each + 3 for their " · " separator) when present. Floored at 1 so
# #{=/N/…:} never sees N=0 (no clip) or negative (keeps the tail) once the
# other segments alone fill the border.
CN_W="#{?${CODENAME},#{e|+|:#{w:${CODENAME}},3},0}"
ST_W="#{?${LEAD_STATE_PLAIN},#{e|+|:#{w:${LEAD_STATE_PLAIN}},3},0}"
W_RAW="#{e|-|:#{e|-|:#{e|-|:#{pane_width},9},${CN_W}},${ST_W}}"
W="#{?#{e|<|:${W_RAW},1},1,${W_RAW}}"

# Segments join with " · ", each present only when non-empty.
NAME_SEG="#{?${CODENAME},#[bold]${CODENAME}#[nobold],}"
STATE_SEG="#{?${LEAD_STATE},#{?${CODENAME}, · ,}${LEAD_STATE},}"
TITLE_SEG="#{?${TITLE_RAW},#{?#{||:${CODENAME},${LEAD_STATE}}, · ,}#{=/${W}/…:${TITLE_RAW}},}"

FLOAT_BRANCH="#{?pane_active,#[fg=${thm_mauve}]━━ #{@pane_label} ━━,#[bg=${thm_bg}]#[fg=${thm_overlay_1}]━━ #{@pane_label} ━━}"
AEYE_BRANCH="#{?pane_active,#[fg=${thm_mauve}]━━ aeye ━━,#[bg=${thm_bg}]#[fg=${thm_overlay_1}]━━ aeye ━━}"
ROLE_BRANCH="━━ #[bold]${ROLE_NAME}#[nobold]#{?${STATE}, ${STATE_GLYPH} ${STATE},} ━━"
ANCHOR_BRANCH="━━ ${NAME_SEG}${STATE_SEG}${TITLE_SEG} ━━"
PLAIN_BRANCH="#{?#{&&:#{pane_active},#{&&:#{>:#{window_panes},1},#{==:#{window_zoomed_flag},0}}},#[fg=${thm_mauve}]━━ #[fg=${thm_green}]●#[fg=${thm_mauve}] ━━,#[bg=${thm_bg}]#[fg=${thm_overlay_1}]━━━━━}"

ROLE_COND="#{&&:#{!=:${ROLE_NAME},},#{!=:${ROLE_NAME},lead}}"
ANCHOR_COND="#{&&:${ANCHOR},#{||:#{!=:${TITLE_RAW},},#{!=:${CODENAME},}}}"

tmux setw -g pane-border-format \
	"#{?@pane_label,${FLOAT_BRANCH},#{?${AEYE},${AEYE_BRANCH},#{?${ROLE_COND},${ROLE_BRANCH},#{?${ANCHOR_COND},${ANCHOR_BRANCH},${PLAIN_BRANCH}}}}}"

# Border colour (moved in from config/tmux.conf.tmpl so this bats file can
# evaluate the real shipped strings): aeye forces mauve/overlay_1 regardless
# of crew colour; everything else falls through role colour (bridged, then
# local) → window agent colour (bridged, then local when occupied) → the
# same mauve/overlay_1 default. Mirror behaviour is unchanged.
ROLE_COLOR_CHAIN_MAUVE="#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@crew_role_color,#{@crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{?@window_has_agent,#{?@crew_color,#{@crew_color},${thm_mauve}},${thm_mauve}}}}}"
ROLE_COLOR_CHAIN_OVERLAY="#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@crew_role_color,#{@crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{?@window_has_agent,#{?@crew_color,#{@crew_color},${thm_overlay_1}},${thm_overlay_1}}}}}"
tmux setw -g pane-active-border-style \
	"bg=${thm_bg},fg=#{?${AEYE},${thm_mauve},${ROLE_COLOR_CHAIN_MAUVE}}"
tmux setw -g pane-border-style \
	"bg=${thm_bg},fg=#{?${AEYE},${thm_overlay_1},${ROLE_COLOR_CHAIN_OVERLAY}}"

# --- tmux-fingers hints (requires colourN format, not hex) ---
tmux set -g @fingers-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_mauve"),bold"
tmux set -g @fingers-highlight-style "fg=colour$(hex_to_256 "$thm_yellow"),bg=colour$(hex_to_256 "$thm_surface_1")"
tmux set -g @fingers-selected-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_green"),bold"
tmux set -g @fingers-selected-highlight-style "fg=colour$(hex_to_256 "$thm_teal"),bg=colour$(hex_to_256 "$thm_surface_1")"
