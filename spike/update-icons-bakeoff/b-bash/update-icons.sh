#!/usr/bin/env bash
# Variant B: scripts/tmux-update-icons.sh's 1 s window-naming path with two
# changes and nothing else: the list-sessions and list-panes reads become one
# tmux call, and `timeout 2 git` becomes a read -t guard on a process
# substitution (no timeout fork). Same libs, same per-window logic.
#
# Out of scope, as in every variant: the 5 s arming sweep, carousel/remux
# stamping, the cwd-move reconcile, the reflow kick, agent-state file parsing,
# and the branch-transition path. Each exits 3 rather than run a path the
# fixture does not exercise.
#
# Needs OG_LIB_ICONS/OG_LIB_CLAUDE/OG_LIB_LOG, OG_AGENT_COMMANDS, OG_MAX_ICONS
# (bench.sh exports them from the built wrapper).

# shellcheck source=/dev/null
source "$OG_LIB_ICONS"
# shellcheck source=/dev/null
source "$OG_LIB_CLAUDE"
# shellcheck source=/dev/null
source "$OG_LIB_LOG"

AGENT_COMMANDS="$OG_AGENT_COMMANDS"
MAX_ICONS="$OG_MAX_ICONS"

normalize_wrapped_cmd() {
	REPLY="$1"
	[[ $REPLY == .*-wrapped ]] && REPLY="${REPLY#.}" && REPLY="${REPLY%-wrapped}"
}

out_of_scope() {
	echo "update-icons bake-off: $1 is out of scope" >&2
	exit 3
}

# git_branch DIR -- `git branch --show-current` guarded by a 2 s read timeout.
# A process substitution + read -t replaces the `timeout` exec; a stuck git is
# killed instead of waited on. Sets REPLY.
git_branch() {
	local fd rc=0
	exec {fd}< <(git -C "$1" branch --show-current 2>/dev/null)
	local pid=$!
	read -r -t 2 -u "$fd" REPLY || rc=$?
	((rc > 128)) && kill "$pid" 2>/dev/null
	exec {fd}<&-
	((rc == 0)) || REPLY=""
}

main() {
	SESSION=$1
	SERVER_START=$3
	CATPPUCCIN_FLAVOR=$5
	SERVER_PID=$6

	setup_claude_colors "$CATPPUCCIN_FLAVOR"
	claude_prune_stale_state "$SERVER_START" "$SERVER_PID"

	declare -A sess_id_of
	declare -A win_procs win_pane_path win_cur_branch win_cur_task win_cur_name
	declare -A win_cur_display win_cur_padded win_cur_ago win_cur_rename win_cur_crew win_cur_crew_seen win_cur_bridge
	declare -A win_cur_has_agent win_cur_manual win_cur_naming_dirty win_cwd win_poison
	declare -A all_sess sess_cur_active_icon sess_cur_session_fg sess_active_proc sess_active_win

	# One roundtrip for both reads. The S/P prefix routes each row; a session
	# name may contain '|' so it is taken as the remainder after the id.
	local data line rest
	data=$(tmux list-sessions -F 'S|#{session_id}|#{session_name}' \; \
		list-panes -a -f '#{!:#{pane_modal_flag}}' -F 'P|#{pane_id}|#{session_id}|#{window_index}|#{pane_index}|#{pane_current_path}|#{pane_current_command}|#{@branch}|#{pane_floating_flag}|#{@worktree}|#{@window_cwd_seen}|#{?window_modal_pane,#{pane_last},#{pane_active}}|#{window_active}|#{s/[|]/ /:@window_ai_name}|#{s/[|]/ /:@remux_relaunch}|#{@window_icon_display}|#{@window_icon_padded}|#{@window_claude_ago}|#{automatic-rename}|#{@active_pane_icon}|#{@claude_session_fg}|#{@crew_name}|#{@crew_seen}|#{@bridge_win}|#{@bridge_proc}|#{@claude_img_src}|#{@window_has_agent}|#{@window_manual_name}|#{@window_naming_dirty}|#{@window_task}')

	local pane_id sess idx pidx pane_path proc cur_branch pane_floating cur_worktree cur_cwd_seen pane_active window_active cur_ai_name cur_relaunch cur_display cur_padded cur_ago cur_rename opt_active_icon opt_session_fg cur_crew cur_crew_seen cur_bridge bridge_proc cur_img_src cur_has_agent cur_manual cur_naming_dirty cur_task
	local sid sname wkey existing
	while IFS= read -r line; do
		case $line in
		S\|*)
			rest="${line#S|}"
			sid="${rest%%|*}"
			sname="${rest#*|}"
			sess_id_of[$sname]="$sid"
			;;
		P\|*)
			# shellcheck disable=SC2034  # unused fields keep the format string A's verbatim
			IFS='|' read -r pane_id sess idx pidx pane_path proc cur_branch pane_floating cur_worktree cur_cwd_seen pane_active window_active cur_ai_name cur_relaunch cur_display cur_padded cur_ago cur_rename opt_active_icon opt_session_fg cur_crew cur_crew_seen cur_bridge bridge_proc cur_img_src cur_has_agent cur_manual cur_naming_dirty cur_task <<<"${line#P|}"
			[[ -n $bridge_proc ]] && proc="$bridge_proc"
			wkey="$sess:$idx"
			[[ $pane_active == 0 || $pane_active == 1 ]] || win_poison[$wkey]=1
			all_sess[$sess]=1
			sess_cur_active_icon[$sess]="$opt_active_icon"
			sess_cur_session_fg[$sess]="$opt_session_fg"
			if [[ -z ${win_pane_path[$wkey]+x} ]]; then
				win_pane_path[$wkey]="$pane_path"
				win_cur_branch[$wkey]="$cur_branch"
				win_cur_task[$wkey]="$cur_task"
				win_cur_name[$wkey]="$cur_ai_name"
				win_cur_display[$wkey]="$cur_display"
				win_cur_padded[$wkey]="$cur_padded"
				win_cur_ago[$wkey]="$cur_ago"
				win_cur_rename[$wkey]="$cur_rename"
				win_cur_crew[$wkey]="$cur_crew"
				win_cur_crew_seen[$wkey]="$cur_crew_seen"
				win_cur_bridge[$wkey]="$cur_bridge"
				win_cur_has_agent[$wkey]="$cur_has_agent"
				win_cur_manual[$wkey]="$cur_manual"
				win_cur_naming_dirty[$wkey]="$cur_naming_dirty"
			fi
			if [[ -z ${win_cwd[$wkey]+x} && $pane_floating != 1 ]]; then
				win_cwd[$wkey]="$pane_path"
			fi
			[[ $window_active == 1 ]] && sess_active_win[$sess]="$idx"
			[[ $pane_active == 1 && $window_active == 1 ]] && sess_active_proc[$sess]="$proc"
			[[ -z $proc ]] && continue
			existing="${win_procs[$wkey]:-}"
			case " $existing " in
			*" $proc "*) ;;
			*) win_procs[$wkey]="${existing:+$existing }$proc" ;;
			esac
			;;
		esac
	done <<<"$data"
	INVOKE_SID="${sess_id_of[$SESSION]:-}"

	# Agent-state files are not parsed in any variant.
	[[ -z $(claude_pane_ids) ]] || out_of_scope "agent state files"

	local tmux_cmds="" wkey_ s idx_ p has_agent clear_needed branch pane_path_ display icon icon_dw
	declare -A win_icons win_icon_dw win_display
	declare -a all_idx=()
	for wkey_ in "${!win_pane_path[@]}"; do
		all_idx+=("$wkey_")
		s="${wkey_%:*}"
		idx_="${wkey_##*:}"
		pane_path_="${win_cwd[$wkey_]:-${win_pane_path[$wkey_]}}"

		has_agent=""
		if [[ ${win_cur_bridge[$wkey_]:-} != 1 ]]; then
			# shellcheck disable=SC2086  # win_procs is a space-joined string
			for p in ${win_procs[$wkey_]:-}; do
				normalize_wrapped_cmd "$p"
				case " $AGENT_COMMANDS " in *" $REPLY "*)
					has_agent=1
					break
					;;
				esac
			done
		fi
		# No self-report files exist in the fixture, so task and AI name are empty
		# and the naming_read gate (which only guards their reads) drops out.
		if [[ -n ${win_cur_task[$wkey_]:-} ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @window_task ''"$'\n'
		fi
		if [[ -n ${win_cur_name[$wkey_]:-} ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @window_ai_name ''"$'\n'
		fi
		if [[ ${win_cur_crew[$wkey_]:-} != "${win_cur_crew_seen[$wkey_]:-}" ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @crew_seen '${win_cur_crew[$wkey_]:-}'"$'\n'
		fi

		clear_needed=""
		if [[ -z $has_agent && -n ${win_cur_naming_dirty[$wkey_]:-} ]]; then
			clear_needed=1
		fi
		if [[ -z ${win_poison[$wkey_]:-} && ${win_cur_bridge[$wkey_]:-} != 1 && ($has_agent != "${win_cur_has_agent[$wkey_]:-}" || -n $clear_needed) ]]; then
			[[ -n $has_agent ]] || out_of_scope "agent-left naming clear"
			tmux_cmds+="set -qw -t '$wkey_' @window_has_agent 1"$'\n'
		fi

		if [[ ($s == "$INVOKE_SID" && $idx_ == "${sess_active_win[$INVOKE_SID]:-}") || -z ${win_cur_branch[$wkey_]:-} ]]; then
			git_branch "$pane_path_"
			branch="$REPLY"
			[[ $branch == "${win_cur_branch[$wkey_]:-}" ]] || out_of_scope "branch transition"
		fi

		build_proc_icons "${win_procs[$wkey_]:-}" "$MAX_ICONS"
		display="${REPLY% }"
		icon="$REPLY"
		icon_dw=$REPLY_DW
		win_icons[$wkey_]="$icon"
		win_icon_dw[$wkey_]=$icon_dw
		win_display[$wkey_]="$display"

		if [[ -n ${win_cur_ago[$wkey_]:-} ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @window_claude_ago ''"$'\n'
		fi
	done

	local active_icon
	for s in "${!all_sess[@]}"; do
		active_icon=""
		proc="${sess_active_proc[$s]:-}"
		normalize_wrapped_cmd "$proc"
		proc="$REPLY"
		[[ -n $proc ]] && active_icon="${ICON_MAP[$proc]:-}"
		if [[ $active_icon != "${sess_cur_active_icon[$s]:-}" ]]; then
			tmux_cmds+="set -q -t '$s' @active_pane_icon '$active_icon'"$'\n'
		fi
		if [[ -n ${sess_cur_session_fg[$s]:-} ]]; then
			tmux_cmds+="set -q -t '$s' @claude_session_fg ''"$'\n'
		fi
	done

	local TARGET_DW=$((MAX_ICONS * 3 + 2))
	for wkey_ in "${all_idx[@]}"; do
		if [[ ${win_display[$wkey_]} != "${win_cur_display[$wkey_]:-}" ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @window_icon_display '${win_display[$wkey_]}'"$'\n'
		fi
		if [[ ${win_cur_bridge[$wkey_]:-} == 1 ]]; then
			if [[ ${win_cur_rename[$wkey_]:-} == 1 ]]; then
				tmux_cmds+="set -qw -t '$wkey_' automatic-rename off"$'\n'
			fi
		elif [[ ${win_cur_rename[$wkey_]:-} != 1 && ${win_cur_manual[$wkey_]:-} != 1 ]]; then
			tmux_cmds+="set -qw -t '$wkey_' automatic-rename on"$'\n'
		fi
		pad_to_width "${win_icons[$wkey_]}" "${win_icon_dw[$wkey_]}" "$TARGET_DW"
		if [[ $REPLY != "${win_cur_padded[$wkey_]:-}" ]]; then
			tmux_cmds+="set -qw -t '$wkey_' @window_icon_padded '$REPLY'"$'\n'
		fi
	done

	if [[ -n $tmux_cmds ]]; then
		printf '%s' "$tmux_cmds" | tmux source -
	fi
}

main "$@"
