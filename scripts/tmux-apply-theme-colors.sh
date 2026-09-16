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

# Bail if catppuccin hasn't loaded yet
[[ -z $thm_mauve || -z $thm_bg ]] && exit 0

# --- Pane borders ---
# Nested #{@thm_*} inside #[] don't expand at render time, so we interpolate here.
# @pane_label (floating utility panes) → mauve titled border; then the bridged
# dispatcher decorations (#640) — a role pane's own role/state, else the mirror
# window's crew codename; else multi-pane ● / plain. Neither bridge branch sets
# an fg: the crew colour reaches them through pane-border-style, as on the remote.
# Each #[...] block carries at most one style attribute (bg OR fg), never a
# comma-joined pair: pane-border-format's #{?...} ternary comma-splitter
# (format_choose/format_skip1) tracks nesting depth for #{...} but not #[...],
# so a joined #[bg=X,fg=Y] is silently misparsed as an extra branch boundary —
# both the @pane_label-inactive branch and the true-default branch below were
# rendering empty because of this before the split (#648).
tmux setw -g pane-border-format \
	"#{?@pane_label,#{?pane_active,#[fg=${thm_mauve}]━━ #{@pane_label} ━━,#[bg=${thm_bg}]#[fg=${thm_overlay_1}]━━ #{@pane_label} ━━},#{?@bridge_crew_role,━━ #[bold]#{@bridge_crew_role}#[nobold] #{@bridge_crew_state} ━━,#{?@bridge_crew_name,━━ #[bold]#{@bridge_crew_name}#[nobold] ━━,#{?#{&&:#{pane_active},#{&&:#{>:#{window_panes},1},#{==:#{window_zoomed_flag},0}}},#[fg=${thm_mauve}]━━ #[fg=${thm_green}]●#[fg=${thm_mauve}] ━━,#[bg=${thm_bg}]#[fg=${thm_overlay_1}]━━━━━}}}}"

# --- tmux-fingers hints (requires colourN format, not hex) ---
tmux set -g @fingers-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_mauve"),bold"
tmux set -g @fingers-highlight-style "fg=colour$(hex_to_256 "$thm_yellow"),bg=colour$(hex_to_256 "$thm_surface_1")"
tmux set -g @fingers-selected-hint-style "fg=colour$(hex_to_256 "$thm_crust"),bg=colour$(hex_to_256 "$thm_green"),bold"
tmux set -g @fingers-selected-highlight-style "fg=colour$(hex_to_256 "$thm_teal"),bg=colour$(hex_to_256 "$thm_surface_1")"
