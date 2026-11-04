#!/usr/bin/env bats
bats_require_minimum_version 1.5.0
# Dispatcher "standing by" overlay (#1083): a dispatcher window (raw @crew_name
# dispatcher, @window_has_agent 1) whose state is idle/done while a live worker
# exists renders the standby glyph, and tmux-update-icons publishes the global
# @og_dispatchers token list. The flake sandbox's querying client is non-UTF-8
# (glyphs read back as `_`), so the test's copy of lib-claude.sh swaps the
# idle/done/standby glyphs for ASCII tokens.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$TDIR/data"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen"
	export TMPDIR="$TDIR"
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))
	export AGENT_COMMANDS="claude"

	FAKE_REFLOW="$TDIR/fake-reflow"
	printf '#!/bin/sh\nexit 0\n' >"$FAKE_REFLOW"
	chmod +x "$FAKE_REFLOW"

	LIB_CLAUDE="$TDIR/lib-claude.sh"
	sed \
		-e 's/^CLAUDE_ICON_STANDBY=.*/CLAUDE_ICON_STANDBY="SB"/' \
		-e 's/^CLAUDE_ICON_IDLE=.*/CLAUDE_ICON_IDLE="ID"/' \
		-e 's/^CLAUDE_ICON_DONE=.*/CLAUDE_ICON_DONE="DN"/' \
		scripts/lib-claude.sh >"$LIB_CLAUDE"

	UPDATE_ICONS="$TDIR/update-icons.sh"
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@/["claude"]="C"/' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$LIB_CLAUDE|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e "s|@MAX_ICONS@|2|g" \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	REPO="$TDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" branch -q -M main

	# pane_current_command is the executable basename: a bash copy named
	# `claude` is the default shell for agent windows. Sessions are created
	# first, while the default shell is still non-agent.
	mkdir -p "$TDIR/bin"
	cp -L "$(command -v bash)" "$TDIR/bin/claude"
	chmod +x "$TDIR/bin/claude"

	tmux -f /dev/null new-session -d -s S1 -c "$REPO" -x 200 -y 50
	tmux new-session -d -s S2 -c "$REPO" -x 200 -y 50
	tmux set -g default-shell "$TDIR/bin/claude"
	tmux set -g default-command ''
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

# mkwin SESSION CREW_NAME CREW_ID HAS_AGENT STATE [CMD]
# Creates a window (a `claude` shell unless CMD is given) with the crew
# options and a fresh unseen pane state file; sets WID. Empty arguments leave
# the option unset / write no state file.
mkwin() {
	local sess="$1" name="$2" cid="$3" has="$4" state="$5" cmd="${6:-}" out pane
	if [[ -n $cmd ]]; then
		out=$(tmux new-window -d -P -F '#{window_id}|#{pane_id}' -t "$sess:" -c "$REPO" "$cmd")
	else
		out=$(tmux new-window -d -P -F '#{window_id}|#{pane_id}' -t "$sess:" -c "$REPO")
	fi
	WID="${out%%|*}"
	pane="${out##*|}"
	[[ -z $name ]] || tmux set -w -t "$WID" @crew_name "$name"
	[[ -z $cid ]] || tmux set -w -t "$WID" @crew_id "$cid"
	[[ -z $has ]] || tmux set -w -t "$WID" @window_has_agent "$has"
	if [[ -n $state ]]; then
		printf 'state=%s\ntimestamp=%s\nunseen=1\nsession=x\n' "$state" "$CLAUDE_NOW" \
			>"$CLAUDE_STATUS_DIR/panes/${pane#%}"
	fi
}

tick() {
	run bash "$UPDATE_ICONS" S1
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}
}

display_of() {
	tmux show -wv -t "$1" @window_icon_display 2>/dev/null || true
}

dispatchers_opt() {
	tmux show -gv @og_dispatchers 2>/dev/null || true
}

@test "idle dispatcher with a same-session live worker shows standby" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	mkwin S1 amber w1 1 processing
	tick
	[[ "$(display_of "$d")" == *SB* ]]
	[[ "$(display_of "$d")" != *ID* ]]
}

@test "done dispatcher with a live worker shows standby" {
	mkwin S1 dispatcher "" 1 "done"
	local d="$WID"
	mkwin S1 amber w1 1 processing
	tick
	[[ "$(display_of "$d")" == *SB* ]]
	[[ "$(display_of "$d")" != *DN* ]]
}

@test "idle dispatcher without a worker keeps the idle glyph" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	tick
	[[ "$(display_of "$d")" == *ID* ]]
	[[ "$(display_of "$d")" != *SB* ]]
}

@test "a worker without @window_has_agent is not live" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	# non-agent command: the render loop re-stamps has_agent from it
	mkwin S1 amber w1 "" "" "sleep 600"
	tick
	[[ "$(display_of "$d")" == *ID* ]]
	[[ "$(display_of "$d")" != *SB* ]]
}

@test "a worker without @crew_id is not a worker" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	mkwin S1 amber "" 1 processing
	tick
	[[ "$(display_of "$d")" == *ID* ]]
}

@test "another dispatcher is not a worker" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	mkwin S1 dispatcher w1 1 idle
	tick
	[[ "$(display_of "$d")" == *ID* ]]
}

@test "a dispatcher with @crew_id joins workers by id across sessions" {
	mkwin S1 dispatcher c1 1 idle
	local d="$WID"
	mkwin S2 amber c1 1 processing
	tick
	[[ "$(display_of "$d")" == *SB* ]]
}

@test "a dispatcher with @crew_id ignores a same-session worker with another id" {
	mkwin S1 dispatcher c1 1 idle
	local d="$WID"
	mkwin S1 amber c2 1 processing
	tick
	[[ "$(display_of "$d")" == *ID* ]]
	[[ "$(display_of "$d")" != *SB* ]]
}

@test "a bridge-mirror worker does not count" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	mkwin S1 amber w1 1 processing
	tmux set -w -t "$WID" @bridge_win 1
	tick
	[[ "$(display_of "$d")" == *ID* ]]
	[[ "$(display_of "$d")" != *SB* ]]
}

@test "a bridge-mirror dispatcher is ignored" {
	mkwin S1 dispatcher "" 1 idle
	local d="$WID"
	tmux set -w -t "$d" @bridge_win 1
	mkwin S1 amber w1 1 processing
	tick
	[[ "$(display_of "$d")" != *SB* ]]
	[ -z "$(dispatchers_opt)" ]
}

@test "a processing dispatcher keeps its processing glyph" {
	mkwin S1 dispatcher "" 1 processing
	local d="$WID"
	mkwin S1 amber w1 1 processing
	tick
	[[ "$(display_of "$d")" != *SB* ]]
	[[ "$(display_of "$d")" == *"#[fg=#94e2d5]"* ]]
	[ "$(dispatchers_opt)" = "processing" ]
}

@test "@og_dispatchers lists tokens in fixed order and clears at zero" {
	mkwin S1 dispatcher "" 1 idle
	local standby="$WID"
	mkwin S1 amber w1 1 processing
	mkwin S2 dispatcher "" 1 waiting
	local waiting="$WID"
	mkwin S2 dispatcher "" 1 idle
	local idle="$WID"
	# has_agent but no pane state counts as idle
	mkwin S2 dispatcher "" 1 ""
	local bare="$WID"
	tick
	[ "$(dispatchers_opt)" = "waiting standby idle idle" ]

	tmux kill-window -t "$standby"
	tmux kill-window -t "$waiting"
	tmux kill-window -t "$idle"
	tmux kill-window -t "$bare"
	tick
	[ -z "$(dispatchers_opt)" ]
}
