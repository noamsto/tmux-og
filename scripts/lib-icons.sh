#!/usr/bin/env bash
# Shared icon utilities for tmux scripts.
# Sourced (not executed) — provides constants and functions.
# Nix build time substitution provides ICON_MAP and FALLBACK_ICON.

# shellcheck disable=SC2034,SC2190  # REPLY_DW used by callers; icon map entries are Nix-generated
declare -A ICON_MAP=(
	@ICON_MAP@
)
FALLBACK_ICON="@FALLBACK_ICON@"

# _icon_cell_width CHAR
# Determines display width of a single icon character to match tmux's measurement.
# Sets _ICW to integer width (avoids clobbering REPLY used by callers).
#
# Rules (mirrors picker/main.go runeCellWidth, which delegates to go-runewidth):
# - Nerd Font PUA → 1 cell
# - Supplementary-plane emoji (U+1F000+) → 2 cells
# - BMP characters with Emoji_Presentation=Yes (Unicode TR51) → 2 cells
# - Everything else → 1 cell
#
# Without the BMP list, symbols like ⚙ (U+2699), ❄ (U+2744), ↯ (U+21AF) would be
# miscounted as 2 cells, breaking icon-column padding for windows whose process
# icons fall outside the supplementary-plane emoji range.
_icon_cell_width() {
	local -i cp
	printf -v cp '%d' "'${1:0:1}"
	if ((cp == 0)); then
		_ICW=2 # unrecognized → assume wide
	elif ((cp >= 0xE000 && cp <= 0xF8FF)) || ((cp >= 0xF0000)); then
		_ICW=1 # Nerd Font PUA
	elif ((cp >= 0x1F000)); then
		_ICW=2 # supplementary-plane emoji
	elif ((cp >= 0x231A && cp <= 0x231B)) ||
		((cp >= 0x23E9 && cp <= 0x23EC)) ||
		((cp == 0x23F0 || cp == 0x23F3)) ||
		((cp >= 0x25FD && cp <= 0x25FE)) ||
		((cp >= 0x2614 && cp <= 0x2615)) ||
		((cp >= 0x2648 && cp <= 0x2653)) ||
		((cp == 0x267F || cp == 0x2693 || cp == 0x26A1)) ||
		((cp >= 0x26AA && cp <= 0x26AB)) ||
		((cp >= 0x26BD && cp <= 0x26BE)) ||
		((cp >= 0x26C4 && cp <= 0x26C5)) ||
		((cp == 0x26CE || cp == 0x26D4 || cp == 0x26EA)) ||
		((cp >= 0x26F2 && cp <= 0x26F3)) ||
		((cp == 0x26F5 || cp == 0x26FA || cp == 0x26FD)) ||
		((cp == 0x2705)) ||
		((cp >= 0x270A && cp <= 0x270B)) ||
		((cp == 0x2728 || cp == 0x274C || cp == 0x274E)) ||
		((cp >= 0x2753 && cp <= 0x2755)) ||
		((cp == 0x2757)) ||
		((cp >= 0x2795 && cp <= 0x2797)) ||
		((cp == 0x27B0 || cp == 0x27BF)) ||
		((cp >= 0x2B1B && cp <= 0x2B1C)) ||
		((cp == 0x2B50 || cp == 0x2B55)); then
		_ICW=2 # BMP emoji-presentation defaults
	else
		_ICW=1
	fi
}

# build_proc_icons "proc1 proc2 ..." MAX_COUNT
# Builds space-separated icon string from process names, capped at MAX_COUNT.
# Sets REPLY to the icon string (trailing space per icon).
# Sets REPLY_DW to display width (per-icon width varies: nerd font=1, emoji=2, +1 space each).
build_proc_icons() {
	local procs="$1" max="$2"
	REPLY=""
	REPLY_DW=0
	local count=0
	local -A written=()
	# shellcheck disable=SC2086  # intentional word splitting
	for proc in $procs; do
		((count >= max)) && break
		# nix makeWrapper exposes the real binary as `.foo-wrapped` (what
		# pane_current_command reports on macOS); map it back for the lookup.
		[[ $proc == .*-wrapped ]] && proc="${proc#.}" && proc="${proc%-wrapped}"
		local icon="${ICON_MAP[$proc]:-$FALLBACK_ICON}"
		[[ -z $icon || -n ${written[$icon]} ]] && continue
		written[$icon]=1
		REPLY+="$icon "
		_icon_cell_width "$icon"
		((REPLY_DW += _ICW + 1)) # icon width + 1 space
		((count++)) || true
	done
}

# pad_to_width STRING CURRENT_WIDTH TARGET_WIDTH
# Pads STRING with spaces to reach TARGET_WIDTH.
# Sets REPLY to padded string.
pad_to_width() {
	local str="$1" current="$2" target="$3"
	local pad_needed=$((target - current))
	((pad_needed < 0)) && pad_needed=0
	local pad=""
	printf -v pad '%*s' "$pad_needed" ''
	REPLY="${str}${pad}"
}

# measure_display_width STRING
# Computes the terminal display width of STRING: ASCII = 1 cell, non-ASCII via
# _icon_cell_width (matches tmux's measurement of nerd/emoji glyphs).
# Sets REPLY_DW to the integer width.
# Requires a UTF-8 locale so per-char indexing classifies multibyte codepoints.
measure_display_width() {
	local str="$1" ch
	local -i i cp w=0
	for ((i = 0; i < ${#str}; i++)); do
		ch="${str:i:1}"
		printf -v cp '%d' "'$ch"
		if ((cp < 128)); then
			((w += 1))
		else
			_icon_cell_width "$ch"
			((w += _ICW))
		fi
	done
	REPLY_DW=$w
}

# truncate_to_width STRING MAXW
# Truncates STRING to at most MAXW display cells, appending "…" (1 cell) when it
# had to cut. Glyph-aware (uses _icon_cell_width). Sets REPLY.
# Requires a UTF-8 locale (same as measure_display_width).
truncate_to_width() {
	local str="$1" ch out=""
	local -i i cp w=0 cw maxw="$2"
	measure_display_width "$str"
	if ((REPLY_DW <= maxw)); then
		REPLY="$str"
		return
	fi
	# Reserve one cell for the ellipsis.
	for ((i = 0; i < ${#str}; i++)); do
		ch="${str:i:1}"
		printf -v cp '%d' "'$ch"
		if ((cp < 128)); then
			cw=1
		else
			_icon_cell_width "$ch"
			cw=$_ICW
		fi
		((w + cw > maxw - 1)) && break
		out+="$ch"
		((w += cw))
	done
	REPLY="${out}…"
}
