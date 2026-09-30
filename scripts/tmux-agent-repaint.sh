#!/usr/bin/env bash
# One-shot repaint nudge for a newborn agent pane (#883). An agent TUI that
# rendered while its pane was still being split/refit keeps a garbled first
# frame; once it is ready, one real size change and back makes it repaint.
#   args: <pane> <birth WxH>   (the tmux-update-icons sweep claims the pane's
#                               @og_birth_size and starts this via run-shell -b)
set -uo pipefail

pane=${1:-}
birth=${2:-}
[[ $pane =~ ^%[0-9]+$ && $birth =~ ^[0-9]+x[0-9]+$ ]] || exit 0

# A free nudge slot: not zoomed (a zoom is a sibling worker's nudge or the
# user's, so wait it out), no float (re-checked here so a popup opened during
# the wait blocks the nudge), and — for a multi-pane window — the target is
# active or nobody is looking, because a zoom makes its pane active and would
# steal the keys of a user typing into another pane. Evaluated inside
# `if -F` together with the nudge, so no sibling can slot in between.
SLOT='#{&&:#{&&:#{!:#{window_zoomed_flag}},#{!:#{P:#{?pane_floating_flag,1,}}}},#{||:#{==:#{window_panes},1},#{||:#{pane_active},#{==:#{window_active_clients},0}}}}'

# Ready: past startup, then output-quiet for 1 s, capped. An empty
# pane_output_generation (a resident server predating the format) compares
# equal, so it counts as quiet.
start=$SECONDS
sleep 3
while :; do
	g1=$(tmux display -p -t "$pane" '#{pane_output_generation}' 2>/dev/null) || exit 0
	sleep 1
	g2=$(tmux display -p -t "$pane" '#{pane_output_generation}' 2>/dev/null) || exit 0
	[[ $g1 == "$g2" ]] && break
	((SECONDS - start >= 12)) && break
done

info=$(tmux display -p -t "$pane" '#{pane_width}|#{pane_height}|#{window_id}|#{window_width}|#{window_height}|#{window_panes}|#{pane_floating_flag}|#{@bridge_win}|#{P:#{?pane_floating_flag,1,}}' 2>/dev/null) || exit 0
IFS='|' read -r pw ph wid ww wh np floating bridge floats <<<"$info"
[[ ${pw}x$ph == "$birth" ]] && exit 0
[[ $floating == 1 || -n $bridge || -n $floats ]] && exit 0
[[ $wid =~ ^@[0-9]+$ && $ww =~ ^[0-9]+$ && $wh =~ ^[0-9]+$ ]] || exit 0

deadline=$((SECONDS + 8))
while ((SECONDS < deadline)); do
	if [[ $np == 1 ]]; then
		saved=$(tmux show -wqv -t "$wid" window-size 2>/dev/null)
		# Two separate resizes, so the app sees two real size changes rather
		# than one coalesced no-op. The claim means one worker per pane, and a
		# single-pane window has one pane, so nothing interleaves the
		# window-size save/restore.
		out=$(tmux if -F -t "$pane" "#{&&:$SLOT,#{==:#{window_panes},1}}" "resize-window -t $wid -x $((ww - 1)) -y $((wh - 1))" 'display -p no' 2>/dev/null) || exit 0
		if [[ $out != no ]]; then
			sleep 0.3
			tmux resize-window -t "$wid" -x "$ww" -y "$wh" 2>/dev/null
			if [[ -z $saved ]]; then
				tmux set -wu -t "$wid" window-size 2>/dev/null
			else
				tmux set -w -t "$wid" window-size "$saved" 2>/dev/null
			fi
			exit 0
		fi
		np=$(tmux display -p -t "$pane" '#{window_panes}' 2>/dev/null) || exit 0
		[[ $np != 1 ]] && continue
	else
		np=$(tmux display -p -t "$pane" '#{window_panes}' 2>/dev/null) || exit 0
		[[ $np == 1 ]] && continue
		# The focus read, the slot check and the zoom are one command list in
		# the server, so no sibling's zoom can be observed or interleaved:
		# act/last are the focus as it stood with no zoom in the window.
		out=$(tmux if -F -t "$pane" "$SLOT" "display -p -t $pane '#{P:#{?pane_active,#{pane_id},}}|#{P:#{?pane_last,#{pane_id},}}' ; resize-pane -Z -t $pane" 'display -p no' 2>/dev/null) || exit 0
		if [[ $out != no ]]; then
			IFS='|' read -r act last <<<"$out"
			# A malformed read still unzooms, focus left alone, rather than
			# leaving the window zoomed.
			if ! [[ $act =~ ^%[0-9]+$ && ($last == "" || $last =~ ^%[0-9]+$) ]]; then
				act=$pane last=
			fi
			sleep 0.3
			# Unzoom and focus restore are ONE command list, so a phase-aligned
			# sibling cannot zoom between them. Keep the order: last first, then
			# active, so both {last} and the active pane end as they were. Do not
			# reorder or split. An empty {last} cannot be restored.
			restore=
			if [[ $act != "$pane" ]]; then
				[[ -n $last && $last != "$pane" ]] && restore=" ; select-pane -t $last"
				restore+=" ; select-pane -t $act"
			fi
			tmux if -F -t "$pane" '#{&&:#{window_zoomed_flag},#{pane_active}}' "resize-pane -Z -t $pane$restore" 2>/dev/null
			exit 0
		fi
	fi
	sleep 0.25
done
