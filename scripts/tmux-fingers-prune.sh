#!/usr/bin/env bash
# Invoked once per config load with #{pid}: removes tmux-fingers per-server
# state dirs (tmux-<pid>/) whose server is gone. Fingers writes one on every
# load and never removes it (#1066).
set -euo pipefail

# Guarded so the RAW script still runs under bats, where @lib_claude@ is not
# substituted (same pattern and reason as tmux-reap-pane.sh).
# shellcheck source=/dev/null
if [[ -f "@lib_claude@" ]]; then
	source "@lib_claude@"
else
	exit 0
fi

live_pid=${1:-}
data_dir="${XDG_DATA_HOME:-$HOME/.local/share}/tmux-fingers"
stamp="$data_dir/.prune-stamp"

[[ -d $data_dir && -w $data_dir ]] || exit 0

# Rate limit: one sweep per 10 minutes across every server sharing the dir.
[[ -z $(find "$stamp" -mmin -10 2>/dev/null) ]] || exit 0
touch "$stamp"

shopt -s nullglob
for dir in "$data_dir"/tmux-*/; do
	dir=${dir%/}
	pid=${dir##*/tmux-}
	[[ $pid =~ ^[0-9]+$ ]] || continue
	[[ $pid == "$live_pid" ]] && continue
	claude_pid_is_tmux "$pid" && continue
	rm -rf -- "$dir"
done
