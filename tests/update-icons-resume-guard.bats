#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# Regression test for the RESUME_CLAUDE stamper in tmux-update-icons.sh: it
# iterates claude_pane_ids() (panes/* UNION screen/*), so a Cursor/Codex pane
# that only has a screen/<id> file (agent-detect scrapes every known agent CLI
# unconditionally, regardless of which hooks are enabled) is yielded too. For
# such a pane read_pane_state never sets a transcript, so desired="" — and
# unless the stamper's guard is scoped to values it owns, that clobbers back
# to empty any @remux_relaunch a different agent's own hook just stamped.
#
# Runs the real script against a private, config-less tmux server (same
# pattern as update-icons-enrich-trigger.bats), so bare `tmux` calls inside it
# see a real pane, not a fake.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen"
	export TMPDIR="$TDIR"

	# update-icons throttles its presence sweep on CLAUDE_NOW % 5, and that sweep
	# reaps the very state files these tests write. Unpinned it fires on one run
	# in five, which is what made this suite flake (#373); pinning it makes every
	# run exercise the reap. Floored from the real clock, not a constant — the
	# fixtures below stamp `timestamp=$(date +%s)`, and a reader clock detached
	# from them would skew every staleness comparison.
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))

	# Fake reflow: update-icons kicks it on every branch change; a no-op is
	# fine here, this file only asserts on @remux_relaunch stamping.
	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	# Runnable update-icons with Nix placeholders resolved.
	UPDATE_ICONS="$TDIR/update-icons.sh"
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e 's|@MAX_ICONS@|5|g' \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	REPO="$TDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" branch -q -M main

	tmux -f /dev/null new-session -d -s S -c "$REPO" -x 200 -y 50
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done

	PANE_ID="$(tmux list-panes -t S -F '#{pane_id}')"
	PANE_ID="${PANE_ID#%}"
}

teardown() {
	# BATS_TEST_COMPLETED is unset only on failure. The interesting evidence is
	# whether the pane's state file survived the run and what the script said
	# while losing it — neither is recoverable afterwards, since bats removes
	# BATS_TEST_TMPDIR before --keep-failed can preserve the sandbox.
	if [ -z "${BATS_TEST_COMPLETED:-}" ]; then
		echo "state files: $(ls "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen" 2>&1)"
		echo "update-icons stderr: $(cat "$UI_LOG" 2>/dev/null)"
	fi
	tmux kill-server 2>/dev/null || true
}

run_update_icons() {
	# $2 = RESUME_CLAUDE ("on"), same arg tmux itself passes via #{@resume_claude}.
	# stderr is kept, not discarded: `tmux set -p` (no -q) is the only place a
	# lost @remux_relaunch write announces itself.
	UI_LOG="$TDIR/update-icons.log"
	bash "$UPDATE_ICONS" S on >/dev/null 2>"$UI_LOG" || true
}

# assert_relaunch EXPECTED — compares @remux_relaunch and, on mismatch, reports
# what it actually held. A bare `[ "$(tmux show -pv …)" = … ]` reported only
# tmux's "invalid option: @remux_relaunch" on stderr, which says the option is
# unset but never says what the test wanted (#366).
assert_relaunch() {
	local want="$1" got
	got=$(tmux show -pv -t "%$PANE_ID" @remux_relaunch 2>/dev/null) || got="<unset>"
	if [ "$got" != "$want" ]; then
		echo "@remux_relaunch: expected [$want], actual [$got]"
		return 1
	fi
}

# run_update_icons_with_carousel VALUE BIN — like run_update_icons, but passes
# VALUE as $4 (RESUME_CAROUSEL / #{@resume_carousel}) and BIN as
# CAROUSEL_RESTORE_BIN (a prefix env, not `export`, so the value never has to
# be read back through a shell variable — see the callers below). $3
# (SERVER_START) is left blank so the script falls back to its own
# display-message read against the real private server, same as
# run_update_icons's $3 does.
run_update_icons_with_carousel() {
	UI_LOG="$TDIR/update-icons.log"
	CAROUSEL_RESTORE_BIN="$2" bash "$UPDATE_ICONS" S on "" "$1" >/dev/null 2>"$UI_LOG" || true
}

@test "screen-only pane (no hook transcript) does not clobber another agent's relaunch stamp" {
	printf 'state=idle\ntimestamp=%s\n' "$(date +%s)" >"$CLAUDE_STATUS_DIR/screen/$PANE_ID"
	tmux set -p -t "%$PANE_ID" @remux_relaunch "cursor-agent --resume abc123"

	run_update_icons

	# The stamp surviving proves nothing on its own — a run that deleted the
	# state file leaves it untouched too, which is exactly how #373 hid here.
	[ -e "$CLAUDE_STATUS_DIR/screen/$PANE_ID" ]
	assert_relaunch "cursor-agent --resume abc123"
}

@test "screen-only pi pane's relaunch stamp survives (resumePi, #661)" {
	# agent-detect scrapes pi like every known agent CLI, so a pi pane is a
	# screen-only pane with no transcript — the same guard shape as the cursor
	# case above, pinned for the pi relaunch value. The fixture is a realistic
	# pi-relaunch-stamp output: replayed flags + appended --session <file>.
	printf 'state=idle\ntimestamp=%s\n' "$(date +%s)" >"$CLAUDE_STATUS_DIR/screen/$PANE_ID"
	tmux set -p -t "%$PANE_ID" @remux_relaunch "pi --name reef --no-approve --session '/home/u/.pi/agent/sessions/--slug--/2026-09-16T00-00-00-000Z_01a0aa66-2252-76fc-a266-37d19900b74d.jsonl'"

	run_update_icons

	[ -e "$CLAUDE_STATUS_DIR/screen/$PANE_ID" ]
	assert_relaunch "pi --name reef --no-approve --session '/home/u/.pi/agent/sessions/--slug--/2026-09-16T00-00-00-000Z_01a0aa66-2252-76fc-a266-37d19900b74d.jsonl'"
}

@test "hook pane with a transcript still stamps claude --resume <uuid>" {
	TRANSCRIPT="$TDIR/deadbeef-0000-0000-0000-000000000001.jsonl"
	: >"$TRANSCRIPT"
	{
		echo "state=processing"
		echo "timestamp=$(date +%s)"
		echo "session=work"
		echo "transcript=$TRANSCRIPT"
	} >"$CLAUDE_STATUS_DIR/panes/$PANE_ID"

	run_update_icons

	assert_relaunch "claude --resume deadbeef-0000-0000-0000-000000000001"
}

@test "a pane that moved from Codex/Cursor to a live Claude session overwrites the foreign stamp" {
	TRANSCRIPT="$TDIR/cafebabe-0000-0000-0000-000000000003.jsonl"
	: >"$TRANSCRIPT"
	{
		echo "state=processing"
		echo "timestamp=$(date +%s)"
		echo "session=work"
		echo "transcript=$TRANSCRIPT"
	} >"$CLAUDE_STATUS_DIR/panes/$PANE_ID"
	tmux set -p -t "%$PANE_ID" @remux_relaunch "codex resume abc123"

	run_update_icons

	assert_relaunch "claude --resume cafebabe-0000-0000-0000-000000000003"
}

@test "no write is issued when the stamp already matches the desired value" {
	TRANSCRIPT="$TDIR/11111111-0000-0000-0000-000000000002.jsonl"
	: >"$TRANSCRIPT"
	{
		echo "state=processing"
		echo "timestamp=$(date +%s)"
		echo "session=work"
		echo "transcript=$TRANSCRIPT"
	} >"$CLAUDE_STATUS_DIR/panes/$PANE_ID"
	tmux set -p -t "%$PANE_ID" @remux_relaunch "claude --resume 11111111-0000-0000-0000-000000000002"

	REAL_TMUX="$(command -v tmux)"
	SPY_DIR="$TDIR/tmux-spy"
	SPY_LOG="$TDIR/tmux-calls.log"
	mkdir -p "$SPY_DIR"
	: >"$SPY_LOG"
	cat >"$SPY_DIR/tmux" <<-SPYEOF
		#!/bin/sh
		printf '%s\n' "\$*" >>"$SPY_LOG"
		exec "$REAL_TMUX" "\$@"
	SPYEOF
	chmod +x "$SPY_DIR/tmux"

	PATH="$SPY_DIR:$PATH" run_update_icons

	# Same vacuity trap as test 1: "no write was issued" is satisfied by a run
	# that wiped the state dir, so pin the fixture's survival first.
	[ -e "$CLAUDE_STATUS_DIR/panes/$PANE_ID" ]
	assert_relaunch "claude --resume 11111111-0000-0000-0000-000000000002"
	run ! grep -q '^set .*@remux_relaunch' "$SPY_LOG"
}

@test "the presence sweep's own format survives a non-UTF-8 locale" {
	# Every other reap test feeds the parser rows it built itself, so the real -F
	# format was never exercised (#373). Two things make this one bite: the
	# format is lifted out of the built script rather than restated, so producer
	# and assertion cannot drift, and the locale is forced rather than inherited,
	# so a tab format is red under `nix develop` too — not only in the sandbox.
	: >"$CLAUDE_STATUS_DIR/panes/$PANE_ID"
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh

	local fmt rows
	# Addressed on the assignment, not a bare "list-panes -a -F": a comment above
	# it that happens to quote the command would otherwise win the match and
	# leave $fmt empty.
	fmt=$(sed -n "/rows=\$(tmux list-panes -a -F/{s/.*-F '\([^']*\)'.*/\1/p;q;}" "$UPDATE_ICONS")
	[ -n "$fmt" ]
	rows=$(env -u LANG -u LC_ALL -u LC_CTYPE tmux list-panes -a -F "$fmt")

	claude_reap_dead_panes "$rows"

	[ -e "$CLAUDE_STATUS_DIR/panes/$PANE_ID" ] ||
		{ echo "reap deleted a live pane; rows were [$rows]" && false; }
}

@test "a failed @remux_relaunch write is loud" {
	# `tmux set -pq` returned 0 and printed nothing on a bad target, so a lost
	# write could only ever surface downstream as "invalid option" (#373). The
	# spy redirects the stamper's write to a pane that cannot exist; without -q
	# tmux must say so, and run_update_icons must not swallow it.
	TRANSCRIPT="$TDIR/aaaaaaaa-0000-0000-0000-000000000004.jsonl"
	: >"$TRANSCRIPT"
	{
		echo "state=processing"
		echo "timestamp=$(date +%s)"
		echo "transcript=$TRANSCRIPT"
	} >"$CLAUDE_STATUS_DIR/panes/$PANE_ID"

	# The spy rewrites only the -t target and forwards the script's own argv, so
	# what gets tested is the script's flags. A spy that issued its own command
	# would pass with -q restored — it would be asserting about itself.
	REAL_TMUX="$(command -v tmux)"
	SPY_DIR="$TDIR/tmux-spy"
	mkdir -p "$SPY_DIR"
	# /bin/sh, not /usr/bin/env: the Nix flake-check sandbox has no /usr/bin.
	# Rotates argv once, swapping only the -t operand, so every other flag the
	# script passed (notably the absence of -q) reaches tmux untouched.
	cat >"$SPY_DIR/tmux" <<-SPYEOF
		#!/bin/sh
		# Anchored on the verb: a future read-back via \`show -pv … @remux_relaunch\`
		# must not get its target rewritten too, or this test would go green on a
		# silent write again — the exact hole it exists to close.
		case "\$1:\$* " in
		set:*" @remux_relaunch "*)
			n=\$#; prev=
			while [ "\$n" -gt 0 ]; do
				a=\$1; shift
				if [ "\$prev" = -t ]; then set -- "\$@" %999; else set -- "\$@" "\$a"; fi
				prev=\$a
				n=\$((n - 1))
			done ;;
		esac
		exec "$REAL_TMUX" "\$@"
	SPYEOF
	chmod +x "$SPY_DIR/tmux"

	PATH="$SPY_DIR:$PATH" run_update_icons

	# Match the pane id, not tmux's wording — the id is ours, the message is not.
	grep -q '%999' "$UI_LOG" ||
		{ echo "write was silent; stderr was [$(cat "$UI_LOG")]" && false; }
}

# --- RESUME_CAROUSEL (carousel viewer restore, #577) ---
# A viewer pane has no claude-status state file, so this is a separate pass
# over the same batched list-panes read, gated on @claude_img_src rather than
# a pane file. These tests never touch CLAUDE_STATUS_DIR for that reason.

@test "carousel viewer pane gets @remux_relaunch stamped when RESUME_CAROUSEL is on" {
	local bin="$TDIR/store/tmux-carousel-restore"
	tmux set -p -t "%$PANE_ID" @claude_img_src "12345-$PANE_ID"

	run_update_icons_with_carousel on "$bin"

	# The stamp carries the host pane's CURRENT index, which is what lets the
	# restored script break ties between agent panes instead of guessing. Here
	# @claude_img_src names this very pane, so the index is its own.
	idx=$(tmux display-message -p -t "%$PANE_ID" '#{pane_index}')
	assert_relaunch "$bin $idx"
}

@test "no write is issued when the carousel stamp already matches" {
	local bin="$TDIR/store/tmux-carousel-restore"
	tmux set -p -t "%$PANE_ID" @claude_img_src "12345-$PANE_ID"
	idx=$(tmux display-message -p -t "%$PANE_ID" '#{pane_index}')
	tmux set -p -t "%$PANE_ID" @remux_relaunch "$bin $idx"

	REAL_TMUX="$(command -v tmux)"
	SPY_DIR="$TDIR/tmux-spy-carousel"
	SPY_LOG="$TDIR/tmux-calls-carousel.log"
	mkdir -p "$SPY_DIR"
	: >"$SPY_LOG"
	cat >"$SPY_DIR/tmux" <<-SPYEOF
		#!/bin/sh
		printf '%s\n' "\$*" >>"$SPY_LOG"
		exec "$REAL_TMUX" "\$@"
	SPYEOF
	chmod +x "$SPY_DIR/tmux"

	PATH="$SPY_DIR:$PATH" run_update_icons_with_carousel on "$bin"

	# Same vacuity trap as the Claude-stamp version of this test: the stamp
	# surviving is checked first, so a run that wiped it can't pass by omission.
	assert_relaunch "$bin $idx"
	run ! grep -q '^set .*@remux_relaunch' "$SPY_LOG"
}

@test "RESUME_CAROUSEL off, or the arg omitted entirely, stamps nothing" {
	local bin="$TDIR/store/tmux-carousel-restore"
	tmux set -p -t "%$PANE_ID" @claude_img_src "12345-$PANE_ID"

	run_update_icons_with_carousel off "$bin"
	assert_relaunch "<unset>"

	# The arg omitted entirely, as the run-shell hook invocation
	# (config/tmux.conf.nix) does — run_update_icons never passes a 4th argv.
	# CAROUSEL_RESTORE_BIN is supplied anyway: left unset it defaults to the
	# literal "@carousel_restore@", which trips the independent placeholder
	# guard on its own, so this case would pass even with the $RESUME_CAROUSEL
	# check deleted. Passing a real path isolates the missing-$4 default as the
	# only thing that can keep the stamp away.
	CAROUSEL_RESTORE_BIN="$bin" run_update_icons
	assert_relaunch "<unset>"
}

@test "a pane with no @claude_img_src is never stamped as a carousel viewer" {
	run_update_icons_with_carousel on "$TDIR/store/tmux-carousel-restore"

	assert_relaunch "<unset>"
}

@test "the carousel stamp is a bare value: no VAR=value prefix, no shell metacharacters" {
	local bin="$TDIR/store/abc123-tmux-carousel-restore/bin/tmux-carousel-restore"
	tmux set -p -t "%$PANE_ID" @claude_img_src "12345-$PANE_ID"

	run_update_icons_with_carousel on "$bin"

	# fact 7(a): the stamp must be valid under fish as well as POSIX sh — no
	# VAR=value env prefix and no shell metacharacter — checked on the value
	# tmux actually holds, not on the fixture it was set to.
	got=$(tmux show -pv -t "%$PANE_ID" @remux_relaunch 2>/dev/null) || got="<unset>"
	case "$got" in
	*[A-Za-z_]=*) echo "stamp carries a VAR=value token: [$got]" && false ;;
	esac
	# An absolute path, then at most a bare integer (the host pane index). The
	# space is the only whitespace allowed and nothing else may appear: fish
	# rejects a VAR=value prefix outright, and any of ;|&$`()<>*?" would be
	# interpreted by every shell tmux-remux might hand this to.
	[[ $got =~ ^[A-Za-z0-9/_.-]+( [0-9]+)?$ ]] ||
		{ echo "stamp is not a bare path plus optional index: [$got]" && false; }
}
