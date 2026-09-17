#!/usr/bin/env bats
# Live proof for #646: pane-shell-prompt (OSC 133;A) on the wrapped server clears
# an exited agent's state from the shared files AND the bridge options, and its
# pane_current_command discriminator refuses a nested prompt inside a still-running
# agent. A shell that emits no OSC 133 leaves the state alone (the dead-agent
# floor is the only withdrawal path), and a colliding pane id on a second server
# cannot clear a foreign session's file (the ownership guard).

load helper

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	SOCKET="og-pane-shell-prompt-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# Mandatory (#603/#647 precedent): these hooks fire inside this test server
	# and delete files under these dirs, whose defaults are the real /tmp trees.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers,names}

	# #671's cases drive tmux-reflow-windows/tmux-update-icons directly (a
	# synchronous pass, since a `new-session -d` server has no attached client
	# to tick status-format[0]/the reflow hooks on its own) — both are real,
	# substituted packages on PATH via this check's nativeBuildInputs
	# (flake.nix). SHIM_DIR is a `tmux` wrapper routing their own bare `tmux`
	# calls at this test's -L socket, the same trick tests/test-display.sh
	# uses for its own direct reflow invocation.
	SHIM_DIR="$BATS_TEST_TMPDIR/shim"
	mkdir -p "$SHIM_DIR"
	cat >"$SHIM_DIR/tmux" <<-SHIMEOF
		#!$(command -v bash)
		exec "$TMUX_BIN" -L "$SOCKET" "\$@"
	SHIMEOF
	chmod +x "$SHIM_DIR/tmux"
}

teardown() {
	t kill-server 2>/dev/null || true
	if [ -n "${SCRATCH_SOCK:-}" ]; then
		timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" kill-server 2>/dev/null || true
	fi
	return 0
}

t() {
	timeout --foreground 30s "$TMUX_BIN" -L "$SOCKET" "$@"
}

tb() {
	timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" "$@"
}

bare_id() {
	local id="$1"
	id="${id#%}"
	printf '%s' "$id"
}

wait_for() {
	# wait_for <seconds> <command...> -- polls until <command...> exits 0.
	local timeout=$1
	shift
	local remaining=$timeout
	while ((remaining-- > 0)); do
		"$@" && return 0
		sleep 1
	done
	return 1
}

# wopt TARGET OPTION — window option value, "" if unset. -qv suppresses the
# "unknown option" error a not-yet-stamped @window_* option would otherwise
# raise, and prints the raw value with no quote-escaping.
wopt() {
	t show-options -w -t "$1" -qv "$2" 2>/dev/null || true
}

# window_naming_cleared TARGET BARE_PANE_ID — #671's window-wide reset done:
# @window_has_agent/@window_ai_name/@window_task empty and their
# names/tasks/issues files gone for BARE_PANE_ID.
window_naming_cleared() {
	local target="$1" bare="$2"
	[ -z "$(wopt "$target" @window_has_agent)" ] || return 1
	[ -z "$(wopt "$target" @window_ai_name)" ] || return 1
	[ -z "$(wopt "$target" @window_task)" ] || return 1
	[ ! -e "$CLAUDE_STATUS_DIR/names/$bare" ] || return 1
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/$bare" ] || return 1
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ] || return 1
}

# window_agent_gone_only TARGET BARE_PANE_ID — the manual-name arm of
# claude_clear_window_naming: @window_has_agent empty and issues/<pane> gone,
# but naming (ai_name/task/their files) untouched — asserted by the caller.
window_agent_gone_only() {
	local target="$1" bare="$2"
	[ -z "$(wopt "$target" @window_has_agent)" ] || return 1
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ] || return 1
}

# reflow SESSION — direct tmux-reflow-windows pass. A numeric WIDTH arg
# bypasses the script's own #{client_width} lookup (empty/non-numeric for an
# unattached session, tests/test-display.sh's own precedent), so this works
# with no client ever attached to SESSION.
reflow() {
	PATH="$SHIM_DIR:$PATH" tmux-reflow-windows "$1" 200 --force >/dev/null 2>&1
}

# update_icons_tick SESSION — one direct tmux-update-icons pass (the
# backstop), standing in for the status-format[0] `#()` poller an unattached
# test session never ticks on its own (#580).
update_icons_tick() {
	PATH="$SHIM_DIR:$PATH" tmux-update-icons "$1" >/dev/null 2>&1
}

# seed_shell_state PANE_ID SESSION — state + screen + interrupt + the two
# bridge options, the exact set claude_clear_agent_state clears. Hand-writes
# panes/<id> directly; seed_shell_state_via_writer (below) exercises the real
# writer instead, so at least one test breaks if that format drifts.
seed_shell_state() {
	local id sess
	id="$(bare_id "$1")"
	sess="$2"
	printf 'state=processing\ntimestamp=1000000000\nsession=%s\n' "$sess" \
		>"$CLAUDE_STATUS_DIR/panes/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/screen/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/interrupt/$id"
	t set-option -p -t "$1" @claude_status "processing 1000000000 0"
	t set-option -p -t "$1" @agent_screen "processing 1000000000"
}

# seed_shell_state_via_writer PANE_ID SESSION — same net effect as
# seed_shell_state, but panes/<id> is written by the real claude-status-update
# writer (claude-status-update.sh ~:479/:585) instead of a hand-rolled printf,
# so a writer format change (the session=/state= shape) breaks this suite too.
# --session is passed explicitly rather than relying on the writer's own
# `tmux display-message -p -t "$pane_id" '#{session_name}'` fallback, since
# that call targets the default tmux server, not this test's `-L $SOCKET` one.
# bridge_stamp inside the writer no-ops here (no $TMUX in this shell), so the
# two bridge options are still set by hand, like screen/interrupt.
seed_shell_state_via_writer() {
	local id sess
	id="$(bare_id "$1")"
	sess="$2"
	CLAUDE_STATUS_DIR="$CLAUDE_STATUS_DIR" bash scripts/claude-status-update.sh \
		processing --pane "$id" --session "$sess"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/screen/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/interrupt/$id"
	t set-option -p -t "$1" @claude_status "processing 1000000000 0"
	t set-option -p -t "$1" @agent_screen "processing 1000000000"
}

three_gone() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt; do
		[ ! -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

three_present() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt; do
		[ -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

# The pane shell is bash: its builtin printf is fork-free, so pane_current_command
# stays `bash` when we printf the mark — exactly the "shell is at the prompt" case.

@test "agent exits to prompt: files and bridge options cleared" {
	t new-session -d -s one -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t one -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state_via_writer "$id" one
	three_present "$id"

	t send-keys -t "$id" 'printf "\033]133;A\033\\"' Enter
	wait_for 5 three_gone "$id"
	# -F '#'{option_value}' is the raw read: bare show-options prints an empty
	# option as '' (the quote-escaping template), which -z would read as non-empty.
	[ -z "$(t show-options -p -t "$id" -F '#{option_value}' @claude_status 2>/dev/null)" ]
	[ -z "$(t show-options -p -t "$id" -F '#{option_value}' @agent_screen 2>/dev/null)" ]
}

@test "nested prompt inside a live agent: not cleared" {
	# pane_current_command comes from osdep_get_name: on Linux it reads
	# /proc/<pgrp>/cmdline (argv[0]), so `exec -a pi bash ...` fakes an agent
	# fine there — but on macOS it reads the kernel process name via
	# proc_pidinfo/pbsi_comm (tmux e880cf63, osdep-darwin.c), which tracks the
	# executable's own file name and ignores an argv[0] rewrite. exec -a pi
	# therefore still reports `bash` on darwin, the handler correctly sees a
	# non-agent foreground, and the state gets cleared — a false test failure,
	# not a handler bug. Use a real file named `pi` (a copy, not a symlink: a
	# symlink's kernel name is the target's) so the kernel process name is
	# `pi` on both platforms.
	mkdir -p "$TEST_HOME/bin"
	cp "$(command -v bash)" "$TEST_HOME/bin/pi"
	chmod +x "$TEST_HOME/bin/pi"
	# A script for the copied binary to interpret, so pane_current_command
	# (the interpreter's own kernel name) reads `pi`, not the script's name.
	cat >"$TEST_HOME/pi-script.sh" <<'EOF'
printf "\033]133;A\033\\"
sleep 3
EOF

	t new-session -d -s two -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t two -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state "$id" two
	three_present "$id"

	# The fake agent's file name is `pi` (in the manifest) while it runs and
	# emits a nested A — the handler must read it as a live agent and skip.
	# One send-keys argument: multiple args concatenate with no separator.
	t send-keys -t "$id" "$TEST_HOME/bin/pi $TEST_HOME/pi-script.sh" Enter
	sleep 1
	three_present "$id"
	sleep 3 # let the fake agent drain before teardown
}

@test "no OSC 133 emitted: state stays (the floor is the only withdrawal)" {
	t new-session -d -s three -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t three -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state "$id" three
	three_present "$id"

	t send-keys -t "$id" 'echo hi' Enter
	sleep 2
	three_present "$id"
}

@test "cross-server guard: a colliding pane id in a foreign session is skipped" {
	t new-session -d -s alpha -x 80 -y 24 -c "$PWD" -- bash
	local a_id
	a_id="$(t list-panes -t alpha -F '#{pane_id}')"
	[ -n "$a_id" ]
	# Seed alpha's own panes/<id> file; the option seeding is irrelevant here.
	printf 'state=processing\ntimestamp=1000000000\nsession=alpha\n' \
		>"$CLAUDE_STATUS_DIR/panes/$(bare_id "$a_id")"

	SCRATCH_SOCK="$BATS_TEST_TMPDIR/scratch.sock"
	tb new-session -d -s beta -x 80 -y 24 -c "$PWD" -- bash
	local b_id
	b_id="$(tb list-panes -t beta -F '#{pane_id}')"
	[ -n "$b_id" ]
	# Both are first panes: the ids collide (per-server %N).
	[ "$a_id" = "$b_id" ]

	# beta's prompt fires the hook; the handler reads session=alpha vs beta and
	# must refuse to clear.
	tb send-keys -t "$b_id" 'printf "\033]133;A\033\\"' Enter
	sleep 2
	[ -e "$CLAUDE_STATUS_DIR/panes/$(bare_id "$a_id")" ]
}

# #671: reset window naming/crew display when a window's last agent exits.
# claude_clear_agent_state's own pane-scoped state (exercised above) and
# claude_clear_window_naming's window-scoped naming/crew reset are two
# different clears fired by the same pane-shell-prompt hook invocation — these
# cases are additive to the ones above, not a replacement for them.

@test "window's last agent exits: naming/crew cleared, @crew_name kept, grid badge hidden" {
	t new-session -d -s naming1 -x 80 -y 24 -c "$PWD" -- bash
	local id bare
	id="$(t list-panes -t naming1 -F '#{pane_id}')"
	bare="$(bare_id "$id")"
	[ -n "$id" ]

	t set-option -w -t naming1 @window_has_agent 1
	t set-option -w -t naming1 @crew_name coral
	t set-option -w -t naming1 @crew_color colour210
	t set-option -w -t naming1 @window_ai_name "Old AI Name"
	t set-option -w -t naming1 @window_task "old task"
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'old task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	t send-keys -t "$id" 'printf "\033]133;A\033\\"' Enter
	wait_for 5 window_naming_cleared naming1 "$bare"

	# Hard constraint: @crew_name/@crew_color are dispatcher-owned and this
	# feature must never touch them.
	[ "$(wopt naming1 @crew_name)" = coral ]
	[ "$(wopt naming1 @crew_color)" = colour210 ]

	# The multi-line grid badge is reflow-computed into @window_crew_disp; an
	# empty @window_has_agent must blank it even though @crew_name lives on.
	reflow naming1
	[ -z "$(wopt naming1 @window_crew_disp)" ]
}

@test "second pane still runs a live agent: window-wide reset is skipped" {
	mkdir -p "$TEST_HOME/bin"
	cp "$(command -v bash)" "$TEST_HOME/bin/pi"
	chmod +x "$TEST_HOME/bin/pi"
	cat >"$TEST_HOME/pi-sleep.sh" <<-'EOF'
		sleep 5
	EOF

	t new-session -d -s naming2 -x 80 -y 24 -c "$PWD" -- bash
	t split-window -t naming2 -c "$PWD" -- bash
	local id_a id_b bare_a
	id_a="$(t list-panes -t naming2 -F '#{pane_id}' | sed -n 1p)"
	id_b="$(t list-panes -t naming2 -F '#{pane_id}' | sed -n 2p)"
	bare_a="$(bare_id "$id_a")"
	[ -n "$id_a" ] && [ -n "$id_b" ] && [ "$id_a" != "$id_b" ]

	# Pane B keeps a live agent running for the whole window.
	t send-keys -t "$id_b" "$TEST_HOME/bin/pi $TEST_HOME/pi-sleep.sh" Enter
	sleep 1

	t set-option -w -t naming2 @window_has_agent 1
	t set-option -w -t naming2 @crew_name coral
	t set-option -w -t naming2 @window_ai_name "Old AI Name"
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare_a"

	# Only pane A's prompt fires — the hook's own list-panes scan (step 2)
	# must still see pane B's live agent and skip the clear entirely.
	t send-keys -t "$id_a" 'printf "\033]133;A\033\\"' Enter
	sleep 2

	[ "$(wopt naming2 @window_has_agent)" = 1 ]
	[ "$(wopt naming2 @window_ai_name)" = "Old AI Name" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare_a" ]

	reflow naming2
	case "$(wopt naming2 @window_crew_disp)" in
	*coral*) : ;;
	*) echo "badge should still render, @window_crew_disp=[$(wopt naming2 @window_crew_disp)]" && false ;;
	esac

	sleep 4 # let the fake agent drain before teardown
}

@test "agent relaunches: backstop restores @window_has_agent with no crew re-stamp" {
	mkdir -p "$TEST_HOME/bin"
	cp "$(command -v bash)" "$TEST_HOME/bin/pi"
	chmod +x "$TEST_HOME/bin/pi"
	cat >"$TEST_HOME/pi-sleep.sh" <<-'EOF'
		sleep 5
	EOF

	t new-session -d -s naming3 -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t naming3 -F '#{pane_id}')"
	[ -n "$id" ]

	# A stale crew stamp from a prior agent occupancy, and no @window_has_agent
	# yet (as if the window had already been cleared once).
	t set-option -w -t naming3 @crew_name coral
	t set-option -w -t naming3 @crew_color colour99

	t send-keys -t "$id" "$TEST_HOME/bin/pi $TEST_HOME/pi-sleep.sh" Enter
	sleep 1

	update_icons_tick naming3
	[ "$(wopt naming3 @window_has_agent)" = 1 ]
	# No re-stamp: the backstop's has_agent=1 branch only ever sets
	# @window_has_agent, never @crew_name/@crew_color.
	[ "$(wopt naming3 @crew_name)" = coral ]
	[ "$(wopt naming3 @crew_color)" = colour99 ]

	reflow naming3
	case "$(wopt naming3 @window_crew_disp)" in
	*coral*) : ;;
	*) echo "badge should reappear, @window_crew_disp=[$(wopt naming3 @window_crew_disp)]" && false ;;
	esac

	sleep 4 # let the fake agent drain before teardown
}

@test "manual rename survives the agent-exit clear and a later automatic-rename tick" {
	t new-session -d -s naming5 -x 80 -y 24 -c "$PWD" -- bash
	local id bare
	id="$(t list-panes -t naming5 -F '#{pane_id}')"
	bare="$(bare_id "$id")"
	[ -n "$id" ]

	t set-option -w -t naming5 @window_has_agent 1
	t set-option -w -t naming5 @crew_name coral
	t set-option -w -t naming5 @window_ai_name "Old AI Name"
	t set-option -w -t naming5 @window_task "old task"
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'old task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	# The prefix + , bind's underlying commands (config/tmux.conf.tmpl /
	# .reference.nix): rename-window (which turns automatic-rename off on its
	# own) then the new durable @window_manual_name marker.
	t rename-window -t naming5 -- MyName
	t set-window-option -t naming5 @window_manual_name 1
	# show-options -qv renders a boolean option as "on"/"off" (unlike the
	# "1"/"0" a format string like #{automatic-rename} would expand to).
	[ "$(wopt naming5 automatic-rename)" = off ]

	t send-keys -t "$id" 'printf "\033]133;A\033\\"' Enter
	wait_for 5 window_agent_gone_only naming5 "$bare"

	# Naming state is left untouched for a manually-renamed window.
	[ "$(wopt naming5 @window_ai_name)" = "Old AI Name" ]
	[ "$(wopt naming5 @window_task)" = "old task" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]

	# The actual regression this closes: a later tick must not flip
	# automatic-rename back on for a manually-renamed window (#671) — before
	# this fix, tmux-update-icons' reassert-on-tick logic couldn't tell a
	# genuine user rename from tmux-remux's restore-induced automatic-rename
	# off, and reverted it within ~1s regardless.
	update_icons_tick naming5
	[ "$(wopt naming5 automatic-rename)" = off ]
	[ "$(t list-windows -t naming5 -F '#{window_name}')" = MyName ]
}

@test "bridge window: untouched by both the event hook and the backstop" {
	mkdir -p "$TEST_HOME/bin"
	cp "$(command -v bash)" "$TEST_HOME/bin/pi"
	chmod +x "$TEST_HOME/bin/pi"
	cat >"$TEST_HOME/pi-sleep.sh" <<-'EOF'
		sleep 5
	EOF

	t new-session -d -s naming6 -x 80 -y 24 -c "$PWD" -- bash
	local id bare
	id="$(t list-panes -t naming6 -F '#{pane_id}')"
	bare="$(bare_id "$id")"
	[ -n "$id" ]

	t set-option -w -t naming6 @bridge_win 1
	t set-option -w -t naming6 @window_has_agent 1
	t set-option -w -t naming6 @window_ai_name "Bridge Name"
	t set-option -w -t naming6 @window_task "bridge task"
	printf 'Bridge Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'bridge task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	# Event hook: gets past step 0 (@window_has_agent was 1 at fire time) but
	# must bail at the @bridge_win check before touching anything.
	t send-keys -t "$id" 'printf "\033]133;A\033\\"' Enter
	sleep 2

	[ "$(wopt naming6 @window_has_agent)" = 1 ]
	[ "$(wopt naming6 @window_ai_name)" = "Bridge Name" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/issues/$bare" ]

	# Backstop: restore the realistic invariant first — @window_has_agent is
	# never written for a bridge window by anything, so it never actually
	# starts stamped "1" in real operation (its only writer, this same
	# backstop, unconditionally skips bridge windows before scanning). Then
	# prove an agent-shaped foreground command in the mirror's local pane
	# still doesn't matter: win_cur_bridge alone gates the scan.
	t set-option -w -t naming6 @window_has_agent ""
	t send-keys -t "$id" "$TEST_HOME/bin/pi $TEST_HOME/pi-sleep.sh" Enter
	sleep 1

	update_icons_tick naming6
	[ -z "$(wopt naming6 @window_has_agent)" ]
	[ "$(wopt naming6 @window_ai_name)" = "Bridge Name" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]

	sleep 4 # let the fake agent drain before teardown
}

# Unit-level: claude_clear_agent_state's ownership guard, exercised by sourcing
# lib-claude.sh directly — no live tmux server needed (claude_progress_emit and
# the trailing `tmux set` both self-guard with no server on PATH/reachable).

@test "ownership guard: empty caller session does not clear (fails closed)" {
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh
	mkdir -p "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR"
	printf 'state=processing\ntimestamp=1000000000\nsession=real\n' >"$CLAUDE_PANES_DIR/9"
	: >"$CLAUDE_SCREEN_DIR/9"
	: >"$CLAUDE_INTERRUPT_DIR/9"

	claude_clear_agent_state 9 ""

	[ -e "$CLAUDE_PANES_DIR/9" ]
	[ -e "$CLAUDE_SCREEN_DIR/9" ]
	[ -e "$CLAUDE_INTERRUPT_DIR/9" ]
}

@test "ownership guard: unterminated session= line with mismatching session does not clear" {
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh
	mkdir -p "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR"
	# No trailing newline after the last (session=) line.
	printf 'state=processing\ntimestamp=1000000000\nsession=real' >"$CLAUDE_PANES_DIR/9"
	: >"$CLAUDE_SCREEN_DIR/9"
	: >"$CLAUDE_INTERRUPT_DIR/9"

	claude_clear_agent_state 9 other

	[ -e "$CLAUDE_PANES_DIR/9" ]
	[ -e "$CLAUDE_SCREEN_DIR/9" ]
	[ -e "$CLAUDE_INTERRUPT_DIR/9" ]
}
