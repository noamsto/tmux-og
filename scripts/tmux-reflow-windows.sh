#!/usr/bin/env bash
# tmux-reflow-windows: Compute window layout and set status-format lines
# Handles split points, dynamic padding, icon caching, and status line count (2-4).
# Called by hooks on window add/remove/resize, NOT every status-interval.
#
# Key design: icon and text are separated for alignment.
# Icons have variable char-to-display-width ratios,
# so padding only the text portion (ASCII branch/dir names) gives
# consistent column alignment regardless of icon encoding.

# shellcheck source=/dev/null  # Nix store path substituted at build time
source @lib_icons@
# shellcheck source=/dev/null
source @lib_enrich@
# shellcheck source=/dev/null
source @lib_log@
# shellcheck source=/dev/null
source @lib_reflow@

# Accept --force (cache bypass, used by enrich scripts after writing vars) and
# --debounce (coalesce a resize burst, see below).
FORCE=0
DEBOUNCE=0
AWAIT_LOCK=0
pos=()
for a in "$@"; do
	case "$a" in
	--force) FORCE=1 ;;
	--debounce) DEBOUNCE=1 ;;
	--await-lock) AWAIT_LOCK=1 ;;
	*) pos+=("$a") ;;
	esac
done
set -- "${pos[@]}"

# Accept session/width as args (from hooks) or fall back to display-message.
# The width fallback is targeted at SESSION, not the caller's current client:
# the bridge daemon forces a reflow of its mirror session from outside any
# client, where an untargeted #{client_width} expands to nothing.
SESSION=${1:-$(tmux display-message -p '#{session_name}')}
WIDTH=${2:-$(tmux display-message -t "$SESSION" -p '#{client_width}')}
MAX_ICONS=@MAX_ICONS@

# Scratch sessions manage their own status bar (hints bar); skip reflow.
case "$SESSION" in
scratch-*) exit 0 ;;
esac

# Empty/non-numeric width: no attached or size-neutral client expanded
# #{client_width} to nothing. Stamping "N:" poisons the cache and lets a
# later real reflow look like a no-op hit (issue #235).
if [[ ! $WIDTH =~ ^[1-9][0-9]*$ ]]; then
	log_enabled && log_event reflow event skip_empty_width width "$WIDTH" sess "$SESSION"
	exit 0
fi

# Debounce a resize burst: client-resized fires once per drag step, and every
# distinct width misses the cache below → a full O(N) recompute each time. The
# hook backgrounds this (-b), so the sleep is off the server's command queue.
# Each invocation stamps a token and waits out the burst; only the last one to
# stamp (the final width) survives the token check and reflows — the rest bail.
if ((DEBOUNCE)); then
	stamp="/tmp/og-reflow-debounce.${SESSION//\//_}"
	token=$EPOCHREALTIME
	printf '%s' "$token" >"$stamp" 2>/dev/null
	sleep 0.12
	last=""
	[[ -f $stamp ]] && last=$(<"$stamp")
	[[ $last == "$token" ]] || exit 0
fi

# Fast-path: skip if window count + size unchanged since last reflow. Layout
# (detail mode + column widths + row cap) depends only on the window set and the
# client size, not on which window is active — focus only changes the active
# tab's color, which tmux re-renders on its own without a reflow.
# One display-message fetches the window count, the stored key and the height
# (the fast path runs on every reflow, so keeping its forks down matters).
# '|'-delimited, not newline: tmux rewrites a newline in a format to "_" for any
# client without a UTF-8 locale, exactly as it does a tab (#373) — which
# collapsed all three fields into win_count, so prev_key was always empty (cache
# never hit) and HEIGHT always 0 (the 4th status row could never unlock).
IFS='|' read -r win_count prev_key HEIGHT < <(tmux display-message -t "$SESSION" -p '#{session_windows}|#{@reflow_key}|#{client_height}' 2>/dev/null)
# Height only gates the extra window row, so a size-neutral client (empty
# client_height) falls back to the baseline cap instead of skipping the reflow
# the way an empty width has to. Resolve it before it reaches the cache key so
# the key can never be stamped with a trailing blank field.
[[ $HEIGHT =~ ^[1-9][0-9]*$ ]] || HEIGHT=0
cache_key="${win_count}:${WIDTH}:${HEIGHT}"
if ((! FORCE)) && [[ $cache_key == "$prev_key" ]]; then
	log_enabled && log_event reflow event cache_hit wins "$win_count" width "$WIDTH" height "$HEIGHT" sess "$SESSION"
	exit 0
fi

# Serialize compute+write across concurrent reflows; every read below happens
# inside the lock, so whoever renders last renders the freshest state. Some
# hooks run this synchronously, so the foreground waits ~2s at most — by the
# clock, not a retry count: each failed acquire spawns processes, and macOS
# forks stretched 40 retries past 5s. Never write unlocked: the batched option
# writes and the separate status-format sets would tear against another
# invocation's. When the budget runs out, a detached waiter owes the render and
# waits past the stale window, so a dead holder's lock is stolen; --force so a
# coincidentally matching key can't skip the render it exists to do.
reflow_lock="${TMPDIR:-/tmp}/og-reflow.lock.${SESSION//\//_}"
locked=0
lock_deadline=$((${EPOCHREALTIME/[^0-9]/} + 2000000))
while :; do
	acquire_lock "$reflow_lock" && {
		locked=1
		break
	}
	((${EPOCHREALTIME/[^0-9]/} < lock_deadline)) || break
	sleep 0.05
done
if ((! locked && AWAIT_LOCK)); then
	deadline=$((SECONDS + OG_LOCK_STALE_SECONDS + 5))
	while ((SECONDS < deadline)); do
		acquire_lock "$reflow_lock" && {
			locked=1
			break
		}
		sleep 0.5
	done
fi
if ((! locked)); then
	((AWAIT_LOCK)) || detach "$0" --await-lock --force "$SESSION" "$WIDTH"
	exit 0
fi

PREFIX_WIDTH=5 # " ├─ " or " ╰─ "

# --- Single pass: collect window data ---
declare -a indices
total=0
has_zoom=0

# read drops any extra delimiters into the final field, so nothing after a
# field can shift the columns before it. @window_task is free-form text that
# may contain '|' and is NOT sanitized, so it must be last to stay protected.
# @window_bridge_name is always daemon-sanitized (never contains '|'), so it's
# safe placed just before @window_task.
# @window_ai_name is sanitized (kebab, no '|') so it sits safely before that.
# @crew_name (agent codename, stamped by an external fan-out harness) is
# token-safe (no '|'). Its @crew_color pairs with it but is read straight from the
# window option in the template, so only the name is pulled here (for width).
# @window_has_agent (#671) is a closed "1"/"" token, same shape, so it sits
# right beside @crew_name — it gates the non-bridge crew badge below.
# @bridge_win/window_name sit after it: bridge_win is "1" or empty, and a
# window_name containing '|' is no worse off here than at the very end.
# The four @bridge_* label fields between them are daemon-sanitized (never
# contain '|'). Only these four are pulled here: the seven @bridge_* colour/state
# values are read live by the format fragments below, so naming them would only
# add unused variables.
FMT='#{window_index}|#{@branch}|#{pane_current_path}|#{window_zoomed_flag}|#{@issue_provider}|#{@issue_id}|#{@issue_title}|#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_check_progress}|#{@issue_branch}|#{@crew_name}|#{@window_has_agent}|#{@window_ai_name}|#{@bridge_win}|#{@bridge_label_id}|#{@bridge_label_rest_long}|#{@bridge_pr_plain}|#{@bridge_crew_name}|#{window_name}|#{@window_bridge_name}|#{@window_task}'
declare -A win_short win_short_dw win_long_dw
declare -A win_id win_id_dw win_rest_short win_rest_long win_pr win_pr_dw
declare -A win_crew win_crew_dw win_crew_disp win_zoom_dw
pr_colw=0   # widest PR segment → shared PR column width (0 when no window has a PR)
crew_colw=0 # widest codename → shared agent-badge column (0 when no window is tagged)
while IFS='|' read -r idx branch pane_path zoomed iprov iid ititle prnum prstate prcheck prmerge prdraft prprog ibranch crew hasagent wai bridge bid brest bpr bcrew wname bname wtask; do
	indices+=("$idx")
	# The zoom marker (" 󰁌", 2 cells) is emitted inline by LABEL_Z on zoomed
	# windows; carve it from that window's label budget so its grid slot stays
	# colw wide (mirrors the crew badge). has_zoom reserves the same 2 cells in
	# the single-line fit test, where the marker is appended to the full label.
	win_zoom_dw[$idx]=0
	((zoomed)) && has_zoom=1 && win_zoom_dw[$idx]=2

	# Remote-bridge mirror window (#167 @bridge_win opt-out): its identity comes
	# from the daemon's @bridge_* copies of the remote window's own label state.
	# Local enrichment is skipped entirely — the issue/PR/branch context here
	# belongs to the launcher's repo, not the remote window this mirrors.
	collapse=0
	if [[ $bridge == 1 ]]; then
		win_id[$idx]="$bid"
		win_rest_long[$idx]="$brest"
		win_pr[$idx]="$bpr"
		crew="$bcrew"
		# Short mode drops the remainder for an id-bearing window, matching
		# build_window_label (which sets REPLY_REST only in its long arm).
		# Without that, total_short == total_long and a mirror-heavy narrow
		# session goes multiline earlier than the equivalent local one.
		win_rest_short[$idx]=""
		[[ -n $bid ]] || win_rest_short[$idx]="$brest"
		# Before the daemon's first label write, fall back to the daemon-owned
		# remote name (@window_bridge_name, NOT #{window_name} — the latter is
		# clobbered by automatic-rename on the real config, #196). Only when the
		# bridge carries neither id nor rest: an empty id alone still leaves the
		# rest as the identity of a remote branch with no detected issue.
		if [[ -z $bid && -z $brest ]]; then
			bwname="${bname:-$wname}"
			win_rest_short[$idx]="$bwname"
			win_rest_long[$idx]="$bwname"
			collapse=1
		fi
	else
		# Stamp belongs to the branch it was written for. If the pane has since
		# cd'd to a different branch, build the label from the current branch
		# instead — the stamp stays on the window and reappears on cd back. An
		# unset @issue_branch is no such evidence: tmux-issue-stamp always writes
		# the two together, so an id without one is hand-set or predates the
		# field, and dropping it would be permanent (the backfill needs a branch
		# to even consider a window).
		if [[ -n $iid && -n $ibranch && $ibranch != "$branch" ]]; then
			iprov="" iid="" ititle=""
			prnum="" prstate="" prcheck="" prmerge="" prdraft="" prprog=""
		fi

		build_window_label short "$iprov" "$iid" "$ititle" "$prnum" "$prstate" "$prcheck" "$branch" "$pane_path" "$prmerge" "$wtask" "$wai" "$prdraft" "$prprog"
		win_id[$idx]="$REPLY_ID"
		win_rest_short[$idx]="$REPLY_REST"
		# shellcheck disable=SC2153 # REPLY_PR set by build_window_label (sourced lib)
		win_pr[$idx]="$REPLY_PR"

		# Long mode only changes the remainder (title / full branch); the id and
		# PR segments are mode-independent.
		build_window_label long "$iprov" "$iid" "$ititle" "$prnum" "$prstate" "$prcheck" "$branch" "$pane_path" "$prmerge" "$wtask" "$wai" "$prdraft" "$prprog"
		win_rest_long[$idx]="$REPLY_REST"

		# Crew badge (#671): once a local window's last agent has exited,
		# @window_has_agent goes empty and the badge must stop rendering here too
		# — an empty crew suppresses it below (mirrors are daemon-owned and
		# unconditional, left alone in the bridge arm above).
		[[ $hasagent == 1 ]] || crew=""
	fi

	# Both arms fall through here, so every width input the fit math reads is
	# filled identically on either path. An arm that skips it leaves pr_colw and
	# crew_colw at 0, and crew_colw == 0 suppresses the badge fragment outright.
	# The composed label is id + rest, which is what build_window_label's REPLY
	# is on the local path.
	win_short[$idx]="${win_id[$idx]}${win_rest_short[$idx]}"
	short_m="${win_short[$idx]}"
	long_m="${win_id[$idx]}${win_rest_long[$idx]}"
	# The daemon stores @window_bridge_name with every '#' doubled, and a
	# '#{@opt}' read hands that back verbatim — but the status line draws it
	# collapsed. Measure what will be drawn, or a '#' in a remote name buys the
	# column a cell it never uses. Only that fallback speaks the doubled dialect
	# and it is all-or-nothing, so one flag decides it for the whole label.
	if ((collapse)); then
		short_m="${short_m//##/#}"
		long_m="${long_m//##/#}"
	fi
	measure_display_width "$short_m"
	win_short_dw[$idx]=$REPLY_DW
	measure_display_width "$long_m"
	win_long_dw[$idx]=$REPLY_DW
	measure_display_width "${win_id[$idx]}"
	win_id_dw[$idx]=$REPLY_DW
	measure_display_width "${win_pr[$idx]}"
	win_pr_dw[$idx]=$REPLY_DW
	((win_pr_dw[$idx] > pr_colw)) && pr_colw=${win_pr_dw[$idx]}

	# Agent codename badge (external fan-out harness stamps @crew_name/@crew_color;
	# a mirror carries the remote's through @bridge_crew_name). Rendered inline off
	# the window's own label, so it is charged per-window in both the single-line
	# fit test and the grid column floor below — never a shared column of its own.
	# The trailing separator space is folded into the segment so the width math is
	# exact.
	if [[ -n $crew ]]; then
		win_crew[$idx]="${crew} "
		measure_display_width "${win_crew[$idx]}"
		win_crew_dw[$idx]=$REPLY_DW
		((win_crew_dw[$idx] > crew_colw)) && crew_colw=${win_crew_dw[$idx]}
	else
		win_crew[$idx]=""
		win_crew_dw[$idx]=0
	fi

	((total++))
done < <(tmux list-windows -t "$SESSION" -F "$FMT")

[[ $total -eq 0 ]] && exit 0

# Stamp the key for this invocation's render (the post-lock count), not the
# pre-lock snapshot: otherwise a later reflow at the stale count cache-hits on
# content drawn for a different window set.
applied_key="${total}:${WIDTH}:${HEIGHT}"

# Fixed icon-column width for the slot math below. The icon *content*
# (@window_icon_padded) is owned solely by tmux-update-icons — don't write it
# here too: this script can't source lib-claude, so it would drop the colored
# claude glyph on every --force reflow, flickering it out until the next tick.
max_icon_width=$((MAX_ICONS * 3 + 2))

# --- Layout: pick label detail (long/short) + column widths, then pack ---
# Slot = idx_width + ": "(2) + name + pr + " "(1) + icon column.
# The PR segment carries its own leading space, so no PR adds nothing.
# The shared pr column (pr_colw) is only charged in the multi-line grid
# slot; single-line entries are unpadded, so the one-row fit charges each
# window its own PR width — otherwise one window growing a PR inflates the
# fit test by pr_colw × window count and flips to compact despite free space.
last_idx=${indices[$((total - 1))]}
idx_width=${#last_idx}
# Fixed ago column in the multi-line slot: the value ticks between reflows and
# flips empty↔set on Claude state changes without triggering a reflow, so a
# live-width column would drift. 1 space + 3 right-aligned cells = 4.
AGO_W=4
slot_overhead=$((idx_width + 3 + max_icon_width)) # ": " + trailing space + icons
overhead=$((slot_overhead + pr_colw + AGO_W))     # + shared pr, ago cols (crew badge is per-window, carved from the label below — not a shared column)

available=$((WIDTH - PREFIX_WIDTH))
zoom_extra=0
((has_zoom)) && zoom_extra=2
SEP_WIDTH=3 # " │ "
# tmux renders at most 5 status lines: the global line plus up to 4 window rows.
# Spending the 5th is only worth it when the terminal has the height to give, so
# the extra row unlocks above TALL_CLIENT_ROWS — a 5-line status bar is an eighth
# of the screen there, against a fifth of an 80x25 at the baseline 3 rows.
MAX_WIN_LINES=3
TALL_CLIENT_ROWS=40
((HEIGHT >= TALL_CLIENT_ROWS)) && MAX_WIN_LINES=4
# Floor for rung 2.5 (long labels in starved columns): keep it only while a
# starved column can still show the id + ~12 chars of branch (≈24 for typical
# 8-char issue ids). Below it the grid degrades to illegible slivers, so fall
# through to short.
LONG_TRUNC_FLOOR=24

# Per-window widths driving the grid: a floor (id + badge + zoom marker, none of
# which the renderer can shrink) and a want (floor + branch/title). The rest is
# capped at MAX_REST_WIDTH so one very long name can't stretch the grid; floors
# are never capped. The single-line fit totals stay uncapped and unpadded — that
# path renders full names via the global format, so it must reserve them.
MAX_REST_WIDTH=40
floor_list=""
want_long_list=""
want_short_list=""
total_long=0
total_short=0
for idx in "${indices[@]}"; do
	floor=$((win_id_dw[$idx] + win_crew_dw[$idx] + win_zoom_dw[$idx]))
	rest_long=$((win_long_dw[$idx] - win_id_dw[$idx]))
	((rest_long > MAX_REST_WIDTH)) && rest_long=$MAX_REST_WIDTH
	rest_short=$((win_short_dw[$idx] - win_id_dw[$idx]))
	((rest_short > MAX_REST_WIDTH)) && rest_short=$MAX_REST_WIDTH
	floor_list+="${floor_list:+ }$floor"
	want_long_list+="${want_long_list:+ }$((floor + rest_long))"
	want_short_list+="${want_short_list:+ }$((floor + rest_short))"
	((total_long += win_long_dw[$idx] + slot_overhead + win_pr_dw[$idx] + win_crew_dw[$idx]))
	((total_short += win_short_dw[$idx] + slot_overhead + win_pr_dw[$idx] + win_crew_dw[$idx]))
done
total_long=$((total_long + (total - 1) * SEP_WIDTH))
total_short=$((total_short + (total - 1) * SEP_WIDTH))

# Detail ladder (which label detail + per-column widths + row count to use) is
# pure integer arithmetic over the per-window widths above -- extracted to
# lib-reflow.sh so it's unit-testable without tmux (tests/reflow.bats).
reflow_pick_layout "$floor_list" "$want_long_list" "$want_short_list" \
	"$total_long" "$total_short" "$total" "$available" "$zoom_extra" \
	"$overhead" "$SEP_WIDTH" "$MAX_WIN_LINES" "$LONG_TRUNC_FLOOR"
labels_mode=$REPLY_LABELS_MODE
needs_multiline=$REPLY_NEEDS_MULTILINE
per=$REPLY_PER
read -ra colws <<<"$REPLY_COLWS"

# Resolved display segments per window. The name column is rendered as
# bold(@window_label_id_disp) + @window_label_disp, so identity + rest fills the
# window's column exactly. The PR segment is padded to its own shared column.
# Single-line mode renders full names via the global format off @window_label_id
# / @window_label_rest_*, so it leaves everything here unpadded.
declare -A win_disp win_pr_glyph win_pr_num win_pr_pad win_id_disp
for pos in "${!indices[@]}"; do
	idx=${indices[$pos]}
	if [[ $labels_mode == long ]]; then
		cur_rest="${win_rest_long[$idx]}"
	else
		cur_rest="${win_rest_short[$idx]}"
	fi

	split_pr_badge "${win_pr[$idx]}"
	win_pr_glyph[$idx]="$REPLY_GLYPH"
	win_pr_num[$idx]="$REPLY_NUM"
	win_pr_pad[$idx]=""

	if ((! needs_multiline)); then
		win_id_disp[$idx]="${win_id[$idx]}"
		win_crew_disp[$idx]="${win_crew[$idx]}"
		win_disp[$idx]="$cur_rest"
		continue
	fi

	colw=${colws[pos % per]}
	# The label must never render wider than its column, or every slot to its
	# right on the row shifts (#271). The badge and zoom marker render inline off
	# this window's label and the id is a fixed prefix, so all three are charged
	# here. When even they overrun the column the badge goes first — it is
	# decoration, the ticket id is identity — and only then is the id clipped.
	# REFLOW_MIN_COLW keeps identity_avail positive, so truncate_to_width always
	# has room for its ellipsis.
	identity_avail=$((colw - win_zoom_dw[$idx]))
	cur_crew="${win_crew[$idx]}"
	cur_crew_dw=${win_crew_dw[$idx]}
	cur_id="${win_id[$idx]}"
	cur_id_dw=${win_id_dw[$idx]}
	if ((cur_crew_dw + cur_id_dw > identity_avail)); then
		cur_crew=""
		cur_crew_dw=0
	fi
	if ((cur_id_dw > identity_avail)); then
		truncate_to_width "$cur_id" "$identity_avail"
		cur_id="$REPLY"
		measure_display_width "$cur_id"
		cur_id_dw=$REPLY_DW
	fi
	win_id_disp[$idx]="$cur_id"
	# Agent badge: the codename + separator space, emitted after the index (see
	# ENTRY). Blank for untagged windows, which then render a pristine full-width
	# label with no leading gap.
	win_crew_disp[$idx]="$cur_crew"

	rest_avail=$((identity_avail - cur_crew_dw - cur_id_dw))
	((rest_avail < 0)) && rest_avail=0
	if ((rest_avail == 0)); then
		cur_rest=""
	else
		truncate_to_width "$cur_rest" "$rest_avail"
		cur_rest="$REPLY"
	fi
	measure_display_width "$cur_rest"
	pad_to_width "$cur_rest" "$REPLY_DW" "$rest_avail"
	win_disp[$idx]="$REPLY"

	printf -v pad '%*s' "$((pr_colw - win_pr_dw[$idx]))" ''
	win_pr_pad[$idx]="$pad"
done

# Split points: break after every REPLY_PER windows. per is already set by
# reflow_pick_layout above (columns per row). An unreached split stays at 999 so
# its row's "index <= split" test passes for every window and the row below it
# renders empty.
current_line=0
split1=999
split2=999
split3=999
if ((needs_multiline)); then
	if ((total > per)); then
		split1=${indices[$((per - 1))]}
		current_line=1
	fi
	if ((total > 2 * per)); then
		split2=${indices[$((2 * per - 1))]}
		current_line=2
	fi
	if ((total > 3 * per)); then
		split3=${indices[$((3 * per - 1))]}
		current_line=3
	fi
fi

# Columns per row in the reflowed grid (single-line mode keeps all on one row).
# Consumed by tmux-window-nav for vertical (row-to-row) window movement.
if ((needs_multiline)); then
	window_per=$per
else
	window_per=$total
fi

# --- Batch simple commands via tmux source, direct calls for complex formats ---
declare -a tmux_cmds=()

# Per-window vars use tmux's argv command-sequence form — one tmux exec per
# window, the 11 sets joined by literal ';' arguments — instead of one exec per
# set (9N execs before). Not `tmux source -`: source re-parses a text stream, so
# free-form issue titles with quotes/';'/'#' would break it. In argv form each
# value is its own execve argument and is never reparsed, so titles pass verbatim.
# @window_label_id stays the full identity for the pickers and the single-line
# global format; @window_label_id_disp is the grid's copy, clipped to the column.
for idx in "${indices[@]}"; do
	target="${SESSION}:${idx}"
	tmux \
		set -w -t "$target" @window_label_short "${win_short[$idx]}" ';' \
		set -w -t "$target" @window_label_id "${win_id[$idx]}" ';' \
		set -w -t "$target" @window_label_id_disp "${win_id_disp[$idx]}" ';' \
		set -w -t "$target" @window_label_rest_short "${win_rest_short[$idx]}" ';' \
		set -w -t "$target" @window_label_rest_long "${win_rest_long[$idx]}" ';' \
		set -w -t "$target" @window_label_disp "${win_disp[$idx]}" ';' \
		set -w -t "$target" @window_pr_plain "${win_pr[$idx]}" ';' \
		set -w -t "$target" @window_pr_glyph "${win_pr_glyph[$idx]}" ';' \
		set -w -t "$target" @window_pr_num "${win_pr_num[$idx]}" ';' \
		set -w -t "$target" @window_pr_pad "${win_pr_pad[$idx]}" ';' \
		set -w -t "$target" @window_crew_disp "${win_crew_disp[$idx]}"
done

# Split points and status line count
tmux_cmds+=("set -t '$SESSION' @window_split '$split1'")
tmux_cmds+=("set -t '$SESSION' @window_split2 '$split2'")
tmux_cmds+=("set -t '$SESSION' @window_split3 '$split3'")
tmux_cmds+=("set -t '$SESSION' @window_per '$window_per'")
tmux_cmds+=("set -t '$SESSION' @reflow_key '$applied_key'")
tmux_cmds+=("set -t '$SESSION' @labels_mode '${labels_mode}'")

tmux_cmds+=("set -t '$SESSION' status $((current_line + 2))")

# Single-line branch collapses into the same batch as an atomic unset of the
# whole session-level status-format array. Doing this as one command matters:
# tmux treats session-level status-format as all-or-nothing (any set index
# suppresses global for ALL indices), so unsetting [0], [1], [2], [3] in
# separate tmux calls creates a visible intermediate where [0] is unset but
# [1..4] are still set — line 0 renders blank and the session name flashes
# away. The per-index unsets that used to live here are redundant with the
# bare `status-format` unset below.
if ((! needs_multiline && current_line == 0)); then
	tmux_cmds+=("set -u -t '$SESSION' status-format")
fi

# Execute batched simple commands + early redraw so layout appears immediately
if log_enabled; then
	log_event reflow event recompute forced "$FORCE" wins "$total" \
		width "$WIDTH" height "$HEIGHT" split1 "$split1" split2 "$split2" \
		split3 "$split3" lines "$((current_line + 2))" colws "${colws[*]}" \
		labels_mode "$labels_mode" sess "$SESSION"
fi
{
	printf '%s\n' "${tmux_cmds[@]}"
	echo "refresh-client -S"
} | tmux source -

# Common format fragments
# Colour/state options read live at render time. A mirror window's own copies
# belong to the launcher's repo; the daemon ships the remote's under @bridge_*,
# so every such read is gated on @bridge_win. Built per option name, not per
# site — each of these appears more than once below. The commas inside the
# conditional are deliberately NOT '#,'-escaped: format_expand resolves it before
# format_draw parses '#[…]', and its argument splitter tracks '#{'/'}' nesting.
declare -A bopt
for o in crew_color pr_number pr_state pr_check_state pr_mergeable pr_review pr_auto_merge; do
	bopt[$o]="#{?#{@bridge_win},#{@bridge_${o}},#{@${o}}}"
done
SEP=" #[fg=#{@thm_subtext_0}#,nobold]│ "
ICON='#{@window_icon_padded}'
# Name column: bold identity prefix + column-padded remainder (id + disp fill
# the window's column exactly). The id is always bold; the remainder stays bold
# on the active window (BASE turns bold on for the whole marker) and drops to
# regular weight elsewhere. Label content changes outside structural events
# (issue stamp, PR arrival) re-enter via the providers' forced reflow calls.
NAME="#[bold]#{@window_label_id_disp}#{?window_active,,#[nobold]}#{@window_label_disp}"
LABEL_Z="${NAME}#{?window_zoomed_flag, 󰁌,}"
IDX="#{p${idx_width}:window_index}"
# Base tab color: the active window's "index: label" text takes bold mauve
# (Catppuccin's accent); the rest dim on the default bg. Text-only — no fill, no
# underline — so colored process glyphs and the PR badge keep their own colors.
# The accent is scoped to "index: label"; ICONFG ends it before the icon column.
BASE="#{?window_active,#[fg=#{@thm_mauve}#,bg=#{@thm_bg}#,bold],#[fg=#{@thm_subtext_0}#,bg=#{@thm_bg}]}"
# End the active accent before the icons: bright fg on the default bg with bold
# off, so glyphs render as on inactive tabs (only brighter) and the colored
# Claude icon shows its state color. No-op on inactive tabs.
ICONFG="#{?window_active,#[fg=#{@thm_fg}#,bg=#{@thm_bg}#,nobold],}"
# PR segment colored by state on every tab. Merged/closed are terminal and
# checked first (merged=mauve, closed=overlay0), so a leftover pending/failed
# rollup can't tint them peach/red; then conflicting/failing=red, pending=peach,
# success/open=green. closed = a dead/superseded PR, dimmed so it can't read as
# a live one. No PR → no color directive. Rendered on the glyph half
# (@window_pr_glyph) only — PRNUM below tints the #<n> half separately, and
# @window_pr_pad is just column padding with no color of its own.
PRCOLOR="#{?#{&&:${bopt[pr_number]},#{!=:${bopt[pr_number]},none}},#{?#{==:${bopt[pr_state]},merged},#[fg=#{@thm_mauve}],#{?#{==:${bopt[pr_state]},closed},#[fg=#{@thm_overlay_0}],#{?#{||:#{==:${bopt[pr_check_state]},failure},#{==:${bopt[pr_mergeable]},conflicting}},#[fg=#{@thm_red}],#{?#{==:${bopt[pr_check_state]},pending},#[fg=#{@thm_peach}],#[fg=#{@thm_green}]}}}},}"
# The #<n> half: tinted by review decision and underlined for a queued
# auto-merge, open PRs only. With no decision it keeps PRCOLOR's tint.
PRNUM="#{?#{==:${bopt[pr_state]},open},#{?#{==:${bopt[pr_review]},approved},#[fg=#{@thm_green}],#{?#{==:${bopt[pr_review]},changes_requested},#[fg=#{@thm_red}],#{?#{==:${bopt[pr_review]},review_required},#[fg=#{@thm_overlay_0}],}}}#{?${bopt[pr_auto_merge]},#[underscore],},}"
# "Last active" column for halted Claude windows (@window_claude_ago, kept fresh
# by tmux-update-icons). Right-aligned and padded to AGO_W's fixed width so the
# value (and an empty value, for active/non-claude windows) always occupies the
# same cells — this is what keeps grid columns aligned as the value ticks and appears.
AGO=" #[fg=#{@thm_overlay_1}]#{p-3:@window_claude_ago}"
# Agent-badge segment, emitted after "index: " (the @window_crew_disp carries a
# trailing separator space). Tinted by its stamped @crew_color; re-assert BASE
# after it so the label reverts to the tab color instead of inheriting the tint.
# Emitted only when at least one window is tagged (crew_colw > 0); untagged
# windows carry an empty @window_crew_disp and render a gapless full-width label.
CREW=""
((crew_colw > 0)) && CREW="#{?${bopt[crew_color]},#[fg=${bopt[crew_color]}#,bg=#{@thm_bg}],}#{@window_crew_disp}${BASE}"
ENTRY="#[range=window|#{window_index}]#[nobold]${BASE}${IDX}: ${CREW}${LABEL_Z}${ICONFG} ${ICON}${PRCOLOR}#{@window_pr_glyph}${PRNUM}#{@window_pr_num}#[nounderscore]#{@window_pr_pad}${AGO}#[norange]"

# Multi-line branches stay on direct `tmux set` calls: FMT0 contains embedded
# single quotes (e.g. '#{session_name}') that break outer-single-quoted
# batched commands. See commit 60421e7.
if ((! needs_multiline && current_line == 0)); then
	: # handled via batched unset above
elif ((current_line == 0)); then
	FMT0=$(tmux show -gv status-format[0] 2>/dev/null)
	[[ -n $FMT0 ]] && tmux set -t "$SESSION" status-format[0] "$FMT0"
	tmux set -t "$SESSION" status-format[1] \
		"#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ╰─ #{W:${ENTRY}#{?next_window_index,${SEP},}}"
	tmux set -t "$SESSION" status-format[2] ""
	tmux set -t "$SESSION" status-format[3] ""
	tmux set -t "$SESSION" status-format[4] ""
else
	FMT0=$(tmux show -gv status-format[0] 2>/dev/null)
	[[ -n $FMT0 ]] && tmux set -t "$SESSION" status-format[0] "$FMT0"
	PREFIX1="#{?#{e|>|:#{session_windows},#{@window_split}},├,╰}─"
	tmux set -t "$SESSION" status-format[1] \
		"#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ${PREFIX1} #{W:#{?#{e|<=|:#{window_index},#{@window_split}},${ENTRY}#{?next_window_index,#{?#{e|<=|:#{next_window_index},#{@window_split}},${SEP},},},}}"

	PREFIX2="#{?#{e|>|:#{session_windows},#{@window_split2}},├,╰}─"
	tmux set -t "$SESSION" status-format[2] \
		"#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ${PREFIX2} #{W:#{?#{e|>|:#{window_index},#{@window_split}},#{?#{e|<=|:#{window_index},#{@window_split2}},${ENTRY}#{?next_window_index,#{?#{e|<=|:#{next_window_index},#{@window_split2}},${SEP},},},},}}"

	PREFIX3="#{?#{e|>|:#{session_windows},#{@window_split3}},├,╰}─"
	tmux set -t "$SESSION" status-format[3] \
		"#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ${PREFIX3} #{W:#{?#{e|>|:#{window_index},#{@window_split2}},#{?#{e|<=|:#{window_index},#{@window_split3}},${ENTRY}#{?next_window_index,#{?#{e|<=|:#{next_window_index},#{@window_split3}},${SEP},},},},}}"

	# Row 4 only ever holds windows when MAX_WIN_LINES rose to 4 on a tall
	# client; at 3 rows @window_split3 stays 999 so this renders empty and tmux
	# is left at `status 4`, which never asks for index 4 anyway.
	tmux set -t "$SESSION" status-format[4] \
		"#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ╰─ #{W:#{?#{e|>|:#{window_index},#{@window_split3}},${ENTRY}#{?next_window_index,${SEP},},}}"
fi

# Force immediate status bar redraw
tmux refresh-client -S 2>/dev/null || true
