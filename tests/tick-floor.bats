#!/usr/bin/env bats
# Live proof for #603's tick floor: the set-hook -g -B monitors in
# config/tmux.conf.nix fire on the server's own clock, independent of any
# attached client -- which tick-floor-conf-assertions (flake.nix) can only
# assert about the emitted TEXT, not about tmux's actual runtime behaviour.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	SOCKET="og-tick-floor-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# Mandatory (#603): these hooks fire inside this test server and
	# reach functions that delete files under these dirs, whose defaults are
	# the developer's real /tmp trees.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR/panes"

	t new-session -d -s s -x 80 -y 24 -c "$PWD"
}

teardown() {
	# Guarded no-ops: most tests never start a coproc client, so CTL_PID/
	# SLEEPER_PID are unset and these blocks do nothing. Centralizing the
	# cleanup here (rather than inline at the end of the test body) means a
	# wait_for timeout earlier in the test -- which aborts the test under
	# bats' errexit -- can never skip past it and leak the client.
	if [ -n "${CTL_PID:-}" ]; then
		printf 'detach-client\n' >&"${CTL[1]}" || true
		kill "$CTL_PID" 2>/dev/null || true
		# Bounded: a client that ignores SIGTERM must not hang the suite.
		local waited=0
		while kill -0 "$CTL_PID" 2>/dev/null && ((waited < 5)); do
			sleep 1
			waited=$((waited + 1))
		done
		kill -9 "$CTL_PID" 2>/dev/null || true
		wait "$CTL_PID" 2>/dev/null || true
	fi
	if [ -n "${SLEEPER_PID:-}" ]; then
		kill "$SLEEPER_PID" 2>/dev/null || true
		wait "$SLEEPER_PID" 2>/dev/null || true
	fi
	t kill-server 2>/dev/null || true
	# Case 5 runs a second, unwrapped server on its own socket/TMUX_TMPDIR;
	# belt-and-suspenders in case an assertion failure skipped its own cleanup.
	[ -n "${SCRATCH_SOCK:-}" ] && tmux -S "$SCRATCH_SOCK" kill-server 2>/dev/null
	return 0
}

t() {
	timeout --foreground 30s "$TMUX_BIN" -L "$SOCKET" "$@"
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

file_exists() { [ -e "$1" ]; }
# The tick stamps carry a per-server suffix (#705), so match by prefix.
stamp_exists() { compgen -G "$1*" >/dev/null; }
file_absent() { [ ! -e "$1" ]; }
pane_pipe_armed() { [ "$(t display-message -p -t "$1" '#{pane_pipe}')" = 1 ]; }

@test "all five tick hooks register via show-hooks -g -B" {
	# show-hooks -g alone prints a monitor's COMMAND with no indication it is
	# a monitor at all (tests/tmux-next38-readiness.bats); -B is the one
	# listing form that reports the subscription itself.
	run t show-hooks -g -B
	[ "$status" -eq 0 ]
	for name in @og-pr-tick @og-backfill-tick @og-usage-tick @og-sweep-tick @og-res-tick; do
		[[ $output == *"$name::"* ]]
	done
}

@test "pr and backfill tick stamps appear with zero clients attached" {
	# No client ever attaches -- new-session -d is genuinely headless, which
	# is the root cause itself: a status-format-driven poller has never run
	# under this condition. Both scripts stamp unconditionally in --tick /
	# --backfill mode, so the stamp alone is a sound recovery witness.
	wait_for 20 stamp_exists "$OG_ENRICH_CACHE_DIR/.last-tick"
	wait_for 20 stamp_exists "$OG_ENRICH_CACHE_DIR/.last-backfill-tick"
}

@test "pr and backfill tick stamps appear with only a control-mode client attached" {
	# The exact production shape on a control-only bridge host (#603's
	# report): a control client renders no status line, so the old
	# status-format[0] #() jobs never ran for it either.
	coproc CTL { "$TMUX_BIN" -L "$SOCKET" -C attach-session -t s; }
	wait_for 20 stamp_exists "$OG_ENRICH_CACHE_DIR/.last-tick"
	wait_for 20 stamp_exists "$OG_ENRICH_CACHE_DIR/.last-backfill-tick"
	# Cleanup lives in teardown (CTL_PID): a wait_for timeout above aborts
	# the test here under errexit and must not skip it.
}

@test "sweep arms pipe-pane on an agent pane with zero clients attached" {
	# A process whose command name really is "claude" on BOTH platforms: a
	# copy of the bash binary under that name (the pattern
	# update-icons-all-windows.bats already uses). `exec -a claude bash` only
	# rewrites argv[0], which is what linux's tmux reads (/proc cmdline) --
	# darwin's tmux reads pbsi_comm, the exec'd FILE's name, so an exec -a
	# fixture reports "bash" there, the agent manifest never matches, and the
	# sweep correctly finds nothing to arm (the darwin-only failure this test
	# had through six CI probe rounds on #608). `read` is a builtin, so the
	# copy never execs into a second image.
	cp -L "$(command -v bash)" "$BATS_TEST_TMPDIR/claude"
	chmod +x "$BATS_TEST_TMPDIR/claude"
	t new-window -t s:99 -c "$PWD" -- "$BATS_TEST_TMPDIR/claude" -c 'read x'
	wait_for 20 pane_pipe_armed s:99
}

@test "upstream assumption: a status-format #() job never expands for a control-mode client, only for a real one" {
	# This does NOT go red if the #603 fix is reverted -- it is not the
	# regression net for this change (the recovery tests above are). It pins
	# the tmux behaviour the whole design rests on (status_line_size returns 0
	# for a CLIENT_CONTROL client, so status_redraw never expands the format),
	# so a future tmux that started expanding status formats for control
	# clients would be noticed here.
	command -v script >/dev/null || skip "no script(1) for a pty"
	script --version 2>/dev/null | grep -q util-linux || skip "util-linux script(1) unavailable"

	local marker="$BATS_TEST_TMPDIR/marker.log"
	: >"$marker"
	local scratch_tmpdir="$BATS_TEST_TMPDIR/scratch-tmux"
	mkdir -p "$scratch_tmpdir"
	SCRATCH_SOCK="$BATS_TEST_TMPDIR/scratch.sock"
	local conf="$BATS_TEST_TMPDIR/scratch.conf"
	cat >"$conf" <<-EOF
		set -g status-interval 1
		set -g status-format[0] "#(echo x >> $marker)"
	EOF

	# Plain tmux (mkTmux), not the wrapped TMUX_BIN: this is a hand-written,
	# tmux-og-free fixture -- the point is to pin bare tmux's own behaviour,
	# not anything this repo's config does.
	TMUX_TMPDIR="$scratch_tmpdir" tmux -S "$SCRATCH_SOCK" -f "$conf" new-session -d -s m -x 80 -y 24

	# Negative half, same server/config/marker as the positive half below:
	# only a control-mode client attached.
	coproc CTL { TMUX_TMPDIR="$scratch_tmpdir" tmux -S "$SCRATCH_SOCK" -C attach-session -t m; }
	sleep 4
	local after_control
	after_control=$(wc -l <"$marker")
	printf 'detach-client\n' >&"${CTL[1]}" || true
	kill "$CTL_PID" 2>/dev/null || true
	wait "$CTL_PID" 2>/dev/null || true
	[ "$after_control" -eq 0 ]

	# Positive control: without this half, "the marker never fired" is
	# equally true of a broken fixture (wrong path, config never loaded, a
	# dead control client) as of the real tmux behaviour being pinned. A real
	# client needs a pty (script) and a stdin that neither closes (EOF
	# detaches the client instantly) nor feeds bytes into the pane as
	# keystrokes -- a `sleep 30` writing into a FIFO gives exactly that, and
	# unlike `< <(sleep 30)` its PID is capturable, so teardown can kill it
	# instead of leaving a stray job running for up to 30s on every test run.
	# (A coproc would also capture the PID, but bash closes a coproc's fd on
	# exec for an async command, so piping its read end into a backgrounded
	# `script` fails with "Bad file descriptor".)
	local stdin_fifo="$BATS_TEST_TMPDIR/real-client-stdin.fifo"
	mkfifo "$stdin_fifo"
	sleep 30 >"$stdin_fifo" &
	SLEEPER_PID=$!
	TMUX_TMPDIR="$scratch_tmpdir" script -qec "tmux -S $SCRATCH_SOCK attach-session -t m" /dev/null \
		>/dev/null 2>&1 <"$stdin_fifo" &
	local script_pid=$!
	sleep 4
	local after_real
	after_real=$(wc -l <"$marker")
	TMUX_TMPDIR="$scratch_tmpdir" tmux -S "$SCRATCH_SOCK" detach-client -s m 2>/dev/null || true
	kill "$script_pid" 2>/dev/null || true

	TMUX_TMPDIR="$scratch_tmpdir" tmux -S "$SCRATCH_SOCK" kill-server 2>/dev/null || true
	SCRATCH_SOCK=""

	[ "$after_real" -gt "$after_control" ]
}
