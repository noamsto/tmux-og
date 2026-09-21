#!/usr/bin/env bats
# Fan-out reflow correctness (issue #150): when the dispatcher opens worker
# windows and stamps @crew_name/@crew_color on them, the crew badge must appear
# and the grid must realign WITHOUT waiting for an unrelated structural event
# (a window close) to bust the win_count:WIDTH reflow cache.
#
# Runs the real scripts against a private, config-less tmux server so bare
# `tmux` calls inside the scripts resolve to it (via TMUX_TMPDIR), never the
# developer's own server.
#
# Run it through nix (`nix build .#checks.<system>.reflow-fanout-tests`, which
# `nix flake check` covers), not a bare `bats tests/reflow-fanout.bats`. The
# tests address the session's first window as S:0, which holds under the plain
# tmux the check pins — but tmux-og's own wrapper bakes in `-f <conf>` ahead of
# the `-f /dev/null` below, and that conf sets base-index 1, so with the wrapper
# on PATH the window is S:1 and half the file fails with "no such window: S:0".

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	# Isolated Claude state root so update-icons' pane/task/name scans find nothing.
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	# Pin TMPDIR so the reflow lock lands where the test can find it.
	export TMPDIR="$TDIR"

	# Fake reflow: records each invocation's argv so we can assert it fired.
	REFLOW_LOG="$TDIR/reflow.log"
	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		echo "\$*" >>"$REFLOW_LOG"
	EOF
	chmod +x "$FAKE_REFLOW"

	# Runnable update-icons with Nix placeholders resolved.
	UPDATE_ICONS="$TDIR/update-icons.sh"
	local licons
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e 's|@MAX_ICONS@|5|g' \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	# Runnable reflow with Nix placeholders resolved.
	REFLOW="$TDIR/reflow.sh"
	local lenrich
	lenrich="$TDIR/lib-enrich.sh"
	sed \
		-e 's/@providers@/linear github/g' \
		-e 's/@enrich_icon_linear@/L/g' -e 's/@enrich_icon_github@/G/g' \
		-e 's/@enrich_icon_pending@/P/g' -e 's/@enrich_icon_success@/S/g' \
		-e 's/@enrich_icon_failure@/F/g' -e 's/@enrich_icon_merged@/M/g' \
		-e 's/@enrich_icon_closed@/X/g' -e 's/@enrich_icon_conflict@/C/g' \
		scripts/lib-enrich.sh >"$lenrich"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_enrich@|$lenrich|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@lib_reflow@|$PWD/scripts/lib-reflow.sh|g" \
		-e 's|@MAX_ICONS@|5|g' \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-reflow-windows.sh >"$REFLOW"
	# Executable, with a sandbox-resolvable shebang: a lock-starved reflow
	# re-execs itself ($0) as a detached waiter.
	chmod +x "$REFLOW"

	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

run_update_icons() {
	: >"$REFLOW_LOG"
	# `|| true`: update-icons fires the reflow with `… & disown`; our fake reflow
	# is instant, so it can be reaped before `disown` runs and make disown (the
	# last command) exit non-zero. tmux ignores this #() callback's exit code, and
	# the reflow log is written before disown regardless — so assert on the log,
	# not the exit status.
	bash "$UPDATE_ICONS" S >/dev/null 2>&1 || true
	# update-icons backgrounds the reflow (`& disown`); wait briefly for the log.
	for _ in $(seq 1 40); do
		[[ -s $REFLOW_LOG ]] && return 0
		sleep 0.05
	done
	return 0
}

@test "crew stamp triggers a forced reflow" {
	run_update_icons # baseline tick seeds window state
	tmux set -wq -t S:0 @crew_name "atlas"
	tmux set -wq -t S:0 @crew_color "#ff0000"

	run_update_icons
	grep -q -- '--force' "$REFLOW_LOG"
}

@test "an unchanged crew name does not re-fire the reflow" {
	tmux set -wq -t S:0 @crew_name "atlas"
	run_update_icons # detects atlas -> fires (and records the seen value)

	run_update_icons # nothing changed
	[ ! -s "$REFLOW_LOG" ]
}

@test "a held reflow lock defers a concurrent reflow's write (no lost update)" {
	tmux new-window -d
	tmux new-window -d # 3 windows -> reflow computes key 3:200:0 (no client height)
	local lock="$TDIR/og-reflow.lock.S"
	mkdir "$lock" # simulate an in-flight reflow holding the lock
	tmux set -q @reflow_key "sentinel"

	bash "$REFLOW" S 200 --force >/dev/null 2>&1 &
	local rpid=$!

	sleep 0.3
	# blocked on the lock -> must not have clobbered anything yet
	[ "$(tmux show -v @reflow_key)" = "sentinel" ]

	rmdir "$lock" # holder finishes
	wait "$rpid"
	# now it acquired, recomputed against fresh state, and stamped the real key
	[ "$(tmux show -v @reflow_key)" = "3:200:0" ]
}

@test "a window count change during a held lock stamps the count it rendered, not its pre-lock snapshot (#614)" {
	tmux new-window -d
	tmux new-window -d
	local ids
	ids=$(tmux list-windows -t S -F '#{window_id}' | tail -n +2)
	local lock="$TDIR/og-reflow.lock.S"
	mkdir "$lock"
	tmux set -q @reflow_key "sentinel"

	bash "$REFLOW" S 200 --force >/dev/null 2>&1 &
	local rpid=$!

	# Close windows while it waits on the lock, after it read win_count=3.
	# On a very slow runner this can pass without racing, never fail wrongly.
	sleep 1
	local id
	for id in $ids; do tmux kill-window -t "$id"; done

	rmdir "$lock"
	wait "$rpid"
	# Stamps the count it rendered (1), not its pre-lock snapshot (3). Poll: a
	# runner slow enough to overrun the foreground budget hands off to the waiter.
	local key=""
	for _ in $(seq 1 50); do
		key=$(tmux show -v @reflow_key)
		[ "$key" = "1:200:0" ] && break
		sleep 0.1
	done
	[ "$key" = "1:200:0" ]
}

@test "a lock held past the foreground budget never writes unlocked, and a detached waiter renders once it frees (#614)" {
	# The foreground gives up after ~2s without writing (writing unlocked would
	# tear), but the render is still owed: a detached waiter picks it up.
	local lock="$TDIR/og-reflow.lock.S"
	mkdir "$lock"
	tmux set -q @reflow_key "sentinel"
	# The timeout only catches a hang; startup on a slow runner is not the budget.
	timeout 15 bash "$REFLOW" S 200 --force >/dev/null 2>&1
	local rstatus=$?
	[ "$rstatus" -eq 0 ]
	[ "$(tmux show -v @reflow_key)" = "sentinel" ]
	rmdir "$lock"
	local key=""
	for _ in $(seq 1 50); do
		key=$(tmux show -v @reflow_key)
		[ "$key" = "1:200:0" ] && break
		sleep 0.1
	done
	[ "$key" = "1:200:0" ]
}

@test "a dead holder's lock is stolen by the detached waiter (#614)" {
	# A SIGKILLed holder never releases its lock dir; the waiter outlasts the
	# stale window and steals it. Staleness is whole-second and a slow runner's
	# startup adds to the ~2s foreground budget, so keep the window well clear
	# of both or the foreground steals it itself.
	export OG_LOCK_STALE_SECONDS=10
	local lock="$TDIR/og-reflow.lock.S"
	mkdir "$lock"
	tmux set -q @reflow_key "sentinel"
	timeout 15 bash "$REFLOW" S 200 --force >/dev/null 2>&1
	local rstatus=$?
	[ "$rstatus" -eq 0 ]
	[ "$(tmux show -v @reflow_key)" = "sentinel" ]
	local key=""
	for _ in $(seq 1 250); do
		key=$(tmux show -v @reflow_key)
		[ "$key" = "1:200:0" ] && break
		sleep 0.1
	done
	[ "$key" = "1:200:0" ]
}

@test "empty or non-numeric WIDTH exits without poisoning @reflow_key" {
	# Prior good key must survive a reflow that has nothing to measure (no
	# attached client → #{client_width} empty) or an explicit junk width.
	tmux set -q @reflow_key "3:200"

	run bash "$REFLOW" S --force
	[ "$status" -eq 0 ]
	[ "$(tmux show -v @reflow_key)" = "3:200" ]

	run bash "$REFLOW" S "0" --force
	[ "$status" -eq 0 ]
	[ "$(tmux show -v @reflow_key)" = "3:200" ]

	run bash "$REFLOW" S "bogus" --force
	[ "$status" -eq 0 ]
	[ "$(tmux show -v @reflow_key)" = "3:200" ]
}

@test "zoom marker is carved from the label so the grid slot stays uniform" {
	# The inline " 󰁌" marker (LABEL_Z) is 2 cells; a zoomed window must reserve
	# them from its own label budget, or its grid slot renders 2 cells wide and
	# shoves that row's icons/PR/separator right (issue #150 follow-up).
	tmux set -wq -t S:0 @branch "aaa"
	tmux new-window -d # S:1 -> the zoomed twin
	tmux set -wq -t S:1 @branch "aaa"
	tmux new-window -d # S:2 -> long branch drives colw so the twins pad, not clip
	tmux set -wq -t S:2 @branch "a-very-long-branch-name-here"

	tmux split-window -d -t S:1
	tmux resize-pane -Z -t S:1
	[ "$(tmux display -t S:1 -p '#{window_zoomed_flag}')" = "1" ]

	# Narrow enough to force the multi-line grid (where the carve matters).
	bash "$REFLOW" S 80 --force >/dev/null 2>&1

	# shellcheck source=/dev/null
	source "$TDIR/lib-icons.sh"
	dw() { measure_display_width "$1"; }
	dw "$(tmux show -wv -t S:0 @window_label_disp)"
	local plain=$REPLY_DW
	dw "$(tmux show -wv -t S:1 @window_label_disp)"
	local zoomed=$REPLY_DW
	# Same content + same colw; the zoomed twin's remainder is exactly 2 shorter.
	[ "$plain" -eq "$((zoomed + 2))" ]
}

@test "a bridge window labels from @window_bridge_name, not the clobbered window_name" {
	# Simulate the real-config clobber: window_name is the wrong cwd-derived
	# name, but the daemon-owned @window_bridge_name holds the remote name.
	tmux set -wq -t S:0 @bridge_win 1
	tmux set-window-option -t S:0 automatic-rename off
	tmux rename-window -t S:0 tmux-og             # the wrong name
	tmux set -wq -t S:0 @window_bridge_name shell # the remote name

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(tmux show -wv -t S:0 @window_label_short)" = "shell" ]
}

# Bridge label carry (#462): the daemon stamps the remote window's own label
# state under @bridge_*. Reflow must feed those through the same width math as a
# local window's, or a mirror renders no badge and no identity.
stamp_mirror() {
	local w=$1 id=$2 rest=$3 crew=$4
	tmux set -wq -t "S:$w" @bridge_win 1
	tmux set-window-option -t "S:$w" automatic-rename off
	tmux set -wq -t "S:$w" @bridge_label_id "$id"
	tmux set -wq -t "S:$w" @bridge_label_rest_long "$rest"
	tmux set -wq -t "S:$w" @bridge_crew_name "$crew"
}

@test "a mirror window renders the bridge crew badge and identity" {
	stamp_mirror 0 "G #460" " land the fix" "atlas"
	tmux set -wq -t S:0 @bridge_crew_color "#ff0000"
	tmux set -wq -t S:0 @bridge_pr_plain " S #12"

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(tmux show -wv -t S:0 @window_crew_disp)" = "atlas " ]
	[ "$(tmux show -wv -t S:0 @window_label_id_disp)" = "G #460" ]
	[ "$(tmux show -wv -t S:0 @window_label_id)" = "G #460" ]
	[ "$(tmux show -wv -t S:0 @window_label_rest_long)" = " land the fix" ]
	# Short mode drops the remainder for an id-bearing window, as the local
	# build_window_label does — otherwise total_short == total_long.
	[ -z "$(tmux show -wv -t S:0 @window_label_rest_short)" ]
	[ "$(tmux show -wv -t S:0 @window_pr_plain)" = " S #12" ]
}

@test "a bare mirror renders the remote name and adds no badge or id column" {
	tmux set -wq -t S:0 @bridge_win 1
	tmux set-window-option -t S:0 automatic-rename off
	tmux rename-window -t S:0 tmux-og
	tmux set -wq -t S:0 @window_bridge_name shell

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(tmux show -wv -t S:0 @window_label_short)" = "shell" ]
	[ -z "$(tmux show -wv -t S:0 @window_crew_disp)" ]
	[ -z "$(tmux show -wv -t S:0 @window_label_id)" ]
}

@test "a narrow mirror grid drops the badge before the id and keeps columns exact" {
	for _ in 1 2 3; do tmux new-window -d; done
	# Uneven id widths, so the column's padding has to compensate for a real
	# difference rather than for nothing.
	stamp_mirror 0 "G #4" " a remote branch title" "atlas"
	stamp_mirror 1 "G #4601" " another title" "atlas"
	stamp_mirror 2 "G #460000" " a remote branch title" "atlas"
	stamp_mirror 3 "G #46" " yet another" "atlas"

	bash "$REFLOW" S 80 --force >/dev/null 2>&1

	local per
	per=$(tmux show -v @window_per)
	[ "$per" -lt 4 ] # multiline, or there are no columns to be exact about

	# The widest id overruns its column with the badge attached: the badge is
	# decoration and goes first, the ticket id is identity and survives whole.
	[ -z "$(tmux show -wv -t "S:$per" @window_crew_disp)" ]
	[ "$(tmux show -wv -t "S:$per" @window_label_id_disp)" = "G #460000" ]
	# ... while its column neighbour still has room for its own badge, which is
	# what keeps the equality below from being a tautology.
	[ -n "$(tmux show -wv -t S:0 @window_crew_disp)" ]

	# shellcheck source=/dev/null
	source "$TDIR/lib-icons.sh"
	# S:0 and S:$per sit in the same grid column. Everything the slot renders off
	# the window's own label — badge, id, padded remainder — must add up to the
	# same width in both, or every slot to the right on one row shifts.
	slot_dw() {
		measure_display_width "$(tmux show -wv -t "S:$1" @window_crew_disp)$(tmux show -wv -t "S:$1" @window_label_id_disp)$(tmux show -wv -t "S:$1" @window_label_disp)"
	}
	slot_dw 0
	local first=$REPLY_DW
	slot_dw "$per"
	local below=$REPLY_DW
	[ "$first" -eq "$below" ]
}

@test "an unset @issue_branch does not suppress the stamp" {
	tmux new-window -d
	tmux set -wq -t S:1 @branch feat/123-a-thing
	tmux set -wq -t S:1 @issue_id "#123"
	tmux set -wq -t S:1 @issue_title "A thing"
	tmux set -wq -t S:1 @pr_number 5
	tmux set -wq -t S:1 @pr_state open
	tmux set -wq -t S:1 @pr_check_state pending

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	# No recorded branch is not evidence of a move, so the stamp — and the PR
	# segment that rode it — still renders.
	[ "$(tmux show -wv -t S:1 @window_label_id)" = "G #123" ]
	[ "$(tmux show -wv -t S:1 @window_pr_num)" = "#5" ]
}

@test "a stamp naming a different branch is still suppressed" {
	tmux new-window -d
	tmux set -wq -t S:1 @branch feat/123-a-thing
	tmux set -wq -t S:1 @issue_id "#123"
	tmux set -wq -t S:1 @issue_title "A thing"
	tmux set -wq -t S:1 @pr_number 5
	tmux set -wq -t S:1 @pr_state open
	tmux set -wq -t S:1 @pr_check_state pending
	tmux set -wq -t S:1 @issue_branch feat/999-left-behind

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	# The stamp belonged to the branch the pane left: the label falls back to the
	# window's own branch (bare number, no '#') and the PR goes with it.
	[ "$(tmux show -wv -t S:1 @window_label_id)" = "G 123" ]
	[ -z "$(tmux show -wv -t S:1 @window_pr_num)" ]
}

@test "a PR badge appearing clips the widest label instead of adding a row" {
	# #686: the single-line fit charges every window its own PR segment, so a
	# badge arriving on one window used to tip a full row into the multi-line
	# grid — a whole status line, and every label capped at its column on top.
	tmux set -wq -t S:0 @branch "feat/a-long-branch-name-with-room-to-clip"
	tmux new-window -d
	tmux set -wq -t S:1 @branch "feat/another-branch"
	tmux new-window -d
	tmux set -wq -t S:2 @branch "feat/third-branch"

	row_is_whole() {
		[ "$(tmux show-options -t S -qv status)" = 2 ] || return 1
		local w
		for w in 0 1 2; do
			case "$(tmux show -wv -t "S:$w" @window_label_disp)" in
			*…) return 1 ;;
			esac
		done
	}

	# Narrowest width that renders one row with every label whole — i.e. the row
	# is exactly full, which is where a badge's 6 cells decide the layout.
	local lo=40 hi=200 mid full=0
	while ((lo <= hi)); do
		mid=$(((lo + hi) / 2))
		bash "$REFLOW" S "$mid" --force >/dev/null 2>&1
		if row_is_whole; then
			full=$mid
			hi=$((mid - 1))
		else
			lo=$((mid + 1))
		fi
	done
	[ "$full" -gt 0 ]

	tmux set -wq -t S:0 @pr_number 12
	tmux set -wq -t S:0 @pr_state open
	tmux set -wq -t S:0 @pr_check_state success
	bash "$REFLOW" S "$full" --force >/dev/null 2>&1

	[ "$(tmux show -wv -t S:0 @window_pr_plain)" = " S #12" ]
	[ "$(tmux show-options -t S -qv status)" = 2 ]
	# The widest label pays for it, and only its display copy — the pickers and
	# the bridge read @window_label_rest_long for the full identity.
	[[ "$(tmux show -wv -t S:0 @window_label_disp)" == *… ]]
	[[ "$(tmux show -wv -t S:0 @window_label_rest_long)" != *… ]]
}

@test "a PR badge pads only the grid column it sits in" {
	# #688: pr_colw used to be the widest badge in the session, charged to every
	# column through OVERHEAD and padded onto every window. One badge in column
	# 0 must cost column 1 nothing.
	tmux set -wq -t S:0 @branch "feat/alpha-branch"
	local i
	for i in 1 2 3; do
		tmux new-window -d
		tmux set -wq -t "S:$i" @branch "feat/branch-number-$i"
	done
	tmux set -wq -t S:0 @pr_number 12
	tmux set -wq -t S:0 @pr_state open
	tmux set -wq -t S:0 @pr_check_state success

	bash "$REFLOW" S 100 --force >/dev/null 2>&1

	[ "$(tmux show -v -t S @window_per)" = 2 ]
	[ "$(tmux show -wv -t S:0 @window_pr_plain)" = " S #12" ]
	# S:2 shares the badge's column and pads to it; S:1 and S:3 are the column
	# that holds no PR at all.
	[ "$(tmux show -wv -t S:2 @window_pr_pad)" = "      " ]
	[ -z "$(tmux show -wv -t S:1 @window_pr_pad)" ]
	[ -z "$(tmux show -wv -t S:3 @window_pr_pad)" ]
}

@test "a grid with cells to spare renders past the label cap" {
	# #688: a want is capped at MAX_REST_WIDTH so one long name cannot stretch
	# the grid, but the cap used to survive a row that had cells left over —
	# titles ellipsised at 40 beside idle columns.
	local long="feat/a-very-long-branch-name-that-runs-well-past-the-forty-cell-cap"
	tmux set -wq -t S:0 @branch "$long-0"
	local i
	for i in 1 2 3 4 5; do
		tmux new-window -d
		tmux set -wq -t "S:$i" @branch "$long-$i"
	done
	tmux set -wq -t S:2 @branch "feat/short"

	bash "$REFLOW" S 160 --force >/dev/null 2>&1

	[ "$(tmux show -v -t S @window_per)" = 2 ]
	# shellcheck source=/dev/null
	source "$TDIR/lib-icons.sh"
	measure_display_width "$(tmux show -wv -t S:0 @window_label_disp)"
	local wide=$REPLY_DW
	[ "$wide" -gt 40 ]
	# S:2 shares that column with a much shorter branch: the widened column
	# still pads down its length, so the row stays aligned.
	measure_display_width "$(tmux show -wv -t S:2 @window_label_disp)"
	[ "$REPLY_DW" -eq "$wide" ]
}
