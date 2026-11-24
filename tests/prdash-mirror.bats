#!/usr/bin/env bats
# Tests scripts/tmux-prdash-mirror.sh: the local prdash launcher of a mirror window.
# prdash and the bridge ctl are stubs; the script runs against a private,
# config-less tmux server, with TMUX_PANE naming a pane of that server.

setup() {
	OG_TMUX_DIR="$(mktemp -d /tmp/og-pm-XXXXXX)"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	export XDG_DATA_HOME="$OG_TMUX_DIR/data"
	export CLAUDE_STATUS_DIR="$OG_TMUX_DIR/status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	unset TMUX TMUX_PANE
	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"
	REAL_TMUX="$(command -v tmux)"
	export REAL_TMUX

	# The script's mktemp -d lands here, so a leftover dir is visible.
	WORK_PARENT="$BATS_TEST_TMPDIR/tmp"
	mkdir -p "$WORK_PARENT"
	export TMPDIR="$WORK_PARENT"

	OUT="$BATS_TEST_TMPDIR/out"
	mkdir -p "$OUT" "$BATS_TEST_TMPDIR/bin" "$BATS_TEST_TMPDIR/shim"
	export OUT
	export OG_SHIM_LOG="$BATS_TEST_TMPDIR/shim.log"
	: >"$OG_SHIM_LOG"
	export FIXTURE="$BATS_TEST_TMPDIR/fixture"
	: >"$FIXTURE"

	cat >"$BATS_TEST_TMPDIR/bin/prdash" <<STUB
#!$BASH
printf '%s\n' "\$@" >"\$OUT/prdash.argv"
printf '%s\n' "\$PWD" >"\$OUT/prdash.pwd"
printf '%s\n' "\$PRDASH_ACTION_FILE" >"\$OUT/prdash.action"
printf '%s\n' "\$PRDASH_REPOS_ROOT" >"\$OUT/prdash.root"
if [[ -d \$PRDASH_REPOS_ROOT && -z \$(ls -A "\$PRDASH_REPOS_ROOT") ]]; then echo empty >"\$OUT/prdash.rootstate"; fi
cat "\$FIXTURE" >>"\$PRDASH_ACTION_FILE"
exit "\${PRDASH_STUB_STATUS:-0}"
STUB
	cat >"$BATS_TEST_TMPDIR/bin/ctl" <<STUB
#!$BASH
printf '%s\n' "\$@" >>"\$OUT/ctl.argv"
echo call >>"\$OUT/ctl.calls"
if [[ -n \${CTL_STUB_ERR:-} ]]; then echo "\$CTL_STUB_ERR" >&2; fi
exit "\${CTL_STUB_STATUS:-0}"
STUB
	chmod +x "$BATS_TEST_TMPDIR/bin/prdash" "$BATS_TEST_TMPDIR/bin/ctl"

	# A tmux in front of the real one that logs notifications.
	printf '#!%s\n' "$BASH" >"$BATS_TEST_TMPDIR/shim/tmux"
	cat >>"$BATS_TEST_TMPDIR/shim/tmux" <<'EOF'
if [[ ${1:-} == display-message && ${2:-} != -p ]]; then
	printf '%s\n' "$*" >>"$OG_SHIM_LOG"
fi
exec "$REAL_TMUX" "$@"
EOF
	chmod +x "$BATS_TEST_TMPDIR/shim/tmux"
	PATH="$BATS_TEST_TMPDIR/shim:$PATH"

	tmux -f /dev/null new-session -d -s S -x 200 -y 50 -c "$BATS_TEST_TMPDIR"
	PANE="$(tmux display-message -p -t S '#{pane_id}')"
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	CLIENT="$(tmux display-message -p -t "$PANE" '#{client_name}')"
}

teardown() {
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	"$REAL_TMUX" kill-server 2>/dev/null || true
	rm -rf -- "${OG_TMUX_DIR:-/nonexistent}"
}

# stamp [repo] [origin]: the options a mirror window and its session carry.
stamp() {
	[[ -z ${1:-} ]] || tmux set-option -w -t "$WIN" @bridge_repo "$1"
	[[ -z ${2:-} ]] || tmux set-option -w -t "$WIN" @og_prdash_origin "$2"
	tmux set-option -w -t "$WIN" @bridge_dir /r/d
	tmux set-option -t S @bridge_sock /s/sock
}

launch() { env TMUX_PANE="$PANE" bash scripts/tmux-prdash-mirror.sh "$BATS_TEST_TMPDIR/bin/prdash" "$BATS_TEST_TMPDIR/bin/ctl"; }

@test "runs prdash for the repo in a scratch dir and forwards the lines in one ctl call" {
	stamp o/n %5
	printf '%s\n' 'W|o/n#1|x' 'C|o/n#2' >"$FIXTURE"
	run launch
	[ "$status" -eq 0 ]
	[ "$(cat "$OUT/prdash.argv")" = "$(printf '%s\n' --repo o/n)" ]
	[ "$(cat "$OUT/prdash.pwd")" != "$PWD" ]
	[[ $(cat "$OUT/prdash.pwd") == "$WORK_PARENT"/* ]]
	[[ $(cat "$OUT/prdash.action") == "$WORK_PARENT"/* ]]
	[[ $(cat "$OUT/prdash.root") == "$WORK_PARENT"/* ]]
	[ -f "$OUT/prdash.rootstate" ]
	[ "$(wc -l <"$OUT/ctl.calls")" -eq 1 ]
	[ "$(cat "$OUT/ctl.argv")" = "$(printf '%s\n' "--display-error=$CLIENT" --sock=/s/sock handoff %5 /r/d 'W|o/n#1|x' 'C|o/n#2')" ]
	[ -z "$(ls -A "$WORK_PARENT")" ]
}

@test "a socket path holding a pipe reaches ctl whole and pane/dir stay correct" {
	stamp o/n %5
	tmux set-option -t S @bridge_sock '/s/a|b/sock'
	printf '%s\n' 'W|o/n#1|x' >"$FIXTURE"
	run launch
	[ "$status" -eq 0 ]
	[ "$(cat "$OUT/ctl.argv")" = "$(printf '%s\n' "--display-error=$CLIENT" '--sock=/s/a|b/sock' handoff %5 /r/d 'W|o/n#1|x')" ]
}

@test "no hand-off lines: ctl is not called" {
	stamp o/n %5
	run launch
	[ "$status" -eq 0 ]
	[ -f "$OUT/prdash.argv" ]
	[ ! -e "$OUT/ctl.calls" ]
	[ ! -s "$OG_SHIM_LOG" ]
}

@test "a line with shell syntax reaches ctl byte-identical and runs nothing" {
	stamp o/n %5
	# shellcheck disable=SC2016 # the hostile line must stay literal
	printf '%s\n' 'W|$(touch pwned)' >"$FIXTURE"
	local script="$PWD/scripts/tmux-prdash-mirror.sh"
	cd "$BATS_TEST_TMPDIR"
	run env TMUX_PANE="$PANE" bash "$script" "$BATS_TEST_TMPDIR/bin/prdash" "$BATS_TEST_TMPDIR/bin/ctl"
	[ "$status" -eq 0 ]
	# shellcheck disable=SC2016 # the hostile line must stay literal
	grep -qxF 'W|$(touch pwned)' "$OUT/ctl.argv"
	[ -z "$(find "$BATS_TEST_TMPDIR" -name pwned)" ]
}

@test "no @bridge_repo falls back to the remote prdash tool" {
	stamp "" %5
	run launch
	[ "$status" -eq 0 ]
	[ ! -e "$OUT/prdash.argv" ]
	[ "$(cat "$OUT/ctl.argv")" = "$(printf '%s\n' "--display-error=$CLIENT" --sock=/s/sock tool %5 prdash /r/d)" ]
}

@test "an invalid @bridge_repo falls back to the remote prdash tool" {
	stamp o %5
	run launch
	[ "$status" -eq 0 ]
	[ ! -e "$OUT/prdash.argv" ]
	[ "$(cat "$OUT/ctl.argv")" = "$(printf '%s\n' "--display-error=$CLIENT" --sock=/s/sock tool %5 prdash /r/d)" ]
}

@test "an invalid origin pane runs nothing and exits 0" {
	stamp o/n 'x;y'
	run launch
	[ "$status" -eq 0 ]
	[ ! -e "$OUT/prdash.argv" ]
	[ ! -e "$OUT/ctl.calls" ]
	[ -n "$(cat "$OG_SHIM_LOG")" ]
}

@test "more than 32 lines forwards nothing and says why" {
	stamp o/n %5
	seq 1 33 | sed 's/^/W|o\/n#/' >"$FIXTURE"
	run launch
	[ "$status" -eq 0 ]
	[ ! -e "$OUT/ctl.calls" ]
	grep -q 32 "$OG_SHIM_LOG"
}

@test "a failing prdash shows its status and forwards nothing" {
	stamp o/n %5
	printf '%s\n' 'W|o/n#1|x' >"$FIXTURE"
	PRDASH_STUB_STATUS=3 run launch
	[ "$status" -eq 0 ]
	[ ! -e "$OUT/ctl.calls" ]
	grep -q 'status 3' "$OG_SHIM_LOG"
}

@test "a failing ctl shows its last stderr line" {
	stamp o/n %5
	printf '%s\n' 'W|o/n#1|x' >"$FIXTURE"
	CTL_STUB_STATUS=1 CTL_STUB_ERR=$'first\nno such client' run launch
	[ "$status" -eq 0 ]
	grep -q 'hand-off failed: no such client' "$OG_SHIM_LOG"
	[ -z "$(grep first "$OG_SHIM_LOG" || true)" ]
}

@test "a failing ctl without stderr shows its exit status" {
	stamp o/n %5
	printf '%s\n' 'W|o/n#1|x' >"$FIXTURE"
	CTL_STUB_STATUS=7 run launch
	[ "$status" -eq 0 ]
	grep -q 'hand-off failed: exit status 7' "$OG_SHIM_LOG"
}

@test "a failing ctl whose error quotes a #(command) runs nothing" {
	stamp o/n %5
	printf '%s\n' 'W|o/n#1|x' >"$FIXTURE"
	# shellcheck disable=SC2016 # the hostile text must stay literal
	CTL_STUB_STATUS=1 CTL_STUB_ERR='bad target "#(touch '"$BATS_TEST_TMPDIR"'/pwned)"' run launch
	[ "$status" -eq 0 ]
	grep -q 'hand-off failed' "$OG_SHIM_LOG"
	sleep 1
	[ ! -e "$BATS_TEST_TMPDIR/pwned" ]
}
