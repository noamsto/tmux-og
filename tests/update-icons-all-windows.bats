#!/usr/bin/env bats
bats_require_minimum_version 1.5.0
# Regression for #580: tmux-update-icons is invoked from status-format[0], which
# tmux only evaluates for a client drawing a status line. A session with no
# attached client never got its own pass, so @window_icon_padded stayed unset
# (empty) rather than the MAX_ICONS*3+2 empty pad. One invocation on A must
# stamp every list-windows -a row.

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
	# reaps state files. Pin it like the other update-icons suites (#373).
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))

	# #671's backstop matches pane_current_command against $AGENT_COMMANDS
	# (env-var test seam, same as AGENT_DETECT_BIN); the sed pipeline below
	# doesn't substitute @AGENT_COMMANDS@, so without this it stays the
	# literal, never-matching placeholder and @window_has_agent never flips.
	export AGENT_COMMANDS="claude"

	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	# MAX_ICONS=2 → empty pad is 8 cells, the measured discriminator in #580.
	MAX_ICONS=2
	PAD_LEN=$((MAX_ICONS * 3 + 2))
	export PAD_LEN

	UPDATE_ICONS="$TDIR/update-icons.sh"
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@/["claude"]="C"/' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e "s|@MAX_ICONS@|$MAX_ICONS|g" \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	REPO="$TDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" branch -q -M main

	# pane_current_command is the executable basename. sleep/cat are coreutils
	# multicall (argv0 dispatch), and a shebang script reports as `sh`, so copy
	# bash to a file named claude and use it as default-shell for one window.
	mkdir -p "$TDIR/bin"
	cp -L "$(command -v bash)" "$TDIR/bin/claude"
	chmod +x "$TDIR/bin/claude"

	tmux -f /dev/null new-session -d -s A -c "$REPO" -x 200 -y 50
	tmux new-session -d -s B -c "$REPO" -x 200 -y 50
	tmux set -g default-shell "$TDIR/bin/claude"
	tmux set -g default-command ''
	tmux new-window -t B -c "$REPO"
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

padded_of() {
	tmux show -wv -t "$1" @window_icon_padded 2>/dev/null || true
}

display_of() {
	tmux show -wv -t "$1" @window_icon_display 2>/dev/null || true
}

opt_of() {
	tmux show -wv -t "$1" "$2" 2>/dev/null || true
}

@test "one pass on A stamps @window_icon_padded on every window including unattached B" {
	# Before the pass, B's windows must still be unstamped (the bug's red).
	[ -z "$(padded_of B:0)" ]
	[ -z "$(padded_of B:1)" ]

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}

	local n=0 sess idx padded disp
	while IFS='|' read -r sess idx; do
		[ -n "$sess" ] || continue
		n=$((n + 1))
		padded=$(padded_of "$sess:$idx")
		[ ${#padded} -eq "$PAD_LEN" ] ||
			{ echo "$sess:$idx padded len ${#padded} want $PAD_LEN value [$padded]" && false; }
	done < <(tmux list-windows -a -F '#{session_name}|#{window_index}')
	[ "$n" -eq 3 ] || { echo "expected 3 windows, got $n" && false; }

	# Shell windows: empty pad (spaces), empty display.
	disp=$(display_of A:0)
	[ -z "$disp" ]
	padded=$(padded_of A:0)
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ]

	padded=$(padded_of B:0)
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ]

	# B:1's executable is named claude → ICON_MAP hit, display is the mapped glyph.
	disp=$(display_of B:1)
	[ "$disp" = "C" ] || { echo "B:1 display [$disp] want [C]; cmd=$(tmux list-panes -t B:1 -F '#{pane_current_command}')" && false; }
}

@test "one pass on A stamps @window_icon_padded on a session named with a pipe" {
	# tmux forbids '.' and ':' in session names; '|' is legal and would shift a
	# middle #{session_name} field in list-panes -F, so wkey is wrong and the
	# pad never lands — the same #580 discriminator.
	# Restore a normal default-shell so this session is a shell window (empty
	# pad), matching A:0 — setup() pointed default-shell at a `claude` binary
	# only to give B:1 an ICON_MAP hit.
	tmux set -g default-shell "$(command -v bash)"
	if ! tmux new-session -d -s 'a|b' -c "$REPO" -x 200 -y 50 2>/dev/null; then
		# skip citing version if this tmux rejects '|'-named sessions
		skip "tmux $(tmux -V) rejects session names containing |"
	fi

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}

	local line sid sname padded
	sid=""
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		sname="${line#*|}"
		if [ "$sname" = "a|b" ]; then
			sid="${line%%|*}"
			break
		fi
	done < <(tmux list-sessions -F '#{session_id}|#{session_name}')
	[ -n "$sid" ] || { echo "could not resolve session id for a|b" && false; }

	padded=$(padded_of "$sid:0")
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ] ||
		{ echo "$sid:0 padded [$padded] want $PAD_LEN spaces" && false; }
}

# #671 backstop: a shell with no OSC 133 support never fires
# tmux-shell-prompt's event trigger, so tmux-update-icons' own per-window
# has_agent transition compare is the only path that can ever clear a window's
# naming/crew display once its last agent exits.
@test "backstop clears naming/crew display once B:1's foreground command is no longer an agent" {
	local pane bare
	pane="$(tmux list-panes -t B:1 -F '#{pane_id}')"
	bare="${pane#%}"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks,issues}

	tmux set -w -t B:1 @window_ai_name "Old AI Name"
	tmux set -w -t B:1 @window_task "old task"
	tmux set -w -t B:1 @crew_name coral
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'old task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ "$(opt_of B:1 @window_has_agent)" = 1 ]

	# Agent exits to a shell with no OSC 133 support: swap the pane's
	# foreground command from the fixture `claude` binary to a plain shell —
	# no hook fires on this, so only a later backstop pass can notice.
	tmux respawn-pane -k -t B:1 -- "$(command -v bash)"
	local tries=20
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t B:1 '#{pane_current_command}')" != claude ] && break
		sleep 0.1
	done

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ -z "$(opt_of B:1 @window_has_agent)" ]
	[ -z "$(opt_of B:1 @window_ai_name)" ]
	[ -z "$(opt_of B:1 @window_task)" ]
	[ ! -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ]
	# Hard constraint: @crew_name is dispatcher-owned and never touched.
	[ "$(opt_of B:1 @crew_name)" = coral ]
}
