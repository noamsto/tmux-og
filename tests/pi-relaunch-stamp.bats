#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# pi-relaunch-stamp: argv → @remux_relaunch replay transform (scripts/pi-relaunch-stamp.sh,
# driven by plugins/pi-relaunch-stamp.ts). The stamper gates its write on a read-back of
# the current value, so the fake tmux is STATEful, not a log-only spy: it stores the value
# each `set-option` writes (per pane) and answers `show-options` from that store, so "the
# second identical invocation writes nothing" is a real assertion rather than an artifact
# of a fake that never answers. Every argv line is also logged for the no-write checks.

setup() {
	export TMUX_PANE="%7"
	STAMP="$BATS_TEST_DIRNAME/../scripts/pi-relaunch-stamp.sh"
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

@test "dispatcher-shaped argv: flags replayed, launch prompt dropped, --session appended" {
	run bash "$STAMP" "$SESS" --name reef --model opencode/deepseek-v4-flash --thinking high --no-approve "resume me"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' '--model' 'opencode/deepseek-v4-flash' '--thinking' 'high' '--no-approve' --session '$SESS'" ]
	# The prompt words must never reach the stamp — they are the one thing a
	# restore must not re-send.
	run ! grep -qF 'resume me' "$TMUX_LOG"
}

@test "@file designators are dropped with the other positionals" {
	run bash "$STAMP" "$SESS" --print @notes.md "some message"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF '@notes.md' "$TMUX_LOG"
	run ! grep -qF 'some message' "$TMUX_LOG"
	[ "$stamped" = "pi '--print' --session '$SESS'" ]
}

@test "existing session-selection flags are removed, only the appended --session survives" {
	run bash "$STAMP" "$SESS" --session old.jsonl --continue --resume -c -r --session-id X --fork Y --name reef

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
	run bash "$STAMP" "$SESS" --name=reef --session=old.jsonl --model=x

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF 'old.jsonl' "$TMUX_LOG"
	[ "$stamped" = "pi '--name=reef' '--model=x' --session '$SESS'" ]
}

@test "a value-taking option consumes its next element even when it starts with -" {
	run bash "$STAMP" "$SESS" --name -weird --model y

	[ "$status" -eq 0 ]
	run set_lines
	# '-weird' is --name's VALUE, not the '-weird' flag — the stamp must replay it.
	[ "$output" = "pi '--name' '-weird' '--model' 'y' --session '$SESS'" ]
}

@test "args with spaces and single quotes are single-quoted with the \\'\\'' trick" {
	run bash "$STAMP" "$SESS" --append-system-prompt "it's a 'path' with spaces" --model "x y"

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--append-system-prompt' 'it'\''s a '\''path'\'' with spaces' '--model' 'x y' --session '$SESS'" ]
}

@test "a | anywhere in the assembled command refuses to stamp" {
	run bash "$STAMP" "$SESS" --name "a|b" --model y

	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "control bytes that fall inside POSIX [:space:] still refuse to stamp" {
	# CR, VT and FF are space-class, so a naive 'printable-or-space' reject
	# lets them through (#661 review finding) — the cntrl-class reject covers
	# C0 + DEL. Pin the gap class, and a non-space C0 byte for good measure.
	cr="$(printf 'a\rb')" # shell injection can't happen (single-quoted), the
	# tmux format reader still mangles it — reject whole.
	run bash "$STAMP" "$SESS" --name "$cr"
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"

	: >"$TMUX_LOG"
	run bash "$STAMP" "$SESS" --name "$(printf 'a\x01b')"
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "--api-key is dropped with its value, like the session-selection flags" {
	# The key would be persisted in the pane option / state.db and re-exposed
	# per restore; a keyed launch restores via the provider env var instead.
	run bash "$STAMP" "$SESS" --model y --api-key sk-not-a-real-key --name reef

	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	run ! grep -qF 'sk-not-a-real-key' "$TMUX_LOG"
	run ! grep -qF -- '--api-key' "$TMUX_LOG"
	[ "$stamped" = "pi '--model' 'y' '--name' 'reef' --session '$SESS'" ]
}

@test "empty session file (--no-session) is a no-op with no tmux call at all" {
	run bash "$STAMP" "" --print hi

	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "change-gate: identical re-stamp writes nothing, a changed session file writes again" {
	run bash "$STAMP" "$SESS" --name reef
	[ "$(grep -c '^set-option' "$TMUX_LOG")" -eq 1 ]

	# Second run, same argv + file: the store-backed show() reads back the same
	# value, so the stamper must not issue another set.
	: >"$TMUX_LOG"
	run bash "$STAMP" "$SESS" --name reef
	[ "$status" -eq 0 ]
	run ! grep -q '^set-option' "$TMUX_LOG"

	# A different session file changes the command; the gate lets it through.
	: >"$TMUX_LOG"
	OTHER="$BATS_TEST_TMPDIR/sessions/--repo--/2026-09-16T01-00-00-000Z_01a0aa66-3333-76fc-a266-37d19900b74d.jsonl"
	run bash "$STAMP" "$OTHER" --name reef
	[ "$status" -eq 0 ]
	local stamped
	stamped="$(set_lines)"
	[ "$stamped" = "pi '--name' 'reef' --session '$OTHER'" ]
}

@test "no-op when TMUX_PANE is unset" {
	unset TMUX_PANE
	run bash "$STAMP" "$SESS" --print hi
	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_LOG" ]
}

@test "no-op when tmux is absent" {
	mkdir -p "$BATS_TEST_TMPDIR/empty"
	local bash_bin
	bash_bin="$(command -v bash)"
	run env PATH="$BATS_TEST_TMPDIR/empty" "$bash_bin" "$STAMP" "$SESS" --print hi
	[ "$status" -eq 0 ]
	[ ! -f "$TMUX_LOG" ]
}
