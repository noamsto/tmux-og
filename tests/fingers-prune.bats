#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# tmux-fingers writes $XDG_DATA_HOME/tmux-fingers/tmux-<server pid>/config.json
# on every config load and never removes it (#1066). tmux-fingers-prune, run
# from the config, removes the dirs of servers that are gone.
#
# Every tmux here runs on its own -L socket with its own TMUX_TMPDIR and an
# XDG_DATA_HOME under the test tmpdir: never the developer's real fingers dir.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	unset TMUX TMUX_PANE
	# Short and fixed: a long $BATS_TEST_TMPDIR can pass the AF_UNIX path limit.
	export TMUX_TMPDIR="/tmp/og1066-$$-${BATS_TEST_NUMBER}"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	export HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$HOME"
	export XDG_CACHE_HOME="$HOME/.cache"
	export XDG_CONFIG_HOME="$HOME/.config"
	export XDG_STATE_HOME="$HOME/.local/state"
	export TERM=xterm-256color
	SHELL="$(command -v bash)"
	export SHELL
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	FINGERS="$XDG_DATA_HOME/tmux-fingers"
	SOCKS=()

	PRUNE="$BATS_TEST_TMPDIR/tmux-fingers-prune"
	sed "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" scripts/tmux-fingers-prune.sh >"$PRUNE" 2>/dev/null || true
	chmod +x "$PRUNE"
}

teardown() {
	local s
	for s in "${SOCKS[@]}"; do
		timeout --foreground 30s "$TMUX_BIN" -L "$s" kill-server 2>/dev/null || true
	done
	kill "${SLEEP_PID:-}" 2>/dev/null || true
	rm -rf "$TMUX_TMPDIR"
	return 0
}

tm() {
	local s=$1
	shift
	timeout --foreground 30s "$TMUX_BIN" -L "$s" "$@"
}

# start_server SOCK: scratch server loading the real wrapped config.
start_server() {
	SOCKS+=("$1")
	tm "$1" new-session -d -s s -x 120 -y 30
}

# wait_for PATH: bounded, non-fatal poll for PATH to exist.
wait_for() {
	local i
	for ((i = 0; i < 100; i++)); do
		[[ -e $1 ]] && return 0
		sleep 0.1
	done
	return 0
}

@test "a dead scratch server's fingers dir is pruned by the next server's config load" {
	start_server fa
	local pa fingers_ran=
	pa=$(tm fa display-message -p '#{pid}')
	wait_for "$FINGERS/tmux-$pa/config.json"
	if [[ -e $FINGERS/tmux-$pa/config.json ]]; then
		fingers_ran=1
		wait_for "$FINGERS/.prune-stamp"
	else
		# The nix sandbox has no /usr/bin/env, so fingers.tmux cannot run there
		# and writes nothing: seed the dir fingers would have written.
		mkdir -p "$FINGERS/tmux-$pa"
		: >"$FINGERS/tmux-$pa/config.json"
	fi
	tm fa kill-server
	# Defeat the rate limit so B's prune is not masked by A's stamp.
	[[ -e $FINGERS/.prune-stamp ]] && touch -d '20 minutes ago' "$FINGERS/.prune-stamp"

	start_server fb
	local pb
	pb=$(tm fb display-message -p '#{pid}')
	wait_for "$FINGERS/tmux-$pb/config.json"
	local i
	for ((i = 0; i < 100; i++)); do
		[[ -d $FINGERS/tmux-$pa ]] || break
		sleep 0.1
	done

	[[ ! -d $FINGERS/tmux-$pa ]]
	[[ -z $fingers_ran ]] || [[ -d $FINGERS/tmux-$pb ]]
}

@test "prune keeps a live tmux pid dir, removes dead and recycled-pid dirs, leaves other names" {
	local live
	live=$(XDG_DATA_HOME="$BATS_TEST_TMPDIR/live-data" bash -c '
		"$1" -L pl new-session -d -s s && "$1" -L pl display-message -p "#{pid}"' _ "$TMUX_BIN")
	SOCKS+=(pl)
	sleep 300 &
	SLEEP_PID=$!
	mkdir -p "$FINGERS/tmux-$live" "$FINGERS/tmux-999999999" "$FINGERS/tmux-$SLEEP_PID" "$FINGERS/tmux-12ab" "$FINGERS/foo"
	: >"$FINGERS/fingers.log"

	run bash "$PRUNE" 1
	[ "$status" -eq 0 ]
	[[ -d $FINGERS/tmux-$live ]]
	[[ ! -d $FINGERS/tmux-999999999 ]]
	[[ ! -d $FINGERS/tmux-$SLEEP_PID ]]
	[[ -d $FINGERS/tmux-12ab ]]
	[[ -d $FINGERS/foo ]]
	[[ -f $FINGERS/fingers.log ]]
}

@test "prune keeps the dir of the pid it is handed" {
	mkdir -p "$FINGERS/tmux-999999998" "$FINGERS/tmux-999999997"
	run bash "$PRUNE" 999999998
	[ "$status" -eq 0 ]
	[[ -d $FINGERS/tmux-999999998 ]]
	[[ ! -d $FINGERS/tmux-999999997 ]]
}

@test "prune skips a second run inside the stamp window" {
	mkdir -p "$FINGERS/tmux-999999997"
	run bash "$PRUNE" 1
	[ "$status" -eq 0 ]
	[[ -e $FINGERS/.prune-stamp ]]
	mkdir -p "$FINGERS/tmux-999999996"
	run bash "$PRUNE" 1
	[ "$status" -eq 0 ]
	[[ -d $FINGERS/tmux-999999996 ]]
}

@test "prune exits 0 and creates nothing when the data dir is absent" {
	run bash "$PRUNE" 1
	[ "$status" -eq 0 ]
	[[ ! -e $FINGERS ]]
}
