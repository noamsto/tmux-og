#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# Coverage for scripts/tmux-carousel-restore.sh (#577): host discovery, the
# key formula, and the two exit-without-stamping paths. Runs the real script
# against a private, config-less tmux server (same pattern as
# update-icons-resume-guard.bats) — @carousel_aeye@ and @AGENT_COMMANDS@ both
# have documented test-seam env overrides (AEYE_BIN / AGENT_COMMANDS), so no
# sed pass over the script is needed.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX

	SCRIPT="$PWD/scripts/tmux-carousel-restore.sh"

	tmux -f /dev/null new-session -d -s S -c "$TDIR" -x 200 -y 50
	tmux set -g base-index 0

	# Reconstruct the real "<socket>,<server pid>,<session>" shape a client's
	# $TMUX would carry — the script parses the pid out of it, and every bare
	# `tmux` call it makes must also resolve to THIS server. tmux picks the
	# socket from $TMUX's first field over TMUX_TMPDIR whenever $TMUX is set,
	# so a placeholder first field (e.g. "x") would break every one of the
	# script's own tmux calls, not just the pid parse.
	SRV_PID="$(tmux display-message -p '#{pid}')"
	TMUX="$TMUX_TMPDIR/tmux-$(id -u)/default,$SRV_PID,0"
	export TMUX

	# Stub viewer: a documented test seam (AEYE_BIN), never PATH — the script
	# never consults PATH for the viewer. Records its argv and AEYE_HOST_PANE
	# so tests can assert both the computed key and the exported host.
	AEYE_STUB="$TDIR/aeye-stub"
	AEYE_LOG="$TDIR/aeye-stub.log"
	: >"$AEYE_LOG"
	cat >"$AEYE_STUB" <<-EOF
		#!/bin/sh
		printf '%s\n' "\$*" >>"$AEYE_LOG"
		printf 'HOST=%s\n' "\$AEYE_HOST_PANE" >>"$AEYE_LOG"
	EOF
	chmod +x "$AEYE_STUB"
	export AEYE_BIN="$AEYE_STUB"
	export AGENT_COMMANDS="claude codex cursor-agent"
}

teardown() {
	if [ -z "${BATS_TEST_COMPLETED:-}" ]; then
		echo "panes: $(tmux list-panes -a -F '#{pane_index}|#{pane_id}|#{pane_current_command}' 2>&1)"
	fi
	tmux kill-server 2>/dev/null || true
}

# make_fake_agent NAME — a renamed copy of bash, not sleep: coreutils' sleep
# is a multicall binary that dispatches on argv[0]/its own filename and exits
# immediately ("coreutils: unknown program 'NAME'") when renamed, which reads
# as a live agent pane for one tick and then a dead one. bash has no such
# dispatch, so a renamed copy runs fine; `-c 'read -r -t 300'` blocks on a
# builtin (no further exec) so #{pane_current_command} keeps reporting NAME,
# not "read" or "sleep".
make_fake_agent() {
	cp "$(command -v bash)" "$TDIR/$1"
	chmod +x "$TDIR/$1"
}

# spawn_pane NAME — splits window S:0, running the fake agent binary NAME
# (from make_fake_agent) as that pane's command.
spawn_pane() {
	tmux split-window -t S:0 -c "$TDIR" "$TDIR/$1" -c "read -r -t 300"
}

# wait_for_pane_cmd IDX WANT — polls up to ~2s for pane index IDX's
# #{pane_current_command} to read WANT. tmux briefly reports the forking
# server's own name before the exec completes, so a bare single read is a
# false negative, not proof of absence (hazard noted for the caller: this
# guards the fast, agent-present paths only — see the no-host test below for
# the genuinely slow, unbounded-by-this-poll path).
wait_for_pane_cmd() {
	local idx="$1" want="$2" tries=40 cmd=""
	while ((tries-- > 0)); do
		cmd=$(tmux list-panes -t S -F '#{pane_index}|#{pane_current_command}' | awk -F'|' -v i="$idx" '$1==i{print $2}')
		[[ $cmd == "$want" ]] && return 0
		sleep 0.05
	done
	echo "pane $idx never reported command '$want' (last saw '$cmd')" >&2
	return 1
}

pane_id_for_index() {
	tmux list-panes -t S -F '#{pane_index}|#{pane_id}' | awk -F'|' -v i="$1" '$1==i{print $2}'
}

@test "host discovery excludes the viewer's own pane and finds the agent sibling" {
	# pane0 (the viewer / TMUX_PANE below) ALSO runs a fake agent binary on
	# purpose: self-exclusion is by pane id (\$pane_id != \$TMUX_PANE), not by
	# command, so this pins that the id check is what protects it — if
	# self-exclusion were keyed on command instead, this pane would satisfy
	# is_agent_cmd and win.
	make_fake_agent claude
	tmux respawn-pane -k -t S:0.0 "$TDIR/claude" -c "read -r -t 300"
	spawn_pane claude
	wait_for_pane_cmd 0 claude
	wait_for_pane_cmd 1 claude

	VIEWER_PANE="$(pane_id_for_index 0)"
	HOST_PANE="$(pane_id_for_index 1)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	want_key="$SRV_PID-${HOST_PANE#%}"
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$want_key" ]
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_axis)" = "side" ]
	grep -qxF "$want_key" "$AEYE_LOG"
	grep -qxF "HOST=$HOST_PANE" "$AEYE_LOG"
}

@test "two agent panes in one window: the lowest pane index wins" {
	# idx1 = codex (created first), idx2 = claude (created second) — the tie
	# is broken on the STATED rule (lowest index), not on list-panes' own
	# order, which here would already agree; the assertion is on the index,
	# not on "whichever line came out first".
	make_fake_agent codex
	make_fake_agent claude
	spawn_pane codex
	spawn_pane claude
	wait_for_pane_cmd 1 codex
	wait_for_pane_cmd 2 claude

	VIEWER_PANE="$(pane_id_for_index 0)"
	LOW_HOST="$(pane_id_for_index 1)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	want_key="$SRV_PID-${LOW_HOST#%}"
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$want_key" ]
}

@test "non-agent sibling panes are ignored; the lone agent pane still wins" {
	make_fake_agent editor
	make_fake_agent claude
	spawn_pane editor
	spawn_pane claude
	wait_for_pane_cmd 1 editor
	wait_for_pane_cmd 2 claude

	VIEWER_PANE="$(pane_id_for_index 0)"
	HOST="$(pane_id_for_index 2)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	want_key="$SRV_PID-${HOST#%}"
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$want_key" ]
}

@test "no agent pane anywhere in the window: exits 0 after the bounded retry, nothing stamped" {
	# Two non-self panes, so the exit-time single-pane fallback does not apply
	# and the retry genuinely expires. CAROUSEL_RESTORE_TRIES is the script's
	# test seam for the 60 * 250ms bound (the bound itself is load-bearing in
	# production — see spec fact 8b — but waiting it out here buys nothing).
	make_fake_agent editor
	make_fake_agent viewer2
	spawn_pane editor
	spawn_pane viewer2
	wait_for_pane_cmd 1 editor
	wait_for_pane_cmd 2 viewer2

	VIEWER_PANE="$(pane_id_for_index 0)"

	CAROUSEL_RESTORE_TRIES=2 TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	got=$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src 2>/dev/null) || got="<unset>"
	[ "$got" = "<unset>" ]
	[ ! -s "$AEYE_LOG" ]
}

@test "unresolvable viewer binary: exits 0 and leaves @claude_img_src unset" {
	# Fast path: the -x check on the viewer binary runs BEFORE host discovery,
	# so this never reaches the retry loop. Pins the B7 half-stamp hazard —
	# the defect being guarded against is a pane marked as a viewer while
	# still running a shell, which tmux-update-icons would re-stamp on every
	# later tick and every later restore would reproduce.
	make_fake_agent claude
	spawn_pane claude
	wait_for_pane_cmd 1 claude

	VIEWER_PANE="$(pane_id_for_index 0)"

	AEYE_BIN="$TDIR/no-such-aeye-binary" TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	got=$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src 2>/dev/null) || got="<unset>"
	[ "$got" = "<unset>" ]
	[ ! -s "$AEYE_LOG" ]
}

@test "retry expires with exactly one non-self pane: falls back to it as host" {
	# The fallback exists because a restoring agent pane legitimately shows the
	# wrong command for a while — during `cat-scrollback` replay its
	# pane_current_command reads "tmux-remux", and a pane-0 carousel is briefly
	# alone in its window (spec fact 8b). Matching "not me" is immune to that,
	# so a sole sibling is the host by elimination. Simulated with a non-agent
	# sibling and a short bound: the agent match never succeeds, the retry
	# expires, and the fallback must still produce a key for that sibling.
	make_fake_agent editor
	spawn_pane editor
	wait_for_pane_cmd 1 editor

	VIEWER_PANE="$(pane_id_for_index 0)"
	HOST_PANE="$(pane_id_for_index 1)"

	CAROUSEL_RESTORE_TRIES=2 TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT"
	[ "$status" -eq 0 ]

	# Keyed to the sibling, not to the viewer's own pane.
	want_key="$SRV_PID-${HOST_PANE#%}"
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$want_key" ]
	grep -qxF "$want_key" "$AEYE_LOG"
	grep -qxF "HOST=$HOST_PANE" "$AEYE_LOG"
}

@test "the host-index hint picks that agent pane over the lowest-index one" {
	# Two agent siblings. Without a hint the tie-break is lowest index (idx 1);
	# the hint names idx 2, which is the pane the stamp recorded as the host, so
	# it must win. This is the whole point of carrying the index across the
	# restore — the alternative is an arbitrary pick between two live agents.
	make_fake_agent claude
	make_fake_agent codex
	spawn_pane claude
	spawn_pane codex
	wait_for_pane_cmd 1 claude
	wait_for_pane_cmd 2 codex

	VIEWER_PANE="$(pane_id_for_index 0)"
	LOW="$(pane_id_for_index 1)"
	HINTED="$(pane_id_for_index 2)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT" 2
	[ "$status" -eq 0 ]

	want_key="$SRV_PID-${HINTED#%}"
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$want_key" ]
	# And specifically NOT the lowest-index pane, which is what wins with no hint.
	[ "$want_key" != "$SRV_PID-${LOW#%}" ]
	grep -qxF "HOST=$HINTED" "$AEYE_LOG"
}

@test "a hint naming no agent pane falls back to the lowest-index agent" {
	# tmux-remux's filter can drop a pane, shifting every index above it, so the
	# hint can point at a pane that is not an agent (or not there at all). It is
	# a preference, never an address: discovery must still land on a real agent
	# rather than trusting a stale index.
	make_fake_agent claude
	make_fake_agent editor
	spawn_pane claude
	spawn_pane editor
	wait_for_pane_cmd 1 claude
	wait_for_pane_cmd 2 editor

	VIEWER_PANE="$(pane_id_for_index 0)"
	AGENT="$(pane_id_for_index 1)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT" 2
	[ "$status" -eq 0 ]

	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$SRV_PID-${AGENT#%}" ]
}

@test "no agent command anywhere (the real restore shape): the hint names the host" {
	# THE regression this file exists for. A tmux-remux-restored pane reports its
	# SHELL as pane_current_command, never the relaunched program — the relaunch
	# is a child sharing the shell's process group. So on the one path this
	# script exists for there are NO agent matches, and discovery has to run on
	# the index hint alone. Three panes, none an agent, hint names index 2.
	make_fake_agent editor
	make_fake_agent pager
	spawn_pane editor
	spawn_pane pager
	wait_for_pane_cmd 1 editor
	wait_for_pane_cmd 2 pager

	VIEWER_PANE="$(pane_id_for_index 0)"
	HINTED="$(pane_id_for_index 2)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT" 2
	[ "$status" -eq 0 ]

	# Before the fix this window restored NOTHING: no agent match ever, and the
	# sole-sibling fallback declines with two siblings.
	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$SRV_PID-${HINTED#%}" ]
	grep -qxF "HOST=$HINTED" "$AEYE_LOG"
}

@test "no agent command and a hint naming no pane: exits 0, nothing stamped" {
	# remux's filter can drop a pane and shift every index above it, so a hint
	# can point at nothing. With more than one sibling there is no way to tell
	# which is the host, and guessing would key the carousel to the wrong pane —
	# worse than leaving today's bare shell.
	make_fake_agent editor
	make_fake_agent pager
	spawn_pane editor
	spawn_pane pager
	wait_for_pane_cmd 1 editor
	wait_for_pane_cmd 2 pager

	VIEWER_PANE="$(pane_id_for_index 0)"

	CAROUSEL_RESTORE_TRIES=2 TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT" 9
	[ "$status" -eq 0 ]

	got=$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src 2>/dev/null) || got="<unset>"
	[ "$got" = "<unset>" ]
	[ ! -s "$AEYE_LOG" ]
}

@test "an agent command outranks the hint when both resolve" {
	# The hint is the authority only when commands are useless. In a live session
	# a pane may have moved since the stamp, so positive evidence (this pane is
	# running an agent) beats a possibly-stale index.
	make_fake_agent claude
	make_fake_agent editor
	spawn_pane claude
	spawn_pane editor
	wait_for_pane_cmd 1 claude
	wait_for_pane_cmd 2 editor

	VIEWER_PANE="$(pane_id_for_index 0)"
	AGENT="$(pane_id_for_index 1)"

	TMUX_PANE="$VIEWER_PANE" run bash "$SCRIPT" 2
	[ "$status" -eq 0 ]

	[ "$(tmux show -pv -t "$VIEWER_PANE" @claude_img_src)" = "$SRV_PID-${AGENT#%}" ]
}
