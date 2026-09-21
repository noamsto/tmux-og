#!/usr/bin/env bash
# Update Claude status for tmux integration
# Called by Claude Code hooks to track session state
# Usage: claude-status-update <state> [--pane PANE_ID] [--session SESSION_NAME]
#
# States: processing, waiting, done, idle, compacting, error, denied
# Environment: Uses $TMUX_PANE and tmux to detect session if not provided

set -euo pipefail

STATE_DIR="${CLAUDE_STATUS_DIR:-/tmp/claude-status}"
PANES_DIR="$STATE_DIR/panes"
ISSUES_DIR="$STATE_DIR/issues"
TASKS_DIR="$STATE_DIR/tasks"
NAMES_DIR="$STATE_DIR/names"
IMAGES_DIR="$STATE_DIR/images"
SCREEN_DIR="$STATE_DIR/screen"

# Ensure directories exist
mkdir -p "$PANES_DIR"

# Event logging (no-op unless debug armed). Guarded so the RAW script still runs
# under tests/claude-issues.bats, where @lib_log@ is not substituted.
# shellcheck source=/dev/null
if [[ -f "@lib_log@" ]]; then
	source "@lib_log@"
else
	log_enabled() { return 1; }
	log_event() { :; }
	# A WORKING detach, not a `:` stub like its neighbours: the notification call
	# below runs through it, and this script is `set -euo pipefail`, so an
	# undefined detach would abort before the pane state write. A no-op stub
	# would be worse than a crash — it would swallow the call and leave the
	# producer's own tests green against a capture fake never invoked. Body
	# copied from lib-log.sh.
	detach() { (nohup "$@" >/dev/null 2>&1 &) }
fi

# Progress OSC helper. Guarded like lib-log so the RAW script still runs under
# bats, where @lib_claude@ is not substituted.
# shellcheck source=/dev/null
if [[ -f "@lib_claude@" ]]; then
	source "@lib_claude@"
else
	claude_progress_emit() { :; }
fi

# Notification seam. A value still starting with '@' means the placeholder was
# never substituted — the raw script under bats, or a build with notifications
# disabled — and is the single "notifications off" mechanism. Never empty: an
# empty value would run `detach "" emit …`.
NOTIFY_BIN="${OG_NOTIFY_BIN:-@notify@}"

# bridge_stamp OPTION VALUE
# Mirrors a status write into a pane option, then refreshes — one tmux call for
# both, replacing the bare refresh each caller used to do. The remote-bridge
# daemon reads these options over its control connection: a control-mode client
# renders no status line, so on a host whose only client is the bridge the #()
# pollers that expose this state locally never run at all.
bridge_stamp() {
	[[ -n ${TMUX:-} ]] || return 0
	if [[ -n ${pane_id:-} ]]; then
		tmux set -pq -t "$pane_id" "$1" "$2" \; refresh-client -S 2>/dev/null || true
	else
		tmux refresh-client -S 2>/dev/null || true
	fi
}

# Reflow seam, pinned to the store path for the reason tmux-update-icons' own
# @reflow@ is: a bare name resolves against the tmux server's frozen PATH and
# stays stale until a server restart (#336). Still starting with '@' means the
# placeholder was never substituted, and disables the forced reflow.
REFLOW_BIN="@reflow@"

# window_stamp OPTION VALUE
# Mirrors a self-report into the WINDOW option tmux-update-icons would otherwise
# write on its 1s tick, then forces the reflow that rebuilds @window_label_*
# from it. bridge_stamp's problem one level up: a control-mode client renders no
# status line, so on a host whose only clients are bridges that poller has never
# run, and it is the sole other writer of @window_ai_name/@window_task (#589).
#
# Active pane only, the rule update-icons applies — a background agent in a
# split must not retitle the window out from under the one on screen, looking
# past a modal float.
window_stamp() {
	[[ -n ${TMUX:-} && -n ${pane_id:-} ]] || return 0
	local active win sess cur
	# session_name last: it may itself contain the '|' delimiter, and as the
	# final read target it absorbs the rest of the line whole.
	IFS='|' read -r active win sess < <(tmux display-message -p -t "$pane_id" \
		'#{?#{?window_modal_pane,#{pane_last},#{pane_active}},1,}|#{window_id}|#{session_name}' 2>/dev/null) || return 0
	[[ $active == 1 && -n $win ]] || return 0
	cur=$(tmux show -wqv -t "$win" "$1" 2>/dev/null)
	[[ $cur == "$2" ]] && return 0
	tmux set -qw -t "$win" "$1" "$2" 2>/dev/null || true
	[[ $REFLOW_BIN == @* ]] && return 0
	"$REFLOW_BIN" "$sess" --force >/dev/null 2>&1 &
}

# Function to clean up stale pane entries
# Removes entries only for panes that no longer exist in tmux. A live pane is
# kept even when its foreground command isn't claude/opencode: Claude shells
# out constantly (builds, pagers, git), and that momentary command must not be
# read as "session gone" — doing so wipes issue stamps mid-session. Genuine
# exit is handled by the SessionEnd hook (`clear`), not by sampling here.
cleanup_stale_panes() {
	[[ -d $PANES_DIR ]] || return 0
	command -v tmux &>/dev/null || return 0

	# Build lookup: pane_id (without %) -> 1 for every pane that still exists
	declare -A pane_exists
	while IFS='|' read -r pid _; do
		pane_exists["${pid#%}"]=1
	done < <(tmux list-panes -a -F '#{pane_id}|#{pane_current_command}' 2>/dev/null || true)

	# A successful query always lists at least this hook's own pane, so an empty
	# map means list-panes failed (server hiccup) or hit the wrong/no server (CC
	# run outside tmux). Bail rather than treat "saw nothing" as "everything is
	# gone" — that would wipe every live pane's status files in one sweep.
	[[ -n ${pane_exists[*]:-} ]] || return 0

	# Check each pane file
	for pf in "$PANES_DIR"/*; do
		[[ -f $pf ]] || continue
		local pane_file="${pf##*/}"

		if [[ -z ${pane_exists[$pane_file]+x} ]]; then
			claude_progress_emit "$pane_file" clear
			rm -f "$pf" "$ISSUES_DIR/${pf##*/}" "$TASKS_DIR/${pf##*/}" "$NAMES_DIR/${pf##*/}" "$IMAGES_DIR/${pf##*/}.jsonl" "$SCREEN_DIR/${pf##*/}"
		fi
	done

	# Orphaned issue / task / name / screen files (pane gone)
	for inf in "$ISSUES_DIR"/* "$TASKS_DIR"/* "$NAMES_DIR"/* "$SCREEN_DIR"/*; do
		[[ -f $inf ]] || continue
		if [[ -z ${pane_exists[${inf##*/}]+x} ]]; then
			rm -f "$inf"
		fi
	done
}

# Parse arguments
state="${1:-}"
pane_id="${TMUX_PANE:-}"
session_name=""
win_target=""
force=0
transcript_path=""

shift || true

# Issue self-report: tracks which issues CC is working on, in a file SEPARATE
# from the pane state file — state hooks fire around the very Bash call that
# runs `issue add`, so sharing the pane file would lose updates.
if [[ $state == "issue" ]]; then
	action="${1:-}"
	shift || true
	id=""
	case "$action" in
	add | done)
		if [[ ${1:-} != --* ]]; then
			id="${1:-}"
			shift || true
		fi
		;;
	clear) ;;
	*)
		echo "Error: Invalid issue action '$action'. Use: add, done, clear" >&2
		exit 1
		;;
	esac
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--pane)
			pane_id="$2"
			shift 2
			;;
		*) shift ;;
		esac
	done
	if [[ $action != "clear" && ! $id =~ ^[A-Za-z0-9_-]+$ ]]; then
		echo "Error: Invalid issue id '$id' (allowed: A-Z a-z 0-9 _ -)" >&2
		exit 1
	fi
	[[ -z $pane_id ]] && exit 0
	issues_file="$ISSUES_DIR/${pane_id#%}"
	case "$action" in
	add)
		mkdir -p "$ISSUES_DIR"
		current=""
		[[ -f $issues_file ]] && IFS= read -r current <"$issues_file" || true
		case ",$current," in
		*",$id,"*) ;;
		*)
			[[ -n $current ]] && current+=","
			printf '%s\n' "$current$id" >"$issues_file"
			;;
		esac
		;;
	done)
		if [[ -f $issues_file ]]; then
			IFS= read -r current <"$issues_file" || true
			keep=()
			IFS=',' read -r -a ids <<<"$current"
			for i in "${ids[@]}"; do
				[[ $i == "$id" || -z $i ]] || keep+=("$i")
			done
			if ((${#keep[@]})); then
				(
					IFS=','
					printf '%s\n' "${keep[*]}"
				) >"$issues_file"
			else
				rm -f "$issues_file"
			fi
		fi
		;;
	clear)
		rm -f "$issues_file"
		;;
	esac
	log_enabled && log_event claude event issue op "$action" id "${id:-}" pane "%${pane_id#%}"
	current=""
	[[ -f $issues_file ]] && IFS= read -r current <"$issues_file" || true
	bridge_stamp @claude_issues "$current"
	exit 0
fi

# Task self-report: a short freeform phrase for "what this pane's Claude is
# doing", captured from the latest user prompt (UserPromptSubmit hook). Kept in
# its own file like the issue ids; surfaces as the window-label fallback when no
# branch/issue/PR distinguishes the window (multiple Claudes in one checkout).
if [[ $state == "task" ]]; then
	action="${1:-}"
	shift || true
	text=""
	case "$action" in
	set)
		# Text is mandatory and free-form (a captured prompt), so take it
		# verbatim — even a leading "--" is content here, not a flag.
		text="${1:-}"
		shift || true
		;;
	clear) ;;
	*)
		echo "Error: Invalid task action '$action'. Use: set, clear" >&2
		exit 1
		;;
	esac
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--pane)
			pane_id="$2"
			shift 2
			;;
		*) shift ;;
		esac
	done
	[[ -z $pane_id ]] && exit 0
	tasks_file="$TASKS_DIR/${pane_id#%}"
	case "$action" in
	set)
		# Squeeze to one line, drop control chars, trim, cap length — the value
		# becomes a tmux window option and a status-bar segment. Delete cntrl (not
		# keep [:print:]): tr is byte-oriented, so [:print:] would strip every
		# non-ASCII byte and mangle UTF-8 (emoji, accents, RTL text).
		clean=$(printf '%s' "$text" | tr '\n\r\t' '   ' | tr -d '[:cntrl:]' | tr -s ' ')
		clean="${clean# }"
		clean="${clean:0:60}"
		clean="${clean% }"
		if [[ -n $clean ]]; then
			mkdir -p "$TASKS_DIR"
			printf '%s\n' "$clean" >"$tasks_file"
		fi
		;;
	clear)
		rm -f "$tasks_file"
		;;
	esac
	task_now=""
	[[ -f $tasks_file ]] && IFS= read -r task_now <"$tasks_file" || true
	bridge_stamp @claude_task "$task_now"
	window_stamp @window_task "$task_now"
	exit 0
fi

# Window name: a short descriptive title for a fallback window (no tracked issue,
# on the default branch). On the first such prompt the UserPromptSubmit hook
# seeds it mechanically from the prompt, then nudges the pane's Claude to replace
# the seed with a concise context-aware title. Kept in its own file;
# tmux-update-icons mirrors it to @window_ai_name, which build_window_label
# prefers over the raw task.
if [[ $state == "name" ]]; then
	action="${1:-}"
	shift || true
	text=""
	case "$action" in
	set)
		text="${1:-}"
		shift || true
		;;
	clear) ;;
	*)
		echo "Error: Invalid name action '$action'. Use: set, clear" >&2
		exit 1
		;;
	esac
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--pane)
			pane_id="$2"
			shift 2
			;;
		*) shift ;;
		esac
	done
	[[ -z $pane_id ]] && exit 0
	names_file="$NAMES_DIR/${pane_id#%}"
	case "$action" in
	set)
		# One line, no control chars, trimmed, capped at 40 cells — the value
		# becomes a window name segment. '|' is mapped to a space: it's the
		# delimiter of the list-panes format tmux-update-icons reads. Byte-oriented
		# tr -d '[:cntrl:]' (not keep [:print:]) preserves UTF-8.
		clean=$(printf '%s' "$text" | tr '\n\r\t|' '    ' | tr -d '[:cntrl:]' | tr -s ' ')
		clean="${clean# }"
		# Strip leading decoration: when seeded from a quoted/pasted first prompt the
		# text carries a U+258E "▎" quote-bar, a leading quote, or a markdown marker
		# (> - # *). Drop the leading non-alphanumeric run so the title starts at the
		# first real word. C-locale [:alnum:] is byte-oriented, so the multibyte ▎
		# (e2 96 8e) is non-alnum and stripped whole; interior chars (#2143) survive.
		clean=$(printf '%s' "$clean" | LC_ALL=C sed -E 's/^[^[:alnum:]]+//')
		clean="${clean:0:40}"
		clean="${clean% }"
		if [[ -n $clean ]]; then
			mkdir -p "$NAMES_DIR"
			printf '%s\n' "$clean" >"$names_file"
		fi
		;;
	clear)
		rm -f "$names_file"
		;;
	esac
	name_now=""
	[[ -f $names_file ]] && IFS= read -r name_now <"$names_file" || true
	window_stamp @window_ai_name "$name_now"
	if [[ -n ${TMUX:-} ]]; then
		tmux refresh-client -S 2>/dev/null || true
	fi
	exit 0
fi

# Issue/PR re-stamp (#137): run right after creating an issue/PR mid-session
# (a session started on main, or a worktree made before the issue existed) so
# @issue_* catches up without waiting for the next branch transition. Silent
# no-op outside a tmux-og tmux — there is no window to stamp. Delegates the
# actual write to tmux-issue-stamp (on PATH via the tmux wrapper), which
# serializes through its own per-window lock alongside post-switch and the
# auto branch-transition trigger.
if [[ $state == "enrich" ]]; then
	explicit_id=""
	if [[ ${1:-} != --* ]]; then
		explicit_id="${1:-}"
		shift || true
	fi
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--pane)
			pane_id="$2"
			shift 2
			;;
		*) shift ;;
		esac
	done
	if [[ -n $explicit_id && ! $explicit_id =~ ^[A-Za-z0-9_-]+$ ]]; then
		echo "Error: Invalid issue id '$explicit_id' (allowed: A-Z a-z 0-9 _ -)" >&2
		exit 1
	fi
	[[ -z ${TMUX:-} || -z $pane_id ]] && exit 0
	command -v tmux-issue-stamp >/dev/null 2>&1 || exit 0
	target="%${pane_id#%}"
	cwd="$(tmux display-message -t "$target" -p '#{pane_current_path}' 2>/dev/null)" || exit 0
	[[ -z $cwd ]] && exit 0
	git -C "$cwd" rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0
	worktree="$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null)" || exit 0
	branch="$(git -C "$cwd" branch --show-current 2>/dev/null)" || branch=""
	[[ -z $branch ]] && exit 0
	tmux-issue-stamp "$target" "$worktree" "$branch" "$explicit_id" >/dev/null 2>&1 &
	# Instant stamps can finish before disown; under set -e that exits non-zero (#207).
	disown 2>/dev/null || true
	exit 0
fi

while [[ $# -gt 0 ]]; do
	case "$1" in
	--pane)
		pane_id="$2"
		shift 2
		;;
	--session)
		session_name="$2"
		shift 2
		;;
	--window)
		win_target="$2"
		shift 2
		;;
	--force)
		force=1
		shift
		;;
	--transcript)
		transcript_path="$2"
		shift 2
		;;
	--*)
		echo "Error: Unknown option '$1'" >&2
		exit 1
		;;
	*)
		shift
		;;
	esac
done

# Validate state
case "$state" in
processing | waiting | done | idle | compacting | error | denied | clear | cleanup | mark-seen) ;;
*)
	echo "Error: Invalid state '$state'. Use: processing, waiting, done, idle, compacting, denied, clear, cleanup, mark-seen" >&2
	exit 1
	;;
esac

# Handle cleanup command (removes stale pane entries)
if [[ $state == "cleanup" ]]; then
	cleanup_stale_panes
	# Force immediate tmux refresh
	if [[ -n ${TMUX:-} ]]; then
		tmux refresh-client -S 2>/dev/null || true
	fi
	exit 0
fi

# Handle mark-seen: clear unseen flag for all panes in the active window.
# Called by tmux hooks on window/session switch.
# Usage: claude-status-update mark-seen --session <name> --window <index>
if [[ $state == "mark-seen" ]]; then
	[[ -n $session_name && -n $win_target ]] || exit 0
	[[ -d $PANES_DIR ]] || exit 0
	# Get pane IDs in the target window
	declare -A target_panes
	while IFS= read -r pid; do
		target_panes["${pid#%}"]=1
	done < <(tmux list-panes -t "${session_name}:${win_target}" -F '#{pane_id}' 2>/dev/null)
	# Remove unseen flag from matching pane files
	changed=false
	for pf in "$PANES_DIR"/*; do
		[[ -f $pf ]] || continue
		pane_file="${pf##*/}"
		[[ -n ${target_panes[$pane_file]+x} ]] || continue
		grep -q '^unseen=1$' "$pf" || continue
		# Rewrite without the unseen line
		grep -v '^unseen=' "$pf" >"${pf}.tmp" && mv "${pf}.tmp" "$pf"
		changed=true
	done
	if $changed && [[ -n ${TMUX:-} ]]; then
		tmux refresh-client -S 2>/dev/null || true
	fi
	exit 0
fi

# If no pane_id, silently exit (common when called from non-tmux context)
if [[ -z $pane_id ]]; then
	exit 0
fi

# Get session name and server pid from tmux if not provided. Pid first in the
# combined query: a session name may itself contain '|', and read's
# last-variable-absorbs-remainder semantics only protect the LAST field.
# <<<"$(... || true)" (not < <(... || true)): the herestring always supplies a
# trailing newline, so read returns 0 even on empty tmux output under set -e.
server_pid=""
if [[ -z $session_name ]] && command -v tmux &>/dev/null; then
	IFS='|' read -r server_pid session_name <<<"$(tmux display-message -p -t "$pane_id" '#{pid}|#{session_name}' 2>/dev/null || true)"
elif [[ -n $session_name ]] && command -v tmux &>/dev/null; then
	server_pid=$(tmux display-message -p -t "$pane_id" '#{pid}' 2>/dev/null || true)
fi

# Clean pane_id for filename (remove % prefix if present)
pane_file="${pane_id#%}"

# Handle clear state (cleanup)
if [[ $state == "clear" ]]; then
	claude_progress_emit "$pane_id" clear
	rm -f "$PANES_DIR/$pane_file" "$ISSUES_DIR/$pane_file" "$TASKS_DIR/$pane_file" "$NAMES_DIR/$pane_file" "$SCREEN_DIR/$pane_file"
	bridge_stamp @claude_status ""
	exit 0
fi

# Don't let a routine "processing"/"done" write clobber a genuine failure.
# Only `error` (StopFailure) is protected: the turn ended, the agent is stopped,
# so any later processing/done is stale noise — keep the error icon up until the
# next prompt clears it (--force).
#
# `waiting`/`denied` are deliberately NOT protected. A permission or elicitation
# dialog fully blocks the turn — no tool hook fires while it's pending — so the
# first processing/done write after one of them is always the genuine resume
# (PostToolUse after approval, ElicitationResult, or Stop). Protecting them froze
# the clock glyph for the rest of the session even though the agent was working.
#
# --force bypasses this guard (used by UserPromptSubmit: a new user prompt is an
# explicit reset signal and must clear any stale terminal state).
if [[ $force -eq 0 ]] && [[ $state == "processing" || $state == "done" ]] && [[ -f "$PANES_DIR/$pane_file" ]]; then
	cur_state=""
	while IFS='=' read -r key val; do
		[[ $key == "state" ]] && {
			cur_state="$val"
			break
		}
	done <"$PANES_DIR/$pane_file"
	case "$cur_state" in
	error) exit 0 ;;
	esac
fi

# For terminal states (done/error/waiting), check if the pane's window is
# currently focused. If not, mark as unseen so the status bar can show an
# attention indicator that persists through staleness dimming.
unseen_line=""
case "$state" in
done | error | waiting | denied)
	win_active=$(tmux display-message -t "$pane_id" -p '#{window_active}' 2>/dev/null) || win_active="1"
	[[ $win_active == "0" ]] && unseen_line=$'\n'"unseen=1"
	;;
esac

# Prior state now drives two consumers — the debug transition log and the
# notification gate below — so the read is unconditional. Still fork-free: the
# same `while IFS='=' read` loop, no grep, no $(…). An ABSENT file reads back as
# the literal sentinel `none`, which counts as a transition on purpose: a fresh
# session's first `waiting` is a genuine event (unlike a first PR stamp, which
# is only the poller learning what already existed).
prior_state="none"
if [[ -f "$PANES_DIR/$pane_file" ]]; then
	while IFS='=' read -r _k _v; do
		[[ $_k == state ]] && {
			prior_state="$_v"
			break
		}
	done <"$PANES_DIR/$pane_file"
fi

# Log only real transitions (from != to). Pre/PostToolUse both fire "processing"
# on every tool call, so logging every write would flood. The tmux id lookups
# still happen only when debug is armed.
if log_enabled && [[ $prior_state != "$state" ]]; then
	win_id=$(tmux display-message -t "$pane_id" -p '#{window_id}' 2>/dev/null || true)
	win_idx=$(tmux display-message -t "$pane_id" -p '#{window_index}' 2>/dev/null || true)
	log_event claude event transition from "$prior_state" to "$state" \
		pane "$pane_id" win_id "$win_id" win "$win_idx" sess "$session_name"
fi

# Transcript path powers interrupt detection (read_pane_state tails it). Every
# state hook delivers it on stdin; preserve the stored one when a write doesn't,
# so a single missing value can't blind the detector mid-session.
if [[ -z $transcript_path && -f "$PANES_DIR/$pane_file" ]]; then
	while IFS='=' read -r key val; do
		[[ $key == "transcript" ]] && {
			transcript_path="$val"
			break
		}
	done <"$PANES_DIR/$pane_file"
fi
transcript_line=""
[[ -n $transcript_path ]] && transcript_line=$'\n'"transcript=$transcript_path"

# Owning tmux server's pid: lets claude_prune_stale_state protect this file
# from another server's boot. Omitted when unresolved.
server_line=""
[[ -n $server_pid ]] && server_line=$'\n'"server=$server_pid"

# Write pane state with timestamp. A passive idle write (idle_prompt, resume)
# preserves the prior timestamp so the "last active" label reflects real work.
printf -v _now '%(%s)T' -1
ts=$_now
if [[ $state == idle && -f "$PANES_DIR/$pane_file" ]]; then
	while IFS='=' read -r key val; do
		[[ $key == "timestamp" && -n $val ]] && {
			ts="$val"
			break
		}
	done <"$PANES_DIR/$pane_file"
fi
cat >"$PANES_DIR/$pane_file" <<EOF
state=$state
timestamp=$ts
session=$session_name${server_line}${unseen_line}${transcript_line}
EOF

# Space-separated triple (state, epoch, unseen) — all three are token-safe, so a
# reader can split on whitespace and the '|' row delimiter stays free for the
# free-form task. See bridge_stamp.
bridge_stamp @claude_status "$state $ts ${unseen_line:+1}"

case "$state" in
processing | compacting) claude_progress_emit "$pane_id" "$state" ;;
*) claude_progress_emit "$pane_id" clear ;;
esac

# Notify on a real transition into an attention state, and only there. The gate
# is `prior != new`, not `new ∈ {waiting,error,denied}`, so the repeated
# `waiting` writes a stalled dialog produces never re-notify. `done` is
# deliberately excluded: it is the highest-frequency terminal state (every turn
# ends in one), the window glyph and the unseen flag already cover it, and
# notifying on it would train the user to ignore notifications. Fires AFTER the
# write, so a notification can never describe a state that failed to persist.
# The router resolves the pane to its window itself, so this adds zero tmux
# calls; detach makes it fire-and-forget.
if [[ $prior_state != "$state" && $NOTIFY_BIN != @* ]]; then
	notify_level=""
	case "$state" in
	waiting) notify_level="info" ;;
	denied) notify_level="warn" ;;
	error) notify_level="error" ;;
	esac
	if [[ -n $notify_level ]]; then
		detach "$NOTIFY_BIN" emit --source claude --level "$notify_level" \
			--pane "$pane_id" --title "claude: $state" --body "$prior_state → $state"
	fi
fi
