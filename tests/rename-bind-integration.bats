#!/usr/bin/env bats
# shellcheck disable=SC2016,SC2088 # the $ and ~ window-name fixtures are literal; that they do NOT expand is the assertion
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for the mirror branch of `prefix + ,` (#367): a real
# keypress, delivered by a real attached client, driving the real emitted conf.
#
# Why not a conf grep: the repo's existing bind coverage
# (`bridge-carousel-bind-assertions`) greps the generated config, which can only
# say the text is there. #367 is about what tmux DOES with that text — a
# remote-derived `#(...)` in the prefill running locally — so the only honest
# observer is the running server.
#
# Two things this harness must get right or it cannot fail:
#   - A key binding fires only for an ATTACHED CLIENT. `send-keys` alone reaches
#     the pane's process, never the key table, so the client here is a second
#     `-L` server whose pane runs a real `tmux attach` and receives the keys.
#   - `#(...)` is asynchronous: format_job_get starts the job and substitutes the
#     PREVIOUS (empty) value, so the side effect lands after run-shell has handed
#     its command to the shell. The security assertion is a bounded poll that
#     waits out the timeout before concluding "never happened", and it ships a
#     positive control proving the channel can observe such a side effect at all.

setup() {
	IN="lz367-in-${BATS_TEST_NUMBER}-$$"
	OUT="lz367-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	# The remote pane id the gate carries. One constant: setup() stamps it and
	# write_argv() builds the expected wire payload from it, so they cannot drift.
	BRIDGE_PANE='%42'
	CTL="${CTL:?set CTL to the built og-remote-bridge-ctl}"
	# argv[0] of every ctl frame. Read from wire.CtlProtocolVersion by the check
	# derivation rather than pinned here: the assertions below are about the
	# EncodeArgv shape, and a protocol bump is not a regression in it.
	CTL_PROTOCOL_VERSION="${CTL_PROTOCOL_VERSION:?set CTL_PROTOCOL_VERSION to wire.CtlProtocolVersion}"

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# tmux takes default-shell from $SHELL, and `#(...)` jobs run under it. Pin it
	# to a shell that certainly exists in the sandbox so the security test and its
	# positive control are measuring the bind, not the shell lookup.
	SHELL="$(command -v bash)"
	export SHELL
	# The poller and sweep monitor hooks fire inside this test server, and the
	# sweep reaches two functions that delete files under these dirs — whose
	# defaults are the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	SOCK="$BATS_TEST_TMPDIR/d.sock"
	REC_DIR="$BATS_TEST_TMPDIR/rec"
	REC_WORK="$BATS_TEST_TMPDIR/rec-work"
	mkdir -p "$REC_DIR" "$REC_WORK"
	start_recorder

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat keys and repaint the status line the prefill
	# assertion reads.
	inner set-option -g @splash_shown 1

	# Flip the bridge gate: @bridge_win on the window, @bridge_pane on the PANE
	# (production stamps it there; a window stamp only resolves by inheritance and
	# would leave the gate a possible false negative), @bridge_sock on the SESSION.
	inner set-option -t s @bridge_sock "$SOCK"
	WIN="$(inner display-message -p -t s: '#{window_id}')"
	inner set-option -w -t "$WIN" @bridge_win 1
	PANE="$(inner list-panes -t s: -F '#{pane_id}' | head -1)"
	inner set-option -p -t "$PANE" @bridge_pane "$BRIDGE_PANE"
	[ "$(inner display-message -p -t "$PANE" -F '#{&&:#{@bridge_win},#{@bridge_pane}}')" = 1 ]
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	[[ -n ${RECORDER_PID:-} ]] && kill "$RECORDER_PID" 2>/dev/null
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# The recording stub standing in for the bridge daemon on @bridge_sock. One copy
# per connection (socat fork); it must answer with a FrameCtlAck, or ctl reports
# "does not speak the ctl protocol" through `display-message -t <client>` and
# overwrites the status line the prefill assertion reads.
#
# The payload is written to a FILE and never passes through a shell variable:
# bash cannot hold a NUL, so $(...)/tr/mapfile/cut would silently destroy the one
# byte the empty-name assertion turns on.
start_recorder() {
	local rec="$BATS_TEST_TMPDIR/recorder.sh"
	cat >"$rec" <<-'EOF'
		set -uo pipefail
		w="$(mktemp -d "$REC_WORK/w.XXXXXX")"
		dd bs=1 count=5 status=none of="$w/hdr" || exit 0
		[[ $(stat -c %s "$w/hdr") -eq 5 ]] || exit 0
		read -r -a b < <(od -An -tu1 -N5 "$w/hdr")
		printf '%s' "${b[0]}" >"$w/type"
		n=$((b[1] * 16777216 + b[2] * 65536 + b[3] * 256 + b[4]))
		: >"$w/payload"
		if ((n > 0)); then
			dd bs=1 count="$n" status=none of="$w/payload" || exit 0
		fi
		# Rename into place only once both files are complete, so a reader that
		# sees the directory sees a whole frame.
		mv "$w" "$REC_DIR/frame.${w##*/}"
		printf '\007'
		head -c 4 /dev/zero
	EOF
	export REC_DIR REC_WORK
	socat "UNIX-LISTEN:$SOCK,fork" "EXEC:bash $rec" &
	RECORDER_PID=$!
	local deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		[[ -S $SOCK ]] && return 0
		sleep 0.1
	done
	printf 'recorder socket never appeared at %s\n' "$SOCK" >&2
	return 1
}

# A real attached client for the inner server: its keys come from a second
# server's pane, which is where a keypress can actually reach the key table.
attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	OPANE="$(outer list-panes -F '#{pane_id}' | head -1)"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
	PREFIX="$(inner show-options -gv prefix)"
}

send() { outer send-keys -t "$OPANE" "$@"; }

press_prefix_key() { # key
	send "$PREFIX"
	sleep 0.2
	send "$1"
}

screen() { outer capture-pane -p -t "$OPANE"; }

# tmux names an un-`-p`'d command-prompt after the first command in its template,
# so the mirror branch prompts "(run-shell) " and the -I seed follows it.
wait_for_prompt() { # expected prompt text
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		screen | grep -qF "(run-shell) $1" && return 0
		sleep 0.1
	done
	printf 'timed out waiting for prompt "(run-shell) %s"; screen was:\n' "$1" >&2
	screen >&2
	return 1
}

wait_for_frame() {
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		compgen -G "$REC_DIR/frame.*/payload" >/dev/null && return 0
		sleep 0.1
	done
	printf 'timed out waiting for a ctl frame in %s\n' "$REC_DIR" >&2
	return 1
}

clear_frames() { rm -rf "${REC_DIR:?}"/frame.*; }

# A `#(...)` job is asynchronous — format_job_get substitutes the previous
# (empty) value and the job lands later — so "it never ran" is only a claim once
# the whole window has elapsed with nothing there. The positive budget is sized
# for CI CPU contention, where the click → display-menu → run-shell chain
# exceeds the old 5s and a payload that DID fire read as a miss (#832);
# negative `run !` callers pass the smaller window (they waited for the menu to
# render already). A timeout dumps the client screen for diagnosis.
SENTINEL_BUDGET_SECS=30
SENTINEL_NEGATIVE_SECS=10

wait_for_sentinel() { # path [budget_secs]
	local budget="${2:-$SENTINEL_BUDGET_SECS}"
	local deadline=$((SECONDS + budget))
	while ((SECONDS < deadline)); do
		[[ -e $1 ]] && return 0
		sleep 0.1
	done
	printf 'wait_for_sentinel("%s") timed out after %ss; screen was:\n' "$1" "$budget" >&2
	screen >&2
	return 1
}

# The single recorded frame's payload file. Fails loudly on 0 or 2+, so an extra
# connection can never be mistaken for the one under test.
sole_payload() {
	local -a d
	mapfile -t d < <(compgen -G "$REC_DIR/frame.*/payload")
	[ "${#d[@]}" -eq 1 ]
	printf '%s' "${d[0]}"
}

# The EncodeArgv payload for one rename request, written straight to a file: bash
# cannot hold a NUL, so it never touches a variable.
write_argv() { # file name
	printf '%s\000rename\000%s\000%s' "$CTL_PROTOCOL_VERSION" "$BRIDGE_PANE" "$2" >"$1"
}

# Byte-exact comparison against the EncodeArgv shape: NUL-separated, no trailing
# separator of its own. An empty name is therefore a trailing NUL and nothing
# after it — the only thing that distinguishes argv [ver,"rename","%42",""] from
# [ver,"rename","%42"], one byte shorter, which every field-splitting reading
# collapses into the same three fields.
assert_wire_argv() { # payload_file name
	local want="$BATS_TEST_TMPDIR/want"
	write_argv "$want" "$2"
	if ! cmp -s "$want" "$1"; then
		printf 'wire argv mismatch for name [%s]\nwant:\n' "$2" >&2
		od -c "$want" >&2
		printf 'got:\n' >&2
		od -c "$1" >&2
		return 1
	fi
}

# --- 7a: the harness proves it can observe, before it asserts anything ---------

# The whole fix rests on #{qs:} wrapping its value in POSIX single quotes. On
# tmux 3.7b that modifier silently returns the value RAW and unquoted -- not an
# error, not empty (an unknown modifier like #{zz:} is what returns empty), so a
# qs:-based bind provides zero quoting there and every assertion below would pass
# while proving nothing. Production ships the pinned next-3.8, and this test binds
# TMUX_BIN to that same wrapped binary; this guard is what keeps a future pin
# downgrade -- or a copy of this file pointed at pkgs.tmux -- from reading green.
@test "the tmux under test really wraps #{qs:} (guards a silent no-op)" {
	inner set-option -w -t "$WIN" @qs_probe "a'b c\$d ~/src x{a,b}"
	local wrapped
	wrapped="$(inner display-message -p -t "$WIN" '#{qs:@qs_probe}')"
	[ "$wrapped" = "'a'\\''b c\$d ~/src x{a,b}'" ]
}

@test "recorder captures a ctl frame and acks it" {
	# Hand-rolled FrameCtl: type byte 6, 4-byte big-endian length, then the argv.
	# The three leading zero bytes hard-code a length below 256, which is all this
	# fixture needs; assert it rather than silently truncating if someone grows it.
	local pl="$BATS_TEST_TMPDIR/handrolled"
	write_argv "$pl" 'hand-rolled'
	[ "$(stat -c %s "$pl")" -lt 256 ]
	{
		printf '\006\000\000\000'
		printf '%b' "$(printf '\\%03o' "$(stat -c %s "$pl")")"
		cat "$pl"
	} | socat - "UNIX-CONNECT:$SOCK" >"$BATS_TEST_TMPDIR/ack"
	wait_for_frame
	local p
	p="$(sole_payload)"
	assert_wire_argv "$p" 'hand-rolled'
	# The ack is what keeps ctl from reporting a protocol error into the client.
	[ "$(stat -c %s "$BATS_TEST_TMPDIR/ack")" -eq 5 ]
	[ "$(cat "$(dirname "$p")/type")" -eq 6 ]

	# ...and the real binary is satisfied by that ack.
	clear_frames
	run "$CTL" --sock="$SOCK" rename "$BRIDGE_PANE" 'harness self-test'
	[ "$status" -eq 0 ]
	[ -z "$output" ]
	wait_for_frame
	assert_wire_argv "$(sole_payload)" 'harness self-test'
}

# --- 7b.1: the discriminating test -------------------------------------------

@test "a #() in the prefilled name never executes locally" {
	attach_client
	local sentinel="$BATS_TEST_TMPDIR/sentinel-neg"
	inner set-option -w -t "$WIN" @window_bridge_name "probe#(touch $sentinel)"

	press_prefix_key ','
	wait_for_prompt 'probe#('
	send Enter
	wait_for_frame

	run ! wait_for_sentinel "$sentinel" "$SENTINEL_NEGATIVE_SECS"

	# ...and the name still travelled, verbatim, as data.
	assert_wire_argv "$(sole_payload)" "probe#(touch $sentinel)"
}

@test "positive control: the same #() through an unprotected run-shell does execute" {
	attach_client
	local sentinel="$BATS_TEST_TMPDIR/sentinel-pos"
	inner set-option -w -t "$WIN" @window_bridge_name "probe#(touch $sentinel)"
	# The pre-fix shape: the prompt result spliced into run-shell's command string,
	# where run-shell format-expands it. If this does NOT fire, the harness cannot
	# observe the vulnerability and the test above is worthless.
	inner bind-key -T prefix F1 command-prompt "-I#{@window_bridge_name}" 'run-shell "echo %%"'

	press_prefix_key F1
	wait_for_prompt 'probe#('
	send Enter

	wait_for_sentinel "$sentinel"
}

# --- 7b.2 + 7b.3: the name arrives verbatim, and the prompt showed it ---------

@test "prefill shows the remote name and the wire argv carries it verbatim" {
	attach_client
	local name
	# The last three catch a regression from #{qs:1} back to #{q:1}: format_quote_shell
	# leaves ~, { and } unescaped and never wraps, so ~/src arrives as the LOCAL
	# home directory and x{a,b} splits into two argv words.
	for name in "it's" 'a$HOME' 'my remote window' 'pr##367' 'café-日本' '~/src' '~root' 'x{a,b}'; do
		clear_frames
		inner set-option -w -t "$WIN" @window_bridge_name "$name"
		press_prefix_key ','
		wait_for_prompt "$name"
		send Enter
		wait_for_frame
		assert_wire_argv "$(sole_payload)" "$name"
	done
}

# The ordinary path: the user clears the prefill and types a name. Covered
# separately because every other case here submits the prefill unedited, and the
# decode on the inbound side (ctl.go's rename verb) applies to whatever comes
# back -- edited or not. Typed `##` collapsing to `#` is the escaped-dialect wart
# the prompt has always had: pre-fix, run-shell's own format expansion collapsed
# it before ctl ever saw it, so this behaviour is unchanged, not introduced.
@test "a name typed over the prefill arrives verbatim" {
	attach_client
	local typed
	for typed in 'my-new-name' 'issue#42' "o'brien" 'a b c'; do
		clear_frames
		inner set-option -w -t "$WIN" @window_bridge_name 'prefilled'
		press_prefix_key ','
		wait_for_prompt 'prefilled'
		send C-u
		sleep 0.3
		send -l "$typed"
		send Enter
		wait_for_frame
		assert_wire_argv "$(sole_payload)" "$typed"
	done
}

# --- 7b.4: the empty result, asserted at byte level ---------------------------

@test "a cleared prompt delivers one empty argv word, not a missing one" {
	attach_client
	inner set-option -w -t "$WIN" @window_bridge_name 'something'
	press_prefix_key ','
	wait_for_prompt 'something'
	send C-u
	sleep 0.3
	send Enter
	wait_for_frame

	local p
	p="$(sole_payload)"
	assert_wire_argv "$p" ""
	# Spelled out, because this is the assertion a field-splitting reading fakes:
	# "<ver>\0rename\0%42\0" carries 3 NULs; dropping the empty word leaves 2 and
	# one byte fewer, and both split into the same three fields.
	[ "$(stat -c %s "$p")" -eq $((${#CTL_PROTOCOL_VERSION} + 12)) ]
	[ "$(tr -dc '\000' <"$p" | wc -c)" -eq 3 ]
}
