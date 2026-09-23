#!/usr/bin/env bash
# Coding-agent usage-limit poller. Two entry modes:
#   --tick      cheap gate from status-format[0]; daemonizes a pass when stale
#   --tick-run  one pass: refresh every OPEN, authed agent's cache concurrently
# Always exits 0. Cache: /tmp/og-agent-usage/<agent>.json, rendered by
# tmux-statusline (Go). Providers curl the usage endpoints with the CLIs' own
# stored tokens — no extra API keys.
#
# scan_open_agents populates OPEN[cmd]=1 per manifest-command pane seen, from
# one `list-panes -a` call. Tick mode gates on "any agent open at all"
# (unchanged semantics: the display gate (tmux-statusline) hides the segment
# when nothing's open, so polling would burn provider quota for an invisible
# segment), and that gate precedes the .last-tick stamp — a gated-out tick
# that spent the cycle would leave the segment on the previous session's
# numbers for a second refresh window after an agent starts. Tick-run
# additionally gates each provider fork on its OWN agent being in OPEN, and
# clears the cache of any agent that's NOT open — otherwise a closed-then-
# reopened agent would show its last session's numbers until the next
# refresh.
set -uo pipefail

# shellcheck source=/dev/null
source @lib_log@

CACHE_DIR="${OG_AGENT_USAGE_DIR:-/tmp/og-agent-usage}"
REFRESH_SECONDS="@refresh_seconds@"
# Space-separated pane-command basenames from the agentdetect manifests
# (claude codex cursor-agent pi) — same source as the update-icons sweep.
AGENT_COMMANDS="@AGENT_COMMANDS@"

declare -gA OPEN=()

# scan_open_agents: populates the global OPEN[cmd]=1 map, one entry per
# manifest-command basename with a pane open somewhere. One `list-panes`
# fork, reached only past the refresh window in tick mode. A failed
# `list-panes` leaves OPEN empty — callers must not read that as "every
# agent closed" (see the stale-cache guard in --tick-run below).
scan_open_agents() {
	local cmds cmd base agent
	cmds=$(tmux list-panes -a -F '#{pane_current_command}' 2>/dev/null) || return
	while IFS= read -r cmd; do
		base=${cmd##*/}
		base=${base#.}
		base=${base%-wrapped}
		for agent in $AGENT_COMMANDS; do
			[[ $base == "$agent" ]] && OPEN[$base]=1
		done
	done <<<"$cmds"
}

mode="tick"
[[ ${1:-} == "--tick-run" ]] && mode="tickrun"

if [[ $mode == "tick" ]]; then
	last_tick="$CACHE_DIR/.last-tick"
	if [[ -f $last_tick ]] && ((EPOCHSECONDS - $(file_mtime "$last_tick") < REFRESH_SECONDS)); then
		exit 0
	fi
	# Gate BEFORE the stamp: a tick with no agent must not spend the cycle, or
	# the first tick after an agent appears waits out another one and the segment
	# comes back showing the last agent session's numbers.
	scan_open_agents
	((${#OPEN[@]})) || exit 0
	# Mark fresh BEFORE daemonizing (same best-effort trade as tmux-pr-enrich):
	# a crashed pass waits one cycle.
	mkdir -p "$CACHE_DIR" 2>/dev/null
	touch "$last_tick"
	detach "${BASH_SOURCE[0]}" --tick-run
	exit 0
fi

# --- tick-run ---
# Re-checked here because --tick-run is its own entry point, and an agent can
# exit between the tick's gate and the detached pass.
scan_open_agents
((${#OPEN[@]})) || exit 0
mkdir -p "$CACHE_DIR" 2>/dev/null

# Stale-cache-on-reopen: per-agent gating below would otherwise reintroduce,
# at agent granularity, the exact bug the gate-before-stamp ordering exists
# to prevent — an agent that was closed and just reopened would show its
# LAST session's numbers until the next refresh window, because its cache
# file was never touched while closed. Clear the cache of every manifest
# command that's NOT currently open. This runs only past the OPEN early-exit
# above, so a failed `list-panes` (OPEN empty) never reaches here and can't
# be misread as "every agent closed".
for agent in $AGENT_COMMANDS; do
	[[ -v OPEN[$agent] ]] && continue
	cache_key=$agent
	[[ $agent == "cursor-agent" ]] && cache_key="cursor"
	rm -f "$CACHE_DIR/$cache_key.json"
done

# Per-provider lock: two overlapping passes (stale .last-tick race) otherwise
# curl the same endpoint twice; the atomic cache write makes the loser harmless.
pids=()
[[ -f $HOME/.claude/.credentials.json && -v OPEN[claude] ]] && (
	acquire_lock "$CACHE_DIR/.lock-claude" || exit 0
	"@usage_claude@"
) &
pids+=($!)
[[ -f $HOME/.codex/auth.json && -v OPEN[codex] ]] && (
	acquire_lock "$CACHE_DIR/.lock-codex" || exit 0
	"@usage_codex@"
) &
pids+=($!)
[[ -f $HOME/.config/cursor/auth.json && -v OPEN[cursor-agent] ]] && (
	acquire_lock "$CACHE_DIR/.lock-cursor" || exit 0
	"@usage_cursor@"
) &
pids+=($!)
[[ (-f "$HOME/.pi/agent/auth.json" || -n ${OPENROUTER_API_KEY:-}) && -v OPEN[pi] ]] && (
	acquire_lock "$CACHE_DIR/.lock-pi" || exit 0
	"@usage_pi@"
) &
pids+=($!)
((${#pids[@]})) && wait "${pids[@]}" 2>/dev/null
exit 0
