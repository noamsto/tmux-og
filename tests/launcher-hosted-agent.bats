#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
bats_require_minimum_version 1.5.0
# Launcher-hosted agent occupancy (#1091): an agent hosted by a launcher shell
# (`bash …/dispatcher` loop, `claude` its child in the same foreground pgrp)
# reports `bash` as pane_current_command, so occupancy derived from the command
# alone read empty and the #692 sweep cleared the live agent's window title.
# Positive evidence is the hook-written panes/<id> `agent_pid=` line, live under
# `kill -0`, whose `server=` (when present) is this server's pid. Covers the
# reader side: claude_pane_hosts_agent, the arm_agent_detect sweep, the per-tick
# has_agent, and the raw tmux-shell-prompt.sh checks.
#
# Tests A-E, H and the helper units run the raw scripts against a fake `tmux`
# that logs every call and answers `list-panes` from a file; F, G and the sweep
# pass drive a real scratch tmux server on its own TMUX_TMPDIR/-L socket.

# --- fake-tmux harness (A-E, H, helper units, shell-prompt) -------------------

setup() {
	TDIR="$BATS_TEST_TMPDIR"
	FAKEBIN="$TDIR/bin"
	mkdir -p "$FAKEBIN"
	TLOG="$TDIR/tmux.log"
	: >"$TLOG"
	ROWS="$TDIR/rows"
	PANES_T="$TDIR/panes-t"
	: >"$ROWS"
	: >"$PANES_T"

	# list-panes -a answers the sweep's rows, `list-panes -t` the shell-prompt
	# hook's window scan; every other call is only logged.
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		echo "\$*" >>"$TLOG"
		case "\$*" in
		*"list-panes -a"*) cat "$ROWS" ;;
		*"list-panes -t"*) cat "$PANES_T" ;;
		*"display-message"*"#{@window_manual_name}"*) echo "" ;;
		*"display-message"*"#{pid}"*) echo 4242 ;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"

	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,names,watchers,live}
	export AGENT_DETECT_BIN="agent-detect"
	export AGENT_COMMANDS="claude codex"
	export CLAUDE_NOW=100
	unset CLAUDE_ASSUME_DEAD_AFTER REPAINT_BIN

	SRV=4242
	export TMUX="/x/sock,$SRV,0"
}

# pane_file ID [LINE...] — writes panes/<ID> with state=idle plus extra lines.
pane_file() {
	local id=$1
	shift
	{
		printf 'state=idle\ntimestamp=100\nsession=sess\n'
		[[ $# -eq 0 ]] || printf '%s\n' "$@"
	} >"$CLAUDE_STATUS_DIR/panes/$id"
}

# row PANE_ID CMD [BRIDGE] — one list-panes -a row of the 12 fields the sweep
# reads: pane_id|cmd|piped|wid|bridge|ha|manual|birth|seen|ai|task|session.
row() {
	printf '%s|%s|0|@3|%s||||||ai-title||sess\n' "$1" "$2" "${3:-}" >"$ROWS"
}

sweep() {
	run bash -c 'source scripts/lib-claude.sh; source scripts/tmux-update-icons.sh; arm_agent_detect force'
	[ "$status" -eq 0 ] || {
		echo "sweep exited $status: $output"
		false
	}
}

assert_title_kept() {
	if grep -q '^set .*@window_ai_name' "$TLOG"; then
		echo "@window_ai_name was written (title cleared); tmux log:"
		cat "$TLOG"
		false
	fi
}

assert_title_cleared() {
	grep -q '^set .*@window_ai_name' "$TLOG" || {
		echo "expected the @window_ai_name clear; tmux log:"
		cat "$TLOG"
		false
	}
	grep -q '@window_naming_dirty 1' "$TLOG" || {
		echo "expected @window_naming_dirty 1; tmux log:"
		cat "$TLOG"
		false
	}
}

dead_pid() {
	local p
	sleep 0 &
	p=$!
	wait "$p"
	printf '%s' "$p"
}

# --- helper units -------------------------------------------------------------

hosts() {
	run -0 bash -c 'source scripts/lib-claude.sh; claude_pane_hosts_agent "$@"; echo "rc=$?"' _ "$@"
	status=${output##*rc=}
}

@test "helper: no panes file is not hosting" {
	hosts %7 "$SRV"
	[ "$status" -eq 1 ]
}

@test "helper: a file without agent_pid is not hosting" {
	pane_file 7 "server=$SRV"
	hosts %7 "$SRV"
	[ "$status" -eq 1 ]
}

@test "helper: a non-numeric agent_pid is not hosting" {
	pane_file 7 "server=$SRV" "agent_pid=abc"
	hosts %7 "$SRV"
	[ "$status" -eq 1 ]
}

@test "helper: a live agent_pid on this server is hosting (bare and %-prefixed id)" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	hosts %7 "$SRV"
	[ "$status" -eq 0 ]
	hosts 7 "$SRV"
	[ "$status" -eq 0 ]
}

@test "helper: a dead agent_pid is not hosting" {
	pane_file 7 "server=$SRV" "agent_pid=$(dead_pid)"
	hosts %7 "$SRV"
	[ "$status" -eq 1 ]
}

@test "helper: a server= mismatch is not hosting" {
	pane_file 7 "server=9999" "agent_pid=$$"
	hosts %7 "$SRV"
	[ "$status" -eq 1 ]
}

@test "helper: an empty server pid against a server= line fails closed" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	hosts %7 ""
	[ "$status" -eq 1 ]
}

@test "helper: a file with no server= line still counts when the pid is live" {
	pane_file 7 "agent_pid=$$"
	hosts %7 "$SRV"
	[ "$status" -eq 0 ]
	hosts %7 ""
	[ "$status" -eq 0 ]
}

@test "helper: agent_pid after server= and before it both parse" {
	printf 'agent_pid=%s\nstate=idle\nserver=%s\n' "$$" "$SRV" >"$CLAUDE_STATUS_DIR/panes/7"
	hosts %7 "$SRV"
	[ "$status" -eq 0 ]
}

@test "helper: an unterminated final line still parses" {
	printf 'state=idle\nserver=%s\nagent_pid=%s' "$SRV" "$$" >"$CLAUDE_STATUS_DIR/panes/7"
	hosts %7 "$SRV"
	[ "$status" -eq 0 ]
}

# --- A-E, H: the sweep (arm_agent_detect force) -------------------------------

@test "A: sweep keeps a launcher-hosted agent's title and stamps occupancy, no pipe-pane" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	row %7 bash
	sweep
	assert_title_kept
	grep -q 'set -qw -t @3 @window_has_agent 1' "$TLOG" || {
		echo "no @window_has_agent 1 write; tmux log:"
		cat "$TLOG"
		false
	}
	run grep -q 'pipe-pane' "$TLOG"
	[ "$status" -ne 0 ]
}

@test "A2: a hosted pane gets the live/ presence stamp" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	row %7 bash
	CLAUDE_ASSUME_DEAD_AFTER=60 sweep
	[ "$(cat "$CLAUDE_STATUS_DIR/live/7")" = 100 ]
}

@test "A3: a hosted pane is not birth-repainted nor birth-graced" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	printf '%%7|bash|0|@3||||100x30|1|ai-title||sess\n' >"$ROWS"
	REPAINT_BIN=/bin/true sweep
	run grep -Eq '^(set .*@og_birth|if |run-shell|pipe-pane)' "$TLOG"
	[ "$status" -ne 0 ]
}

@test "B: a crashed hosted agent (agent_pid dead) clears the title" {
	pane_file 7 "server=$SRV" "agent_pid=$(dead_pid)"
	row %7 bash
	sweep
	assert_title_cleared
}

@test "C: no panes file clears the title (unchanged)" {
	row %7 bash
	sweep
	assert_title_cleared
}

@test "D: a server= mismatch clears the title (fail closed)" {
	pane_file 7 "server=9999" "agent_pid=$$"
	row %7 bash
	sweep
	assert_title_cleared
}

@test "D2: no TMUX with a server= line clears the title (fail closed)" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	row %7 bash
	unset TMUX
	SERVER_PID="" sweep
	assert_title_cleared
}

@test "E: a @bridge_win mirror row is skipped whole" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	row %7 bash 1
	sweep
	run grep -q 'set -qw' "$TLOG"
	[ "$status" -ne 0 ]
}

@test "H: after the clean-exit delete of panes/7 the next sweep clears the title" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	row %7 bash
	sweep
	assert_title_kept
	# what `claude-status-update clear` does on SessionEnd
	rm -f "$CLAUDE_STATUS_DIR/panes/7"
	: >"$TLOG"
	sweep
	assert_title_cleared
}

@test "a real agent command pane still takes the agent path (unchanged)" {
	row %7 claude
	run bash -c 'source scripts/lib-claude.sh; source scripts/tmux-update-icons.sh; arm_agent_detect force'
	[ "$status" -eq 0 ]
	grep -q 'pipe-pane.*%7.*agent-detect 7' "$TLOG"
	assert_title_kept
}

@test "mark-seen style rewrite keeps the agent_pid line" {
	pane_file 7 "server=$SRV" "agent_pid=$$" "unseen=1"
	grep -v '^unseen=' "$CLAUDE_STATUS_DIR/panes/7" >"$TDIR/rewritten"
	cp "$TDIR/rewritten" "$CLAUDE_STATUS_DIR/panes/7"
	hosts %7 "$SRV"
	[ "$status" -eq 0 ]
}

# --- tmux-shell-prompt.sh (raw, @lib_claude@ substituted) ---------------------

# shell_prompt PANE CMD SESS WID HAS BRIDGE
shell_prompt() {
	local script="$TDIR/shell-prompt.sh"
	sed -e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" scripts/tmux-shell-prompt.sh >"$script"
	run bash "$script" "$@"
	[ "$status" -eq 0 ] || {
		echo "shell-prompt exited $status: $output"
		false
	}
}

@test "shell-prompt: a hosted live agent at a nested prompt keeps panes/<id>" {
	pane_file 7 "server=$SRV" "agent_pid=$$"
	shell_prompt %7 bash sess @3 1 0
	[ -f "$CLAUDE_STATUS_DIR/panes/7" ]
	run grep -q '@window_naming_dirty' "$TLOG"
	[ "$status" -ne 0 ]
}

@test "shell-prompt: after the hosted agent dies the existing clear path runs" {
	pane_file 7 "server=$SRV" "agent_pid=$(dead_pid)"
	printf '%%7|bash\n' >"$PANES_T"
	shell_prompt %7 bash sess @3 1 0
	[ ! -f "$CLAUDE_STATUS_DIR/panes/7" ]
	grep -q '@window_naming_dirty 1' "$TLOG"
}

@test "shell-prompt: a sibling hosted pane keeps the window's naming" {
	pane_file 8 "server=$SRV" "agent_pid=$$"
	printf '%%7|bash\n%%8|bash\n' >"$PANES_T"
	shell_prompt %7 bash sess @3 1 0
	run grep -q '@window_naming_dirty' "$TLOG"
	[ "$status" -ne 0 ]
}

@test "shell-prompt: no hosted pane and no agent resets the window as before" {
	printf '%%7|bash\n%%8|bash\n' >"$PANES_T"
	shell_prompt %7 bash sess @3 1 0
	grep -q '@window_naming_dirty 1' "$TLOG"
}

# --- F, G, sweep pass: a real scratch tmux server -----------------------------

# real_setup — scratch server on its own TMUX_TMPDIR/-L socket and state dirs.
# Called from each real-tmux test (the fake-tmux setup above must not leak a
# fake `tmux` onto this PATH).
real_setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"
	REAL_TMUX=$(PATH="${PATH#"$FAKEBIN":}" command -v tmux)
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$TDIR/data"
	export TMPDIR="$TDIR"
	mkdir -p "$TMUX_TMPDIR"
	SOCK="og-hosted-$BATS_TEST_NUMBER-$$"
	# Route every bare `tmux` (update-icons' own calls) at the scratch socket.
	SHIM="$TDIR/shim"
	mkdir -p "$SHIM"
	cat >"$SHIM/tmux" <<-EOF
		#!$(command -v bash)
		exec "$REAL_TMUX" -L "$SOCK" "\$@"
	EOF
	chmod +x "$SHIM/tmux"
	export PATH="$SHIM:${PATH#"$FAKEBIN":}"
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))
	export AGENT_COMMANDS="claude"
	unset AGENT_DETECT_BIN TMUX

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
	local licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@/["claude"]="C"/' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$LIB_CLAUDE|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e "s|@MAX_ICONS@|2|g" \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	# A non-agent default shell for the plain window; `claude` is a bash copy
	# only ever started as the launcher's child.
	mkdir -p "$TDIR/agentbin"
	cp -L "$(command -v bash)" "$TDIR/agentbin/claude"
	chmod +x "$TDIR/agentbin/claude"

	(
		unset TMUX
		"$REAL_TMUX" -L "$SOCK" -f /dev/null new-session -d -s S1 -x 200 -y 50 -c "$TDIR"
	)
	SERVER_PID_REAL=$("$REAL_TMUX" -L "$SOCK" display-message -p '#{pid}')
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		"$REAL_TMUX" -L "$SOCK" set -g "@$v" "#000000"
	done
}

real_teardown() {
	[[ -n ${REAL_TMUX:-} ]] || return 0
	"$REAL_TMUX" -L "$SOCK" kill-server 2>/dev/null || true
}

teardown() {
	real_teardown
}

rt() {
	"$REAL_TMUX" -L "$SOCK" "$@"
}

# start_launcher [SESSION] — a looping launcher shell hosting a `claude` copy
# (a loop, not `bash -c 'cmd'`, which execs and hides the bug). The loop ends
# when $STOP appears, after which the launcher stays alive on a sleep. Sets
# WID, PANE and CHILD (the child's pid, written by the child itself).
start_launcher() {
	local sess=${1:-S1} out
	STOP="$TDIR/stop"
	rm -f "$STOP" "$TDIR/child.pid"
	cat >"$TDIR/launcher.sh" <<-EOF
		while [ ! -e "$STOP" ]; do
		  "$TDIR/agentbin/claude" -c 'echo \$\$ >"$TDIR/child.pid"; while :; do sleep 1; done'
		done
		sleep 600
	EOF
	out=$(rt new-window -d -P -F '#{window_id}|#{pane_id}' -t "$sess:" -c "$TDIR" "$(command -v bash) $TDIR/launcher.sh")
	WID="${out%%|*}"
	PANE="${out##*|}"
	local i
	for ((i = 0; i < 50; i++)); do
		[[ -s $TDIR/child.pid ]] && break
		sleep 0.1
	done
	[[ -s $TDIR/child.pid ]]
	CHILD=$(cat "$TDIR/child.pid")
	kill -0 "$CHILD"
	# the bug's precondition: the pane reports the launcher, not the agent
	[ "$(rt display-message -p -t "$PANE" '#{pane_current_command}')" = bash ]
}

write_hosted_file() {
	printf 'state=idle\ntimestamp=%s\nunseen=1\nsession=x\nserver=%s\nagent_pid=%s\n' \
		"$CLAUDE_NOW" "$SERVER_PID_REAL" "${1:-$CHILD}" >"$CLAUDE_STATUS_DIR/panes/${PANE#%}"
}

tick() {
	run bash "$UPDATE_ICONS" S1
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}
}

wopt() {
	rt show-options -w -t "$1" -qv "$2" 2>/dev/null || true
}

kill_child_keep_launcher() {
	touch "$STOP"
	kill -9 "$CHILD"
	local i
	for ((i = 0; i < 50; i++)); do
		kill -0 "$CHILD" 2>/dev/null || break
		sleep 0.1
	done
	! kill -0 "$CHILD" 2>/dev/null
}

@test "F: per-tick @window_has_agent reads 1 for a launcher-hosted agent" {
	real_setup
	start_launcher
	write_hosted_file
	tick
	[ "$(wopt "$WID" @window_has_agent)" = 1 ]
}

@test "F2: a dispatcher window hosted by a launcher gets the standby overlay" {
	real_setup
	start_launcher
	write_hosted_file
	rt set -w -t "$WID" @crew_name dispatcher
	# the worker is an ordinary agent window: a `claude` bash copy as the shell
	rt set -g default-shell "$TDIR/agentbin/claude"
	rt set -g default-command ''
	local w
	w=$(rt new-window -d -P -F '#{window_id}|#{pane_id}' -t S1: -c "$TDIR")
	rt set -w -t "${w%%|*}" @crew_name amber
	rt set -w -t "${w%%|*}" @crew_id w1
	rt set -w -t "${w%%|*}" @window_has_agent 1
	printf 'state=processing\ntimestamp=%s\nunseen=1\nsession=x\n' "$CLAUDE_NOW" \
		>"$CLAUDE_STATUS_DIR/panes/${w##*|%}"
	# the overlay reads the pre-tick @window_has_agent, so tick twice
	tick
	tick
	[ "$(wopt "$WID" @window_has_agent)" = 1 ]
	[[ "$(wopt "$WID" @window_icon_display)" == *SB* ]]
}

@test "G: when the hosted agent dies the launcher stays and occupancy and names/ clear" {
	real_setup
	start_launcher
	write_hosted_file
	printf 'my-title\n' >"$CLAUDE_STATUS_DIR/names/${PANE#%}"
	tick
	[ "$(wopt "$WID" @window_has_agent)" = 1 ]
	[ "$(wopt "$WID" @window_ai_name)" = my-title ]
	kill_child_keep_launcher
	[ "$(rt display-message -p -t "$PANE" '#{pane_current_command}')" = bash ]
	tick
	[ -z "$(wopt "$WID" @window_has_agent)" ]
	[ -z "$(wopt "$WID" @window_ai_name)" ]
	[ ! -e "$CLAUDE_STATUS_DIR/names/${PANE#%}" ]
}

@test "real OG_TICK_SWEEP via run-shell: a hosted agent's title survives, then clears after it dies" {
	real_setup
	start_launcher
	write_hosted_file
	rt set -w -t "$WID" @window_ai_name my-title
	rt run-shell "OG_TICK_SWEEP=1 CLAUDE_NOW=$CLAUDE_NOW AGENT_COMMANDS=claude bash $UPDATE_ICONS"
	[ "$(wopt "$WID" @window_ai_name)" = my-title ]
	[ "$(wopt "$WID" @window_has_agent)" = 1 ]
	kill_child_keep_launcher
	rt run-shell "OG_TICK_SWEEP=1 CLAUDE_NOW=$CLAUDE_NOW AGENT_COMMANDS=claude bash $UPDATE_ICONS"
	[ -z "$(wopt "$WID" @window_ai_name)" ]
	[ -z "$(wopt "$WID" @window_has_agent)" ]
}
