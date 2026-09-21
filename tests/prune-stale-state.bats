#!/usr/bin/env bats

load helper

setup() {
	# Export before sourcing: lib-claude derives CLAUDE_*_DIR from this at source time.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	unset TMUX TMUX_PANE
	setup_lib_claude
	mkdir -p "$CLAUDE_NAMES_DIR" "$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_PANES_DIR" \
		"$CLAUDE_INTERRUPT_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_WATCHERS_DIR" "$CLAUDE_LIVE_DIR"
}

seed_reap_files() {
	local id="$1" dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		printf 'x' >"$dir/$id"
	done
}

teardown() {
	[[ -n ${FAKE_TMUX_PID:-} ]] && kill "$FAKE_TMUX_PID" 2>/dev/null
	return 0
}

# A long-lived process whose comm is "tmux" — a live owner that passes
# claude_pid_is_tmux. Sets FAKE_TMUX_PID.
start_fake_tmux_server() {
	# A copied bash named "tmux" idling in a builtin read: comm is the exec'd
	# file's name, and a multicall sleep would refuse an unknown argv[0].
	cp "$(command -v bash)" "$BATS_TEST_TMPDIR/tmux"
	mkfifo "$BATS_TEST_TMPDIR/idle"
	# shellcheck disable=SC2016 # $1 is expanded by the child bash, not here
	"$BATS_TEST_TMPDIR/tmux" -c 'read -t 300 <>"$1"' _ "$BATS_TEST_TMPDIR/idle" &
	FAKE_TMUX_PID=$!
}

# Fake tmux on PATH. $1 is has-session's exit status (0 = session exists); $2,
# when set, is what `display-message -p '#{pid}'` prints (this server's own
# pid). Every other display-message (claude_progress_emit's #{pane_tty}) fails
# closed, and so does the pid query when $2 is unset.
install_fake_tmux() {
	local has_session="${1:-1}" own_pid="${2:-}" pid_case=""
	[[ -n $own_pid ]] && pid_case="*'#{pid}'*) echo $own_pid ;;"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		case "\$*" in
		*"has-session"*) exit $has_session ;;
		$pid_case
		*) exit 1 ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
}

# A server that booted mid-2017 — after the fixed "old" mtime below, before now.
SERVER_START=1500000000

# stamp FILE — write FILE and backdate it to 2000 (older than any real server).
stamp() {
	printf 'x' >"$1"
	touch -t 200001010000 "$1"
}

@test "prune drops files older than server start, keeps fresh ones" {
	stamp "$CLAUDE_NAMES_DIR/8"            # pre-restart: stale
	printf 'fresh' >"$CLAUDE_NAMES_DIR/10" # written now: current server
	claude_prune_stale_state "$SERVER_START"
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ -e "$CLAUDE_NAMES_DIR/10" ]
}

@test "prune sweeps every pane-keyed dir" {
	stamp "$CLAUDE_NAMES_DIR/8"
	stamp "$CLAUDE_TASKS_DIR/8"
	stamp "$CLAUDE_PANES_DIR/8"
	stamp "$CLAUDE_INTERRUPT_DIR/8"
	claude_prune_stale_state "$SERVER_START"
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ ! -e "$CLAUDE_TASKS_DIR/8" ]
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
	[ ! -e "$CLAUDE_INTERRUPT_DIR/8" ]
}

@test "prune records the server start marker" {
	claude_prune_stale_state "$SERVER_START"
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start")" = "$SERVER_START" ]
}

@test "prune is a no-op for the same server (marker gate)" {
	claude_prune_stale_state "$SERVER_START"
	# A stale file appearing after the gate is set must survive — the scan only
	# runs once per server, not every tick.
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state "$SERVER_START"
	[ -e "$CLAUDE_NAMES_DIR/8" ]
}

@test "prune re-runs when the server start changes" {
	claude_prune_stale_state "$SERVER_START"
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state $((SERVER_START + 1000))
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start")" = "$((SERVER_START + 1000))" ]
}

@test "prune with empty server start is a no-op" {
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state ""
	[ -e "$CLAUDE_NAMES_DIR/8" ]
	[ ! -e "$CLAUDE_STATUS_DIR/.server_start" ]
}

# #676: cross-server ownership. A panes/<id> file's server= field (stamped by
# claude-status-update.sh / agentstatus.go) names the PID of the tmux server
# that wrote it; when SERVER_PID is passed, claude_prune_stale_state must
# never delete a file owned by a different, still-live PID, regardless of
# mtime — but must still reap its own dead leftovers, exactly as before,
# when SERVER_PID is omitted entirely.

@test "prune protects a stale panes file whose server= pid is a different, live process" {
	start_fake_tmux_server
	printf 'server=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	# 999999999: this booting server's own pid, deliberately not $$.
	claude_prune_stale_state "$SERVER_START" 999999999
	[ -e "$CLAUDE_PANES_DIR/8" ]
}

@test "prune reaps a stale panes file whose server= pid is dead" {
	# 2147483647 exceeds any real pid_max — guaranteed never a live process.
	printf 'server=2147483647\n' >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
}

@test "prune reaps its own stale files (server= matches SERVER_PID)" {
	printf 'server=12345\n' >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 12345
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
}

@test "prune protection extends from panes/<id> to a sibling dir under the same id" {
	start_fake_tmux_server
	printf 'server=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ -e "$CLAUDE_PANES_DIR/8" ]
	[ -e "$CLAUDE_NAMES_DIR/8" ]
}

# #709: a screen-only agent pane (pi/codex/cursor) has no panes/<id>; its owner
# rides screen/<id> (statefile.Writer, agentstatus.go) and watchers/<id>
# (registerWatcher) instead.

@test "prune protects a stale screen file (and its watcher) owned by a live foreign server" {
	start_fake_tmux_server
	printf 'state=idle\ntimestamp=1\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_SCREEN_DIR/8"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/8"
	touch -t 200001010000 "$CLAUDE_SCREEN_DIR/8" "$CLAUDE_WATCHERS_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ -e "$CLAUDE_SCREEN_DIR/8" ]
	[ -e "$CLAUDE_WATCHERS_DIR/8" ]
}

@test "prune protects screen/<id> through a stale watchers file alone" {
	start_fake_tmux_server
	printf 'state=idle\ntimestamp=1\n' >"$CLAUDE_SCREEN_DIR/8"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/8"
	touch -t 200001010000 "$CLAUDE_SCREEN_DIR/8" "$CLAUDE_WATCHERS_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ -e "$CLAUDE_SCREEN_DIR/8" ]
	[ -e "$CLAUDE_WATCHERS_DIR/8" ]
}

@test "prune reaps screen and watchers files whose server= pid is dead" {
	printf 'state=idle\ntimestamp=1\nserver=2147483647\n' >"$CLAUDE_SCREEN_DIR/8"
	printf '123\nserver=2147483647\n' >"$CLAUDE_WATCHERS_DIR/8"
	touch -t 200001010000 "$CLAUDE_SCREEN_DIR/8" "$CLAUDE_WATCHERS_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ ! -e "$CLAUDE_SCREEN_DIR/8" ]
	[ ! -e "$CLAUDE_WATCHERS_DIR/8" ]
}

@test "prune reaps screen and watchers files owned by its own SERVER_PID" {
	printf 'state=idle\ntimestamp=1\nserver=12345\n' >"$CLAUDE_SCREEN_DIR/8"
	printf '123\nserver=12345\n' >"$CLAUDE_WATCHERS_DIR/8"
	touch -t 200001010000 "$CLAUDE_SCREEN_DIR/8" "$CLAUDE_WATCHERS_DIR/8"
	claude_prune_stale_state "$SERVER_START" 12345
	[ ! -e "$CLAUDE_SCREEN_DIR/8" ]
	[ ! -e "$CLAUDE_WATCHERS_DIR/8" ]
}

@test "prune: an own-pid panes file does not hide a foreign live owner in watchers" {
	start_fake_tmux_server
	printf 'server=12345\n' >"$CLAUDE_PANES_DIR/8"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/8"
	printf 'state=idle\ntimestamp=1\n' >"$CLAUDE_SCREEN_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8" "$CLAUDE_WATCHERS_DIR/8" "$CLAUDE_SCREEN_DIR/8"
	claude_prune_stale_state "$SERVER_START" 12345
	[ -e "$CLAUDE_PANES_DIR/8" ]
	[ -e "$CLAUDE_WATCHERS_DIR/8" ]
	[ -e "$CLAUDE_SCREEN_DIR/8" ]
}

@test "prune does not protect an alive owner that is not a tmux process (pid reuse)" {
	command -v ps >/dev/null || skip "ps not available"
	printf 'server=%s\n' "$$" >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
}

@test "prune gates per server pid: same pid skips, a different pid still sweeps" {
	claude_prune_stale_state "$SERVER_START" 111
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 111
	[ -e "$CLAUDE_NAMES_DIR/8" ]
	# Same start_time, another server: its own marker is absent, so it sweeps.
	claude_prune_stale_state "$SERVER_START" 222
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
}

@test "prune with a pid still writes the shared .server_start marker" {
	claude_prune_stale_state "$SERVER_START" 111
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start")" = "$SERVER_START" ]
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start.111")" = "$SERVER_START" ]
}

@test "prune removes per-server markers of dead pids" {
	printf '%s\n' "$SERVER_START" >"$CLAUDE_STATUS_DIR/.server_start.2147483647"
	claude_prune_stale_state "$SERVER_START" 111
	[ ! -e "$CLAUDE_STATUS_DIR/.server_start.2147483647" ]
	[ -e "$CLAUDE_STATUS_DIR/.server_start.111" ]
}

@test "prune reaps a legacy panes file with no server= field" {
	stamp "$CLAUDE_PANES_DIR/8"
	claude_prune_stale_state "$SERVER_START" 999999999
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
}

@test "prune with SERVER_PID omitted ignores server= entirely (backward compatible)" {
	printf 'server=%s\n' "$$" >"$CLAUDE_PANES_DIR/8"
	touch -t 200001010000 "$CLAUDE_PANES_DIR/8"
	claude_prune_stale_state "$SERVER_START"
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
}

@test "reap drops dead pane files across panes/screen/interrupt/tasks/issues/watchers, keeps live ones" {
	local rows
	rows="$(printf '%%3|codex|0\n%%5|fish|0\n')"
	local dir id
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		for id in 3 5 8; do
			printf 'x' >"$dir/$id"
		done
	done
	claude_reap_dead_panes "$rows"
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ ! -e "$dir/8" ]
		[ -e "$dir/3" ]
		[ -e "$dir/5" ]
	done
}

@test "reap never touches CLAUDE_LIVE_DIR" {
	local rows
	rows="$(printf '%%3|codex|0\n')"
	printf 'x' >"$CLAUDE_LIVE_DIR/8"
	claude_reap_dead_panes "$rows"
	[ -e "$CLAUDE_LIVE_DIR/8" ]
}

@test "reap with empty rows is a no-op" {
	printf 'x' >"$CLAUDE_PANES_DIR/8"
	claude_reap_dead_panes ""
	[ -e "$CLAUDE_PANES_DIR/8" ]
}

@test "reap with a live pane id that has no files is a no-op" {
	local rows
	rows="$(printf '%%3|codex|0\n')"
	claude_reap_dead_panes "$rows"
}

@test "reap_pane deletes exactly the named pane's six files, keeps a sibling's" {
	install_fake_tmux 0
	seed_reap_files 5
	seed_reap_files 8
	claude_reap_pane 5
	local dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ ! -e "$dir/5" ]
		[ -e "$dir/8" ]
	done
}

@test "reap_pane rejects empty, garbage, %12x, and command-injection ids" {
	install_fake_tmux 0
	seed_reap_files 5
	claude_reap_pane ""
	claude_reap_pane garbage
	claude_reap_pane '%12x'
	claude_reap_pane '5; rm -rf /'
	local dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ -e "$dir/5" ]
	done
}

@test "reap_pane guard: has-session failure keeps files" {
	install_fake_tmux 1
	printf 'state=idle\nsession=alpha\n' >"$CLAUDE_PANES_DIR/5"
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	[ -e "$CLAUDE_PANES_DIR/5" ]
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "reap_pane guard: has-session success deletes files" {
	install_fake_tmux 0
	printf 'state=idle\nsession=alpha\n' >"$CLAUDE_PANES_DIR/5"
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "reap_pane screen-only pane deletes unguarded" {
	install_fake_tmux 1
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	printf 'x' >"$CLAUDE_TASKS_DIR/5"
	claude_reap_pane 5
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
	[ ! -e "$CLAUDE_TASKS_DIR/5" ]
}

@test "reap_pane accepts bare 5 and %5 equally" {
	install_fake_tmux 0
	seed_reap_files 5
	claude_reap_pane 5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	seed_reap_files 5
	claude_reap_pane %5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
}

# #711: ownership by server= for the pane-exit reapers and the sweep. OWN is the
# fake tmux's own pid; DEAD_PID exceeds any real pid_max.
OWN_PID=4242
DEAD_PID=2147483647
OWNED_DIRS=(PANES SCREEN INTERRUPT TASKS ISSUES WATCHERS)

# seed_owned ID OWNER [SESSION] — a full set of ID's files; OWNER (may be empty
# for a legacy file) goes in server= on panes/screen/watchers, the three writers
# that stamp it.
seed_owned() {
	local id="$1" owner="$2" sess="${3:-alpha}" stamp=""
	[[ -n $owner ]] && stamp="server=$owner"
	printf 'state=idle\nsession=%s\n%s\n' "$sess" "$stamp" >"$CLAUDE_PANES_DIR/$id"
	printf 'state=idle\ntimestamp=1\n%s\n' "$stamp" >"$CLAUDE_SCREEN_DIR/$id"
	printf '123\n%s\n' "$stamp" >"$CLAUDE_WATCHERS_DIR/$id"
	printf 'x' >"$CLAUDE_INTERRUPT_DIR/$id"
	printf 'x' >"$CLAUDE_TASKS_DIR/$id"
	printf 'x' >"$CLAUDE_ISSUES_DIR/$id"
}

# owned_dir NAME — the CLAUDE_<NAME>_DIR value.
owned_dir() {
	local var="CLAUDE_${1}_DIR"
	printf '%s' "${!var}"
}

assert_files() {
	local want="$1" id="$2" name
	for name in "${OWNED_DIRS[@]}"; do
		if [[ $want == kept ]]; then
			[ -e "$(owned_dir "$name")/$id" ]
		else
			[ ! -e "$(owned_dir "$name")/$id" ]
		fi
	done
}

@test "reap_pane keeps every file when panes/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$FAKE_TMUX_PID"
	claude_reap_pane 5
	assert_files kept 5
}

@test "reap_pane deletes when server= is its own pid" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$OWN_PID"
	claude_reap_pane 5
	assert_files gone 5
}

@test "reap_pane deletes when server= names a dead process" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$DEAD_PID"
	claude_reap_pane 5
	assert_files gone 5
}

@test "reap_pane deletes a legacy file with no server=" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 ""
	claude_reap_pane 5
	assert_files gone 5
}

@test "reap_pane keeps a screen-only pane whose screen/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	printf 'state=idle\ntimestamp=1\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_SCREEN_DIR/5"
	printf 'x' >"$CLAUDE_TASKS_DIR/5"
	claude_reap_pane 5
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
	[ -e "$CLAUDE_TASKS_DIR/5" ]
}

@test "reap_pane keeps a pane whose only stamped file is watchers/ naming another live server" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/5"
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	[ -e "$CLAUDE_WATCHERS_DIR/5" ]
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "reap_pane still fails closed on a session mismatch when server= is its own pid" {
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 5 "$OWN_PID"
	claude_reap_pane 5
	assert_files kept 5
}

@test "reap_pane keeps a live-owner file when its own pid cannot be resolved" {
	start_fake_tmux_server
	install_fake_tmux 0
	seed_owned 5 "$FAKE_TMUX_PID"
	claude_reap_pane 5
	assert_files kept 5
}

@test "clear_agent_state keeps everything when panes/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$FAKE_TMUX_PID"
	claude_clear_agent_state 5 alpha
	assert_files kept 5
}

@test "clear_agent_state clears panes/screen/interrupt when server= is its own pid" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$OWN_PID"
	claude_clear_agent_state 5 alpha
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
	[ ! -e "$CLAUDE_INTERRUPT_DIR/5" ]
	[ -e "$CLAUDE_TASKS_DIR/5" ]
	[ -e "$CLAUDE_ISSUES_DIR/5" ]
	[ -e "$CLAUDE_WATCHERS_DIR/5" ]
}

@test "clear_agent_state clears when server= names a dead process" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$DEAD_PID"
	claude_clear_agent_state 5 alpha
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "clear_agent_state clears a legacy file with no server=" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 ""
	claude_clear_agent_state 5 alpha
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "clear_agent_state keeps a screen-only pane whose screen/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 0 "$OWN_PID"
	printf 'state=idle\ntimestamp=1\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_SCREEN_DIR/5"
	claude_clear_agent_state 5 alpha
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "clear_agent_state keeps a screen-only pane whose watchers/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 0 "$OWN_PID"
	printf 'state=idle\ntimestamp=1\n' >"$CLAUDE_SCREEN_DIR/5"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/5"
	claude_clear_agent_state 5 alpha
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "clear_agent_state still fails closed on a session mismatch when server= is its own pid" {
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$OWN_PID" beta
	claude_clear_agent_state 5 alpha
	[ -e "$CLAUDE_PANES_DIR/5" ]
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "clear_agent_state keeps a live-owner file when its own pid cannot be resolved" {
	start_fake_tmux_server
	install_fake_tmux 0
	seed_owned 5 "$FAKE_TMUX_PID"
	claude_clear_agent_state 5 alpha
	assert_files kept 5
}

@test "sweep keeps every file of an absent id whose panes/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 8 "$FAKE_TMUX_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files kept 8
}

@test "sweep keeps every file of an absent screen-only id owned by another live server" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	printf 'state=idle\ntimestamp=1\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_SCREEN_DIR/8"
	printf 'x' >"$CLAUDE_TASKS_DIR/8"
	printf 'x' >"$CLAUDE_INTERRUPT_DIR/8"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	[ -e "$CLAUDE_SCREEN_DIR/8" ]
	[ -e "$CLAUDE_TASKS_DIR/8" ]
	[ -e "$CLAUDE_INTERRUPT_DIR/8" ]
}

@test "sweep keeps every file of an absent id whose watchers/ names another live server" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	printf '123\nserver=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_WATCHERS_DIR/8"
	printf 'x' >"$CLAUDE_SCREEN_DIR/8"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	[ -e "$CLAUDE_WATCHERS_DIR/8" ]
	[ -e "$CLAUDE_SCREEN_DIR/8" ]
}

@test "sweep reaps an absent id owned by its own pid" {
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 8 "$OWN_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files gone 8
}

@test "sweep reaps an absent legacy id with no server=" {
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 8 ""
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files gone 8
}

@test "sweep reaps an absent id whose server= names a dead process" {
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 8 "$DEAD_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files gone 8
}

@test "sweep guards per id: a foreign id is kept while its neighbour is reaped" {
	start_fake_tmux_server
	install_fake_tmux 1 "$OWN_PID"
	seed_owned 8 "$FAKE_TMUX_PID"
	seed_owned 9 "$OWN_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files kept 8
	assert_files gone 9
}

@test "sweep takes the own pid from its OWN_PID argument without asking tmux" {
	start_fake_tmux_server
	install_fake_tmux 1
	seed_owned 8 "$FAKE_TMUX_PID"
	seed_owned 9 "$OWN_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')" "$OWN_PID"
	assert_files kept 8
	assert_files gone 9
}

@test "sweep keeps a live-owner id when its own pid cannot be resolved" {
	start_fake_tmux_server
	install_fake_tmux 1
	seed_owned 8 "$FAKE_TMUX_PID"
	claude_reap_dead_panes "$(printf '%%3|fish|0\n')"
	assert_files kept 8
}

@test "reap_pane: one foreign-stamped file keeps the whole id, own-stamped panes/ included" {
	start_fake_tmux_server
	install_fake_tmux 0 "$OWN_PID"
	seed_owned 5 "$OWN_PID"
	printf 'server=%s\n' "$FAKE_TMUX_PID" >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	assert_files kept 5
}
