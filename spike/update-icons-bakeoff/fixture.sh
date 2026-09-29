#!/usr/bin/env bash
# Scratch tmux server for the update-icons bake-off. Never touches the live
# server: own TMUX_TMPDIR, own socket (-L probe), own CLAUDE_STATUS_DIR.
#
#   fixture.sh up ROOT        build the fixture under ROOT, write ROOT/env
#   fixture.sh snapshot ROOT  print every window/session option, sorted
#   fixture.sh down ROOT      kill the server and remove ROOT
#
# `up` leaves the icon options unstamped (the first variant run writes them)
# and every git window's @branch seeded, so a run exercises the write path but
# not the branch-transition path the bake-off does not cover.
set -euo pipefail

# shellcheck source=/dev/null  # sibling helper
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

cmd="${1:?usage: fixture.sh up|snapshot|down ROOT}"
ROOT="${2:?usage: fixture.sh up|snapshot|down ROOT}"

unset TMUX
export TMUX_TMPDIR="$ROOT/run"
# GIT_CONFIG_GLOBAL/SYSTEM off: the host gitconfig must not change fixture commits.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

tm() { "$ROOT/bin/tmux" -L probe "$@"; }

git_quiet() { git -c user.name=bakeoff -c user.email=bakeoff@example.invalid -c commit.gpgsign=false "$@" >/dev/null; }

# proc_cmd NAME -- a long-lived pane whose foreground command is reported as NAME.
# bash's own read builtin idles it: coreutils is one multicall binary that
# dispatches on argv[0], so `exec -a NAME sleep` would just fail.
proc_cmd() { printf 'exec -a %s bash -c "read -t 1000000 x"' "$1"; }

# seed_git WINDOW REPO BRANCH -- what the reconciler would already have stamped.
seed_git() {
	tm set -w -t "$1" @branch "$3"
	tm set -w -t "$1" @git_root "$2"
	tm set -w -t "$1" @worktree "$2"
	tm set -w -t "$1" @window_cwd_seen "$2"
}

build_repos() {
	local r
	mkdir -p "$ROOT/repos" "$ROOT/nogit/n1" "$ROOT/nogit/n2"
	for r in r1 r2 r3 r4; do
		git_quiet init -q -b main "$ROOT/repos/$r"
		git_quiet -C "$ROOT/repos/$r" commit -q --allow-empty -m init
	done
	git_quiet -C "$ROOT/repos/r2" checkout -q -b feat/x
	git_quiet -C "$ROOT/repos/r1" worktree add -q -b wt-branch "$ROOT/repos/r1-wt"
	git_quiet -C "$ROOT/repos/r4" checkout -q --detach
}

build_server() {
	local R="$ROOT/repos" N="$ROOT/nogit"
	mkdir -p "$ROOT/bin"
	ln -sf "$OG_RAW_TMUX" "$ROOT/bin/tmux"
	# `exec -a` is bash-only and the pane command runs under $SHELL (fish here).
	SHELL="$(command -v bash)" tm -f /dev/null new-session -d -s alpha -x 200 -y 50 -c "$R/r1" "$(proc_cmd nvim)"
	tm new-window -d -t alpha: -c "$R/r2" "$(proc_cmd claude)"
	tm split-window -d -t alpha:1 -c "$R/r2" "$(proc_cmd git)"
	tm new-window -d -t alpha: -c "$N/n1" "$(proc_cmd htop)"
	tm new-window -d -t alpha: -c "$R/r1-wt" "$(proc_cmd lazygit)"
	tm split-window -d -t alpha:3 -c "$R/r1-wt" "$(proc_cmd zsh)"
	tm new-window -d -t alpha: -c "$R/r4" "$(proc_cmd yazi)"

	tm new-session -d -s beta -x 200 -y 50 -c "$R/r3" "$(proc_cmd nvim)"
	tm split-window -d -t beta:0 -c "$R/r3" "$(proc_cmd cargo)"
	tm split-window -d -t beta:0 -c "$R/r3" "$(proc_cmd claude)"
	tm new-window -d -t beta: -c "$N/n2" "$(proc_cmd codex)"
	tm new-window -d -t beta: -c "$R/r2" "$(proc_cmd notaknownproc)"

	tm new-session -d -s gamma -x 200 -y 50 -c "$R/r1" "$(proc_cmd python3)"
	tm new-window -d -t gamma: -c "$R/r2" "$(proc_cmd .claude-wrapped)"
}

# Cached branch state the reconciler would have stamped, plus stale state each
# write path of the steady-state loop has to repair.
seed_options() {
	local R="$ROOT/repos"
	seed_git alpha:0 "$R/r1" main
	seed_git alpha:1 "$R/r2" feat/x
	seed_git alpha:3 "$R/r1-wt" wt-branch
	seed_git beta:0 "$R/r3" main
	seed_git beta:2 "$R/r2" feat/x
	seed_git gamma:0 "$R/r1" main
	seed_git gamma:1 "$R/r2" feat/x

	tm set -w -t alpha:3 automatic-rename off
	tm set -w -t beta:2 automatic-rename off
	tm set -w -t beta:2 @window_manual_name 1
	tm set -w -t alpha:4 @window_task stale-task
	tm set -w -t alpha:4 @window_ai_name stale-name
	tm set -w -t alpha:2 @window_claude_ago 5m
	tm set -w -t gamma:0 @crew_name onyx
	tm set -t gamma @claude_session_fg '#123456'
}

# Trust marker + prune gate, so the shipped script's marker-gated
# claude_prune_stale_state returns immediately (its steady state).
seed_status_dir() {
	local pid start
	pid="$(tm display-message -p '#{pid}')"
	start="$(tm display-message -p '#{start_time}')"
	mkdir -m 700 "$ROOT/status"
	mkdir "$ROOT/status"/{panes,screen,tasks,names,issues,live}
	: >"$ROOT/status/.owner-only"
	printf '%s\n' "$start" >"$ROOT/status/.server_start"
	printf '%s\n' "$start" >"$ROOT/status/.server_start.$pid"
	{
		printf 'export TMUX_TMPDIR=%q\n' "$TMUX_TMPDIR"
		printf 'export TMUX=%q\n' "$(tm display-message -p '#{socket_path}'),$pid,0"
		printf 'export CLAUDE_STATUS_DIR=%q\n' "$ROOT/status"
		# shellcheck disable=SC2016  # $PATH must expand when env is sourced
		printf 'export PATH=%q:"$PATH"\n' "$ROOT/bin"
		# Test seams the shipped script reads from the environment: pin the 5 s
		# arming sweep off (CLAUDE_NOW % 5 != 0) and disable every background
		# helper, so a run is exactly the 1 s window-naming path.
		printf 'export CLAUDE_NOW=1 RECONCILE_BIN=@none ISSUE_STAMP_BIN=@none REFLOW_BIN=@none AGENT_DETECT_BIN=@none\n'
		printf 'OG_ARGS=(alpha "" %q "" latte %q)\n' "$start" "$pid"
	} >"$ROOT/env"
}

snapshot() {
	local id
	tm list-windows -a -F '#{window_id}' | sort | while read -r id; do
		tm show-options -w -t "$id" | sed "s|^|W $id |"
	done
	tm list-sessions -F '#{session_id}' | sort | while read -r id; do
		tm show-options -t "$id" | sed "s|^|S $id |"
	done
}

case "$cmd" in
up)
	resolve_build
	rm -rf "$ROOT"
	mkdir -p "$ROOT/run"
	build_repos
	build_server
	seed_options
	seed_status_dir
	;;
snapshot)
	resolve_build
	snapshot
	;;
down)
	[[ -x $ROOT/bin/tmux ]] && tm kill-server 2>/dev/null || true
	rm -rf "$ROOT"
	;;
*)
	echo "usage: fixture.sh up|snapshot|down ROOT" >&2
	exit 2
	;;
esac
