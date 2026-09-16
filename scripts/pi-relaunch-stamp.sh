#!/usr/bin/env bash
# pi session relaunch stamper: stamp this pane's @remux_relaunch so tmux-remux
# resumes the pi session (not a bare shell) on restore. Invoked by the
# pi-relaunch-stamp.ts extension on session_start and turn_end with
# <session-file> <pi-argv...> (the extension forwards pi's real argv as args,
# so spaces/quotes/$ round-trip without any encoding).
#
# The stamped command replays pi's original flags with per-arg single-quote
# shell quoting, minus the positional launch prompt (never re-sent on restore),
# the session-selection flags (the --session <file> we append replaces them)
# and --api-key (the credential would be persisted in the pane option,
# tmux-remux's state.db and the restored pane's argv — a keyed launch restores
# through the provider's env var instead). @remux_relaunch is emitted verbatim
# by tmux-remux into the restored pane's startup command and run through the
# user's default-shell — fish included — so the value must be valid for all of
# them: every argument is single-quoted with the standard '\'' trick, and the
# value is rejected whole (exit 0, no stamp) if it contains '|' (break the
# | -delimited list-panes -F read-back in tmux-update-icons) or any control
# byte (C0 + DEL: tab/newline/CR/VT/FF/… — all mangled by tmux format output).
# Degrade to a bare-shell restore rather than stamp a broken or exploitable
# command, mirroring codex/cursor.
set -euo pipefail

[[ -n ${TMUX_PANE:-} ]] || exit 0
command -v tmux >/dev/null 2>&1 || exit 0

# shellQuoteSingle — the same '\'' trick tmux-remux's own restore helper uses.
quote() {
	local out="'" s="$1"
	while [[ $s == *\'* ]]; do
		out+="${s%%\'*}'\\''"
		s="${s#*\'}"
	done
	out+="$s'"
	printf '%s' "$out"
}

# A dispatcher-launched worker's own pi flags still carry /nix/store paths
# (--append-system-prompt <protocol dir>/WORKER_PROTOCOL.md, and any -e files
# dispatch supplies) that go stale exactly like the wrapper's, but dispatch
# ships its own resume path — `dispatch resume` — that re-resolves
# engine/model/effort and current protocol paths from the worktree's
# WORKER_TASK.md instead of replaying stale argv. The dispatcher tags
# CREW_WORKER_ID into this pane's environment at launch (`worker:<branch>#…`
# for a lead, `role:<branch>:<role>` for a role-grid pane sharing this
# worktree) and it is inherited straight through to this script, so it is the
# one reliable signal for "this pane is a dispatched worker" without guessing
# at pi's own argv. A lead gets the `dispatch resume` stamp (no store paths);
# a role pane restores as a bare shell — a documented limitation, since there
# is no "resume as role" verb and role panes must not each launch a second
# lead.
cmd=""
case "${CREW_WORKER_ID:-}" in
worker:*) cmd="dispatch resume" ;;
role:*) exit 0 ;;
esac

if [[ -z $cmd ]]; then
	[[ -n ${1:-} ]] || exit 0 # ephemeral (--no-session) — nothing to resume
	session_file="$1"
	shift

	# PI_USER_ARGC (set by the nix-config pi wrapper as $# before its `exec`,
	# the count of trailing argv entries that are the caller's own) drops the
	# wrapper-injected prefix (-e hook-bridge.ts --skill … --prompt-template
	# …) from the replay: those are store paths that go stale after a rebuild
	# once garbage-collected, and replaying them re-injects the CURRENT
	# wrapper's flags a second time on top. Keep only the last PI_USER_ARGC
	# entries of the argv the extension forwarded. Unset (no wrapper, or an
	# older one) or malformed (non-integer, a leading zero other than the
	# literal "0" — bash's own $# is never zero-padded, and a zero-padded
	# value would be misread as octal by the arithmetic below, silently
	# dropping real args — or larger than the available args) falls back to
	# today's replay-all — never crash, never stamp garbage.
	if [[ -n ${PI_USER_ARGC:-} && $PI_USER_ARGC =~ ^(0|[1-9][0-9]*)$ ]] && ((PI_USER_ARGC <= $#)); then
		((PI_USER_ARGC < $#)) && shift $(($# - PI_USER_ARGC))
	fi

	# Replay pi's argv: keep every non-positional flag, dropping the launch
	# prompt and any session-selection flags. Value-taking options consume
	# their next element even when it starts with '-'.
	replay=()
	while (($#)); do
		arg="$1"
		shift
		case "$arg" in
		--)
			# Everything after -- is positional by definition; drop it and stop.
			break
			;;
		--session | --session-id | --fork | --api-key)
			[[ -n ${1:-} ]] && shift # their value is stale or must not be persisted
			continue
			;;
		--continue | --resume | --no-session | -c | -r)
			continue
			;;
		--*=*)
			# Long option with attached value. Drop the session-selection and
			# secret-carrying forms; a kept value-taking option is self-contained,
			# so keep the whole token.
			case "${arg%%=*}" in
			--session | --session-id | --fork | --api-key | --continue | --resume | --no-session) continue ;;
			esac
			replay+=("$arg")
			continue
			;;
		--provider | --model | --system-prompt | --append-system-prompt | --mode | \
			--session-dir | --name | --models | --tools | --exclude-tools | --thinking | \
			--extension | --skill | --prompt-template | --theme | --use-theme | --export | --tui-mode | \
			-n | -t | -xt | -e)
			if (($#)); then
				replay+=("$arg" "$1")
				shift
			else
				replay+=("$arg") # dangling value: keep the flag, pi will report it
			fi
			continue
			;;
		-*)
			replay+=("$arg")
			continue
			;;
		*)
			continue # positional message / @file — the launch prompt, never re-sent
			;;
		esac
	done

	cmd="pi"
	for a in "${replay[@]}"; do
		cmd+=" $(quote "$a")"
	done
	cmd+=" --session $(quote "$session_file")"
fi

# Reject rather than sanitize: a literal | shifts update-icons'
# | -delimited read of @remux_relaunch, and any control byte — tab, newline,
# CR, vertical tab, form feed, 0x01-…, DEL — is mangled by tmux format output.
# Quoting does not help; the reader is raw. The cntrl class is C0 + DEL.
case "$cmd" in
*'|'*) exit 0 ;;
esac
[[ $cmd =~ [[:cntrl:]] ]] && exit 0

# Stamp only on change: a stable pane forks nothing per turn, and the sibs
# (cursor/codex) are the precedent for the silent best-effort set.
cur="$(tmux show-options -pqv -t "$TMUX_PANE" @remux_relaunch 2>/dev/null)" || cur=""
[[ $cmd == "$cur" ]] && exit 0
tmux set-option -p -t "$TMUX_PANE" @remux_relaunch "$cmd"
