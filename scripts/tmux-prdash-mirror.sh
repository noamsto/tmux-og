#!/usr/bin/env bash
# Run prdash LOCALLY for a mirror window and forward its hand-offs to the remote.
#
#   tmux-prdash-mirror.sh <prdash-bin> <ctl-bin>
#
# The command of a local float pane opened inside a mirror window, so $TMUX_PANE
# is the float. The script reads its context from tmux itself: `new-pane`'s
# shell-command and its `-e` values are NOT format-expanded (measured on tmux
# next-3.9: `-e "R=#{@bridge_repo}"` arrives literally). The bind therefore stashes
# the pressed remote pane in the window option @og_prdash_origin and this script
# resolves it, with @bridge_repo/@bridge_dir (window) and @bridge_sock (session,
# the RAW socket path; --sock= is added here).
#
# prdash runs in a private scratch dir with an empty-DIRECTORY repos root (an
# empty string would fall back to ~/git), so it finds no local checkout and
# writes only path-free hand-off entries. Its action file is then forwarded in
# ONE ctl call: a bulk `W` must run sequentially in a single remote float. The
# daemon caps a hand-off at 32 entries; a larger one is refused here rather than
# forwarded partially. A prdash that exits non-zero forwards nothing and says so;
# so does a ctl that fails (ctl exits 0 once it has shown its own error banner).
#
# An invalid @bridge_repo falls back to the remote prdash (ctl `tool`). An
# invalid pane id (never a remote pane) only notifies.

set -uo pipefail

prdash=$1
ctl=$2

# The socket is last: its path may hold '|'; the dir cannot (the remote label
# format rewrites it and the daemon rejects whitespace).
IFS='|' read -r pane repo client dir sock < <(
	tmux display-message -p -t "$TMUX_PANE" \
		'#{@og_prdash_origin}|#{@bridge_repo}|#{client_name}|#{@bridge_dir}|#{@bridge_sock}'
)

if [[ ! $pane =~ ^%[0-9]+$ ]]; then
	tmux display-message "prdash: no remote pane to hand off to"
	exit 0
fi

name=${repo#*/}
if [[ ! $repo =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]+$ ]] ||
	[[ $name == . || $name == .. ]]; then
	exec "$ctl" --display-error="$client" --sock="$sock" tool "$pane" prdash "$dir"
fi

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/root"
cd "$work" || exit 1

PRDASH_ACTION_FILE="$work/handoff" PRDASH_REPOS_ROOT="$work/root" "$prdash" --repo "$repo"
status=$?
if ((status != 0)); then
	tmux display-message -d 5000 "prdash: exited with status $status; nothing was handed off"
	exit 0
fi

[[ -s $work/handoff ]] || exit 0
mapfile -t lines <"$work/handoff"

if ((${#lines[@]} > 32)); then
	tmux display-message -d 5000 "prdash: ${#lines[@]} hand-offs exceed the bridge's 32-entry cap; nothing was sent"
	exit 0
fi

"$ctl" --display-error="$client" --sock="$sock" handoff "$pane" "$dir" "${lines[@]}" 2>"$work/err"
status=$?
if ((status != 0)); then
	# ctl's error can quote a PR branch name. display-message expands `#{...}`
	# formats and strftime `%` sequences (and an older resident server, #407, may
	# still run `#(...)`; the pinned next-3.9 does not), so double `#` and `%`.
	err=$(tail -n 1 "$work/err")
	err=${err//#/##}
	err=${err//'%'/%%}
	tmux display-message -d 5000 "prdash: hand-off failed: ${err:-exit status $status}"
fi
