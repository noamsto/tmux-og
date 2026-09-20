#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# pi-relaunch-stamp: hookyard's normalized envelope → @remux_relaunch replay
# transform (scripts/pi-relaunch-stamp.sh). The stamper gates its write on a read-back of
# the current value, so the fake tmux is STATEful, not a log-only spy: it stores the value
# each `set-option` writes (per pane) and answers `show-options` from that store, so "the
# second identical invocation writes nothing" is a real assertion rather than an artifact
# of a fake that never answers. Every argv line is also logged for the no-write checks.

setup() {
	# Tests run from a plain interactive pi launch unless a case opts in —
	# unset rather than assume empty, since a dispatched worker pane (this
	# one, possibly) already has this in its real environment.
	unset CREW_WORKER_ID PI_USER_ARGC
	export TMUX_PANE="%7"
	STAMP="$BATS_TEST_TMPDIR/pi-relaunch-stamp.sh"
	sed "s|@jq@|$(command -v jq)|" "$BATS_TEST_DIRNAME/../scripts/pi-relaunch-stamp.sh" >"$STAMP"
	chmod +x "$STAMP"
	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	export TMUX_STORE="$BATS_TEST_TMPDIR/store"
	mkdir -p "$TMUX_STORE"
	mkdir -p "$BATS_TEST_TMPDIR/bin"
	cat >"$BATS_TEST_TMPDIR/bin/tmux" <<'EOF'
#!/bin/sh
# Log every invocation first, then serve the two shapes the stamper uses.
printf '%s\n' "$*" >>"$TMUX_LOG"
verb="$1"
pane=""
optname=""
value=""
while [ $# -gt 0 ]; do
	a=$1
	shift
	case "$a" in
	-t)
		pane=$1
		shift
		;;
	-*) ;;
	@remux_relaunch) optname=$a ;;
	*) value=$a ;;
	esac
done
mkdir -p "$TMUX_STORE"
case "$verb" in
set-option | set)
	[ -n "$pane" ] && [ -n "$optname" ] && printf '%s' "$value" >"$TMUX_STORE/$pane"
	;;
show-options | show)
	# Unset must fail quietly, like the real `-q` read: the stamper treats a
	# failure as "no stamp yet" and writes.
	if [ -f "$TMUX_STORE/$pane" ]; then
		cat "$TMUX_STORE/$pane"
	else
		exit 1
	fi
	;;
esac
EOF
	chmod +x "$BATS_TEST_TMPDIR/bin/tmux"
	export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
	SESS="$BATS_TEST_TMPDIR/sessions/--repo--/2026-09-16T00-00-00-000Z_01a0aa66-2252-76fc-a266-37d19900b74d.jsonl"
	mkdir -p "$BATS_TEST_TMPDIR/sessions/--repo--"
}

# set_lines — the exact values the stamper asked tmux to store, all of them.
set_lines() {
	grep -F 'set-option' "$TMUX_LOG" | sed 's/^set-option -p -t %7 @remux_relaunch //'
}

stamp() {
	local session_file="$1"
	shift
	jq -cn --arg session_file "$session_file" --args \
		'{native: {session_file: $session_file, argv: $ARGS.positional}}' -- "$@" | bash "$STAMP"
}

stamp_json() {
	printf '%s' "$1" | bash "$STAMP"
}

@test "session_start and turn_end envelopes both stamp a persisted session" {
	run stamp "$SESS" --name reef
	[ "$status" -eq 0 ]
	[ "$(set_lines)" = "pi '--name' 'reef' --session '$SESS'" ]

	: >"$TMUX_LOG"
	run stamp "$SESS" --name lagoon
	[ "$status" -eq 0 ]
	[ "$(set_lines)" = "pi '--name' 'lagoon' --session '$SESS'" ]
}

@test "malformed, missing, null, empty, and NUL envelope fields are no-ops" {
	local payload
	for payload in \
		'not json' \
		'{}' \
		'{"native":{}}' \
		'{"native":{"session_file":null,"argv":[]}}' \
		'{"native":{"session_file":"session","argv":null}}' \
		'{"native":{"session_file":"session","argv":[null]}}' \
		'{"native":{"session_file":"session","argv":[],"user_argv":null}}' \
		'{"native":{"session_file":"\u0000","argv":[]}}' \
		'{"native":{"session_file":"session","argv":["\u0000"]}}'; do
		run stamp_json "$payload"
		[ "$status" -eq 0 ]
		run ! grep -q '^\(show-options\|set-option\)' "$TMUX_LOG"
	done
}

@test "dispatcher-shaped argv: flags replayed, launch prompt dropped, --session appended" {
	run stamp "$SESS" --name reef --model opencode/deepseek-v4-flash --thinking high --no-approve "resume me"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'opencode/deepseek-v4-flash' '--thinking' 'high' '--no-approve' --session '$SESS'" ]
	# The prompt words must never reach the stamp — they are the one thing a
	# restore must not re-send.
	run ! grep -qF 'resume me' "$TMUX_LOG"
}

@test "@file designators are dropped with the other positionals" {
	run stamp "$SESS" --print @notes.md "some message"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF '@notes.md' "$TMUX_LOG"
	run ! grep -qF 'some message' "$TMUX_LOG"
	[ "$stamped" = "pi '--print' --session '$SESS'" ]
}

@test "existing session-selection flags are removed, only the appended --session survives" {
	run stamp "$SESS" --session old.jsonl --continue --resume -c -r --session-id X --fork Y --name reef

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$(printf '%s' "$stamped" | grep -o -- '--session' | wc -l)" -eq 1 ]
	run ! grep -qF 'old.jsonl' "$TMUX_LOG"
	run ! grep -qF -- '--fork' "$TMUX_LOG"
	run ! grep -qF -- '--session-id' "$TMUX_LOG"
	run ! grep -qF -- '--continue' "$TMUX_LOG"
	run ! grep -qF -- '--resume' "$TMUX_LOG"
	[ "$stamped" = "pi '--name' 'reef' --session '$SESS'" ]
}

@test "--opt=value form: session forms dropped, kept options pass through whole" {
	run stamp "$SESS" --name=reef --session=old.jsonl --model=x

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF 'old.jsonl' "$TMUX_LOG"
	[ "$stamped" = "pi '--name=reef' '--model=x' --session '$SESS'" ]
}

@test "a value-taking option consumes its next element even when it starts with -" {
	run stamp "$SESS" --name -weird --model y

	[ "$status" -eq 0 ]
	run set_lines
	# '-weird' is --name's VALUE, not the '-weird' flag — the stamp must replay it.
	[ "$output" = "pi '--name' '-weird' '--model' 'y' --session '$SESS'" ]
}

@test "args with spaces and single quotes are single-quoted with the \\'\\'' trick" {
	run stamp "$SESS" --append-system-prompt "it's a 'path' with spaces" --model "x y"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--append-system-prompt' 'it'\''s a '\''path'\'' with spaces' '--model' 'x y' --session '$SESS'" ]
}

@test "a | anywhere in the assembled command refuses to stamp" {
	run stamp "$SESS" --name "a|b" --model y

	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "control bytes that fall inside POSIX [:space:] still refuse to stamp" {
	# CR, VT and FF are space-class, so a naive 'printable-or-space' reject
	# lets them through (#661 review finding) — the cntrl-class reject covers
	# C0 + DEL. Pin the gap class, and a non-space C0 byte for good measure.
	cr="$(printf 'a\rb')" # shell injection can't happen (single-quoted), the
	# tmux format reader still mangles it — reject whole.
	run stamp "$SESS" --name "$cr"
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"

	: >"$TMUX_LOG"
	run stamp "$SESS" --name "$(printf 'a\x01b')"
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "bridge-sanitized argv does not carry a secret flag into the stamp" {
	run stamp "$SESS" --model y --name reef

	[ "$status" -eq 0 ]
	run ! grep -qF -- '--api-key' "$TMUX_LOG"
	[ "$(set_lines)" = "pi '--model' 'y' '--name' 'reef' --session '$SESS'" ]
}

@test "forged envelopes cannot persist separate or attached secret flags" {
	run stamp_json '{"native":{"session_file":"session","argv":["--api-key","opaque-value","--name","reef"]}}'
	[ "$status" -eq 0 ]
	[ "$(set_lines)" = "pi '--name' 'reef' --session 'session'" ]
	run ! grep -qF 'opaque-value' "$TMUX_LOG"

	: >"$TMUX_LOG"
	run stamp_json '{"native":{"session_file":"session","argv":["--name","reef"],"user_argv":["--access-token=opaque-value","--name","lagoon"]}}'
	[ "$status" -eq 0 ]
	[ "$(set_lines)" = "pi '--name' 'lagoon' --session 'session'" ]
	run ! grep -qF 'opaque-value' "$TMUX_LOG"
}

@test "empty session file (--no-session) is a no-op with no tmux call at all" {
	run stamp "" --print hi

	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "change-gate: identical re-stamp writes nothing, a changed session file writes again" {
	run stamp "$SESS" --name reef
	[ "$(grep -c '^set-option' "$TMUX_LOG")" -eq 1 ]

	# Second run, same argv + file: the store-backed show() reads back the same
	# value, so the stamper must not issue another set.
	: >"$TMUX_LOG"
	run stamp "$SESS" --name reef
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"

	# A different session file changes the command; the gate lets it through.
	: >"$TMUX_LOG"
	OTHER="$BATS_TEST_TMPDIR/sessions/--repo--/2026-09-16T01-00-00-000Z_01a0aa66-3333-76fc-a266-37d19900b74d.jsonl"
	run stamp "$OTHER" --name reef
	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' --session '$OTHER'" ]
}

@test "no-op when TMUX_PANE is unset" {
	unset TMUX_PANE
	run stamp "$SESS" --print hi
	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "PI_USER_ARGC drops the wrapper-injected prefix, keeping only the trailing user args" {
	PI_USER_ARGC=5 run stamp "$SESS" -e /nix/store/abc-hook-bridge.ts --skill /home/x/.claude/skills \
		--name reef --model y "resume me"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF '/nix/store/abc-hook-bridge.ts' "$TMUX_LOG"
	run ! grep -qF -- '--skill' "$TMUX_LOG"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'y' --session '$SESS'" ]
}

@test "sanitized user argv preserves the wrapper boundary after a secret pair is stripped" {
	PI_USER_ARGC=4 run stamp_json '{"native":{"session_file":"session","argv":["-e","/nix/store/bridge.ts","--name","reef"],"user_argv":["--name","reef"]}}'

	[ "$status" -eq 0 ]
	[ "$(set_lines)" = "pi '--name' 'reef' --session 'session'" ]
	run ! grep -qF '/nix/store/bridge.ts' "$TMUX_LOG"
}

@test "PI_USER_ARGC=0 drops every argv entry, stamping only --session" {
	PI_USER_ARGC=0 run stamp "$SESS" -e /nix/store/abc-hook-bridge.ts --skill /home/x/.claude/skills

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi --session '$SESS'" ]
}

@test "PI_USER_ARGC unset replays everything, same as today" {
	run stamp "$SESS" -e /nix/store/abc-hook-bridge.ts --skill /home/x/.claude/skills --name reef

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '-e' '/nix/store/abc-hook-bridge.ts' '--skill' '/home/x/.claude/skills' '--name' 'reef' --session '$SESS'" ]
}

@test "malformed PI_USER_ARGC (non-integer) falls back to replay-all" {
	PI_USER_ARGC=nope run stamp "$SESS" --name reef --model y

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'y' --session '$SESS'" ]
}

@test "PI_USER_ARGC with a leading zero falls back to replay-all (bash would read it as octal)" {
	# "010" fed straight into (( )) arithmetic is octal 8, not decimal 10 —
	# a zero-padded value is never what the wrapper's own $# produces, so
	# reject it whole rather than silently drop real user args.
	PI_USER_ARGC=010 run stamp "$SESS" --name reef --model y

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'y' --session '$SESS'" ]
}

@test "PI_USER_ARGC larger than the available args falls back to replay-all" {
	PI_USER_ARGC=99 run stamp "$SESS" --name reef --model y

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'y' --session '$SESS'" ]
}

@test "CREW_WORKER_ID=worker:… stamps dispatch resume, ignoring argv entirely" {
	CREW_WORKER_ID='worker:feat/661-x#s123' run stamp "$SESS" -e /nix/store/abc-hook-bridge.ts --name reef

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "dispatch resume" ]
	run ! grep -qF 'nix/store' "$TMUX_LOG"
}

@test "CREW_WORKER_ID=worker:… with no session file is a no-op" {
	CREW_WORKER_ID='worker:feat/661-x#s123' run stamp "" --print hi

	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "CREW_WORKER_ID=role:… is a no-op, no tmux call at all" {
	CREW_WORKER_ID='role:feat/661-x:reviewer' run stamp "$SESS" --name reef

	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "no-op when tmux is absent" {
	mkdir -p "$BATS_TEST_TMPDIR/empty"
	local bash_bin
	bash_bin="$(command -v bash)"
	local envelope
	envelope="$(jq -cn --arg session_file "$SESS" --args '{native: {session_file: $session_file, argv: $ARGS.positional}}' -- --print hi)"
	# shellcheck disable=SC2016 # The child shell expands its positional arguments.
	run env PATH="$BATS_TEST_TMPDIR/empty" "$bash_bin" -c 'printf %s "$1" | "$3" "$2"' _ "$envelope" "$STAMP" "$bash_bin"
	[ "$status" -eq 0 ]
	[ ! -f "$TMUX_LOG" ]
}
