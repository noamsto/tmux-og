#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
bats_require_minimum_version 1.5.0 # run !
# Offline M2.1 daemon integration: mirror a local "remote" tmux window into a
# local "host" tmux window via the daemon's --test-local seam (two separate
# tmux -L servers, no ssh). DAEMON / RENDERER are prebuilt absolute store
# paths (flake.nix); fall back to `go build` for local runs.
#
# TMUX_TMPDIR is a short, fixed /tmp dir rather than $BATS_TEST_TMPDIR: tmux
# -L resolves to "$TMUX_TMPDIR/tmux-<uid>/<name>", and a long bats tmpdir
# path pushes that past the unix socket 108-char limit ("File name too
# long"). DST_CONF sets base-index 1 and renumber-windows on (the real
# tmux-og host convention, and what makes an index-keyed mirror go stale —
# #411) plus remain-on-exit on:
# once the daemon exits (timeout/kill), every renderer's socket connection
# drops and its pane's command exits, and without remain-on-exit the pane —
# then the window, then the last-session server — would tear itself down
# before the assertions below get to read pane dims.

setup() {
	export TMUX_TMPDIR="/tmp/og-m2-bats-$$"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	# DST sets global pane-base-index 1, matching the real host's render
	# config (this bit the M2.1 smoke test): spawnRenderer/kill-pane target
	# the local pane by 0-based loop index (daemon.go), which now relies on
	# the daemon stamping a window-level pane-base-index 0 override on every
	# mirror window to stay correct despite the global 1. pane-border-status
	# alone still eats a row per pane regardless of pane-base-index, so DST
	# needs it to match SRC's dims.
	DST_CONF="$BATS_TEST_TMPDIR/dst.conf"
	printf 'set -g base-index 1\nset -g pane-base-index 1\nset -g status on\nset -g pane-border-status top\nset -g remain-on-exit on\nset -g renumber-windows on\n' >"$DST_CONF"
	SRC_CONF="$BATS_TEST_TMPDIR/src.conf"
	printf 'set -g base-index 1\nset -g pane-base-index 1\nset -g status on\nset -g pane-border-status top\nset -g window-size latest\nset -g aggressive-resize on\n' >"$SRC_CONF"
	SRC="tmux -L m2src -f $SRC_CONF" # stands in for the "remote", full render config
	DST="tmux -L m2dst -f $DST_CONF" # the local mirror target

	if [[ -z ${DAEMON:-} ]]; then
		DAEMON="$BATS_TEST_TMPDIR/daemon"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$DAEMON" ./remotebridge/cmd/daemon)
	fi
	if [[ -z ${RENDERER:-} ]]; then
		RENDERER="$BATS_TEST_TMPDIR/renderer"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$RENDERER" ./remotebridge/cmd/renderer)
	fi
	if [[ -z ${CTL:-} ]]; then
		CTL="$BATS_TEST_TMPDIR/ctl"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$CTL" ./remotebridge/cmd/ctl)
	fi
	# pane_current_command truncates a long comm to macOS's MAXCOMLEN (15
	# usable chars) but not Linux's (which reads the full cmdline) — the nix
	# build's RENDERER is the long store binary name
	# "og-remote-bridge-renderer", so grepping the literal "renderer"
	# substring never matches once macOS cuts the pane's reported command
	# short of it. A prefix this short survives that truncation everywhere.
	RENDERER_PROBE="$(basename "$RENDERER" | cut -c1-15)"

	$SRC kill-server 2>/dev/null || true
	$DST kill-server 2>/dev/null || true
}

teardown() {
	$SRC kill-server 2>/dev/null || true
	$DST kill-server 2>/dev/null || true
	tmux -L m2obs kill-server 2>/dev/null || true # pty host for the attached-client test
	rm -rf "$TMUX_TMPDIR"
}

# sorted_dims prints TARGET_ARGS's pane dims, one "WxH" per line, sorted —
# used to compare SRC's and DST's pane sets independent of pane order.
sorted_dims() {
	$1 list-panes -t "$2" -F '#{pane_width}x#{pane_height}' | sort
}

# sorted_tiled_dims is sorted_dims restricted to the non-floating panes: a
# float's own dims don't move when the tiled tree reshapes around it, so
# comparing the full pane set behind an open float would compare noise
# alongside the thing that's actually under test (#535).
sorted_tiled_dims() {
	$1 list-panes -t "$2" -F '#{?pane_floating_flag,,#{pane_width}x#{pane_height}}' | grep -v '^$' | sort
}

@test "daemon mirrors a 2-pane remote window with matching pane dims" {
	# remote: a 210x52 window, uneven horizontal split.
	$SRC new-session -d -s rem -x 210 -y 52
	$SRC split-window -h -t rem
	$SRC resize-pane -t rem.1 -x 60

	# local: pre-created at the same size, one pane — the daemon's
	# convergence step (refresh-client -C) is then a no-op, so the remote's
	# 60/149 split survives untouched.
	$DST new-session -d -s host-sess -x 210 -y 52

	"$DAEMON" --test-local \
		--src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/d1.sock" \
		>"$BATS_TEST_TMPDIR/d1.log" 2>&1 &
	daemon_pid=$!

	# Gate: wait until the pane is a renderer so the layout pipeline has settled.
	for _ in $(seq 1 40); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# Capture state before killing (SIGTERM triggers teardown → DST session gone).
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"
	dst_panes="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"
	[ "$($DST show-options -v -t host-sess @bridge_session)" = rem ]

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
	[ "$dst_panes" -eq 2 ]
}

# M1 regression anchor: a single-pane remote window mirrors to a single local
# pane at matching dims, with no split applied.
@test "daemon mirrors a 1-pane remote window with no split (M1 anchor)" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local \
		--src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/d2.sock" \
		>"$BATS_TEST_TMPDIR/d2.log" 2>&1 &
	daemon_pid=$!

	# Gate: wait until the pane is a renderer before capturing state.
	for _ in $(seq 1 40); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# Capture state before killing (SIGTERM triggers teardown → DST session gone).
	dst_panes="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$dst_panes" -eq 1 ]
	[ "$src_dims" = "$dst_dims" ]
}

# Should-have: exercises the reconcile path (daemon.go reconcileLayout) —
# a remote split mid-session must land a matching pane locally.
@test "daemon reconciles a mid-session remote split" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local \
		--src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/d3.sock" >"$BATS_TEST_TMPDIR/d3.log" 2>&1 &
	daemon_pid=$!

	# Wait for the daemon to have wired the renderer into pane 0 (its
	# respawn-pane replaces the pane's shell) before splitting the remote —
	# a split fired before the daemon reaches its main read loop is a
	# %layout-change that arrives mid-setup and is silently skipped (readReply
	# only consumes reply blocks, discarding async notifications), so this
	# gate (not just "pane count == 1", which is trivially true from the
	# window's initial shell pane) is what makes the timing deterministic.
	for _ in $(seq 1 40); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# An even split (tmux's default) can't distinguish a correct reconcile
	# (re-applying select-layout with the remote's L.Raw) from a broken one
	# that only fixes up the pane count — both land at the same geometry.
	# Resize uneven, mirroring case 1, so the assertion is load-bearing.
	$SRC split-window -h -t rem
	$SRC resize-pane -t rem.1 -x 30

	# Wait for the reconciled 2-pane mirror at matching (uneven) dims.
	for _ in $(seq 1 40); do
		n="$($DST list-panes -t host-sess:1 -F '#{pane_id}' 2>/dev/null | wc -l)"
		if [ "$n" -eq 2 ] && [ "$(sorted_dims "$DST" host-sess:1)" = "$(sorted_dims "$SRC" rem)" ]; then
			break
		fi
		sleep 0.1
	done

	# Capture state before killing (SIGTERM triggers teardown → DST session gone).
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
}

@test "daemon mirrors a 3-window remote session into 3 local windows" {
	$SRC new-session -d -s rem -x 100 -y 30
	$SRC new-window -t rem
	$SRC new-window -t rem
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local \
		--src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dm.sock" \
		>"$BATS_TEST_TMPDIR/dm.log" 2>&1 &
	daemon_pid=$!

	# Gate on all 3 windows carrying a RENDERER pane, not merely existing: the
	# windows are created early in the mirror loop, so a count-only gate opens
	# mid-setup. Names settle last — SRC relabels each window (tmux -> bash) as
	# its shell execs, and a %window-renamed emitted while setup's plain reply
	# reader is running is discarded (B3), so it is the daemon's post-setup
	# reconcile that recovers them.
	for _ in $(seq 1 100); do
		rendered="$($DST list-panes -s -t host-sess -F '#{pane_current_command}' 2>/dev/null | grep -c "$RENDERER_PROBE" || true)"
		[ "$rendered" -eq 3 ] && break
		sleep 0.1
	done
	for _ in $(seq 1 60); do
		src_names="$($SRC list-windows -t rem -F '#{window_name}' | sort | tr '\n' ',')"
		dst_names="$($DST list-windows -t host-sess -F '#{@window_bridge_name}' | sort | tr '\n' ',')"
		[ "$dst_names" = "$src_names" ] && break
		sleep 0.15
	done

	# Capture state before killing (SIGTERM triggers teardown → DST session gone).
	src_wins="$($SRC list-windows -t rem -F '#{window_id}' | wc -l)"
	dst_wins="$($DST list-windows -t host-sess -F '#{window_id}' | wc -l)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_wins" -eq 3 ]
	[ "$dst_wins" -eq 3 ]

	# Each mirror window carries its remote name in @window_bridge_name.
	[ "$dst_names" = "$src_names" ]
}

@test "daemon reflects remote new-window / rename-window / kill-window" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dr.sock" \
		>"$BATS_TEST_TMPDIR/dr.log" 2>&1 &
	daemon_pid=$!

	# Gate: wait until the first window's pane is a renderer (daemon in its loop).
	for _ in $(seq 1 40); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# Add a remote window -> a new local window appears.
	$SRC new-window -t rem
	for _ in $(seq 1 40); do
		n="$($DST list-windows -t host-sess -F '#{window_id}' 2>/dev/null | wc -l)"
		[ "$n" -eq 2 ] && break
		sleep 0.1
	done
	[ "$n" -eq 2 ]

	# Gate: wait until the new window's own pipeline has also settled (its
	# pane is a renderer) before renaming — a rename fired while the
	# window-add's own reply round-trip is still in flight races the
	# non-routing-aware reader used during that pipeline and gets dropped.
	for _ in $(seq 1 40); do
		cmd2="$($DST list-panes -t host-sess:2 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd2 == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# Rename it remotely -> local @window_bridge_name follows (window_name is
	# derived by reflow, which this vanilla tmux -L server does not run).
	newwin="$($SRC list-windows -t rem -F '#{window_id}' | tail -1)"
	$SRC rename-window -t "$newwin" bridged-name
	for _ in $(seq 1 40); do
		names="$($DST list-windows -t host-sess -F '#{@window_bridge_name}' 2>/dev/null)"
		[[ $names == *bridged-name* ]] && break
		sleep 0.1
	done
	[[ $names == *bridged-name* ]]

	# Make window 1 active before killing window 2, so the kill targets a
	# NON-active window: it emits a clean %window-close with no concurrent
	# %session-window-changed / %layout-change reconcile. Killing the ACTIVE
	# window can interleave %window-close with a %layout-change round-trip whose
	# routing-aware reader swallows the close (a known async-notification
	# limitation, tracked as an M2.3 follow-up) — that races under CI load.
	$SRC select-window -t rem:1

	# Kill the added remote window -> its local window goes away (session survives).
	$SRC kill-window -t "$newwin"
	for _ in $(seq 1 40); do
		n="$($DST list-windows -t host-sess -F '#{window_id}' 2>/dev/null | wc -l)"
		[ "$n" -eq 1 ] && break
		sleep 0.1
	done
	[ "$n" -eq 1 ]

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
}

@test "a mirror window closing re-indexes the rest without stranding them" {
	$SRC new-session -d -s rem -x 100 -y 30
	$SRC new-window -t rem
	$SRC new-window -t rem
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dr.sock" \
		>"$BATS_TEST_TMPDIR/dr.log" 2>&1 &
	daemon_pid=$!

	# Gate on the same observable and budget bridge_up uses — @bridge_pane
	# stamped on every mirror pane. A hand-rolled shorter wait is how this test
	# first failed on the macOS runner: three windows do not come up inside the
	# few seconds a one-window mirror does.
	stamped=0
	deadline=$((SECONDS + BRIDGE_UP_BUDGET_SECS))
	while [ "$SECONDS" -lt "$deadline" ]; do
		stamped="$($DST list-panes -s -t host-sess -F '#{@bridge_pane}' 2>/dev/null | grep -c '^%' || true)"
		[ "$stamped" -eq 3 ] && break
		sleep 0.15
	done
	[ "$stamped" -eq 3 ]

	middle="$($SRC list-windows -t rem -F '#{window_id}' | sed -n 2p)"
	last="$($SRC list-windows -t rem -F '#{window_id}' | sed -n 3p)"
	# Kill a NON-active window, for the clean-%window-close reason the
	# new-window/rename/kill-window test above spells out.
	$SRC select-window -t rem:1
	$SRC kill-window -t "$middle"
	for _ in $(seq 1 60); do
		n="$($DST list-windows -t host-sess -F '#{window_id}' 2>/dev/null | wc -l)"
		[ "$n" -eq 2 ] && break
		sleep 0.15
	done
	[ "$n" -eq 2 ]

	# The survivor moved from local index 3 to 2 under renumber-windows. An
	# index-keyed registry would still be addressing 3, so this rename would
	# land nowhere.
	$SRC rename-window -t "$last" survivor
	for _ in $(seq 1 60); do
		name="$($DST display-message -p -t host-sess:2 '#{@window_bridge_name}' 2>/dev/null)"
		[ "$name" = survivor ] && break
		sleep 0.15
	done
	[ "$name" = survivor ]

	# And the next window appends at 3, not past the hole the close left.
	$SRC new-window -t rem
	for _ in $(seq 1 60); do
		idx="$($DST list-windows -t host-sess -F '#{window_index}' 2>/dev/null | tr '\n' ' ')"
		[ "$idx" = "1 2 3 " ] && break
		sleep 0.15
	done
	[ "$idx" = "1 2 3 " ]

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
}

@test "daemon re-converges the remote after a local resize" {
	# Bring up a 1-window mirror at 100x30 (SRC == DST), then RESIZE the local
	# window to 120x40. The resize watcher polls the local size and must push
	# the new dims onto the remote (refresh-client -C), so SRC's window converges
	# to 120x40 without any control-stream event driving it.
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dz.sock" \
		>"$BATS_TEST_TMPDIR/dz.log" 2>&1 &
	daemon_pid=$!

	# Gate: wait until the pane is a renderer (daemon reached its main loop and
	# the watcher goroutine is running) before resizing.
	for _ in $(seq 1 40); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# resize-window sticks on the detached DST session (no attached client to
	# override it under window-size latest), so the local mirror is now 120x40.
	$DST resize-window -t host-sess:1 -x 120 -y 40

	# Poll until the watcher (1s interval) pushes the new size to the remote.
	# RESIZE_CONVERGE_BUDGET_SECS — not a fixed 4s seq — matches the poll
	# interval plus control-stream reconcile under contended CI.
	deadline=$((SECONDS + RESIZE_CONVERGE_BUDGET_SECS))
	dims=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		dims="$($SRC display-message -p -t rem -F '#{window_width}x#{window_height}' 2>/dev/null)"
		[ "$dims" = "120x40" ] && break
		sleep 0.1
	done
	if [ "$dims" != "120x40" ]; then
		echo "resize converge timeout after ${RESIZE_CONVERGE_BUDGET_SECS}s: wanted 120x40, got ${dims:-empty}" >&3
		# The daemon log is the only witness to why the watcher never pushed;
		# without it a platform-specific failure here is undiagnosable from CI.
		echo "--- local: $($DST display-message -p -t host-sess:1 -F '#{window_width}x#{window_height}' 2>&1)" >&3
		echo "--- daemon alive: $(kill -0 "$daemon_pid" 2>/dev/null && echo yes || echo no)" >&3
		echo "--- dz.log ---" >&3
		sed -n '1,60p' "$BATS_TEST_TMPDIR/dz.log" >&3 2>&1 || true
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		return 1
	fi

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$dims" = "120x40" ]
}

# Regression for #231: a remote geometry change emits %layout-change without
# new pane output. The geometry-only reconcile must re-seed the renderer so
# its screen stays identical to the remote after the local window is re-fit.
@test "daemon repaints mirrored content after a remote geometry change" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dg.sock" \
		>"$BATS_TEST_TMPDIR/dg.log" 2>&1 &
	daemon_pid=$!

	# Gate on an observable paint, not RENDERER_PROBE: this test's whole point is
	# proving the daemon repaints, so an actual paint is the more direct signal.
	# Retry the startup marker so an output emitted while the daemon is still
	# wiring its first pane is not lost to setup's reply reader.
	painted=no
	for _ in $(seq 1 20); do
		$SRC send-keys -t rem "printf 'RESEED_GEOMETRY_9F3Q\\n'" Enter
		for _ in $(seq 1 10); do
			out="$($DST capture-pane -p -t host-sess:1 2>/dev/null)"
			[[ $out == *RESEED_GEOMETRY_9F3Q* ]] && {
				painted=yes
				break 2
			}
			sleep 0.1
		done
	done
	[ "$painted" = yes ]

	# A pty-hosted second remote client starts at the current size, then shrinks.
	# The per-window cap permits this smaller client to resize the remote without
	# producing pane output, which must re-fit and re-seed the local mirror.
	OBS="tmux -L m2obs"
	$OBS new-session -d -s obs -x 100 -y 30 "$SRC attach -t rem"
	for _ in $(seq 1 40); do
		clients="$($SRC list-clients -t rem 2>/dev/null | wc -l | tr -d ' ')"
		[ "$clients" -ge 2 ] && break
		sleep 0.1
	done
	[ "$clients" -ge 2 ]
	initial_dims="$(sorted_dims "$SRC" rem)"
	$OBS resize-window -t obs -x 80 -y 24

	# Let the re-seed frame drain without adding any remote output.
	for _ in $(seq 1 50); do
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		[ "$src_dims" != "$initial_dims" ] && [ "$dst_dims" = "$src_dims" ] && break
		sleep 0.1
	done
	src_screen="$($SRC capture-pane -p -t rem)"
	dst_screen="$($DST capture-pane -p -t host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_dims" != "$initial_dims" ]
	[ "$dst_dims" = "$src_dims" ]
	[ "$dst_screen" = "$src_screen" ]
}

@test "daemon converges when DST size != SRC size (ConvergeCmd resizes remote)" {
	# remote starts 120x40; local mirror created at 100x30 — the daemon's
	# refresh-client -C must push 100x30 onto the remote so pane dims converge.
	$SRC new-session -d -s rem -x 120 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 100 -y 30

	# Run in the background and poll for convergence — a foreground `run timeout`
	# would fire SIGTERM on timeout, which now triggers teardown (kill-session),
	# taking the DST server down before the assertions below can read it.
	"$DAEMON" --test-local \
		--src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dc.sock" \
		>"$BATS_TEST_TMPDIR/dc.log" 2>&1 &
	daemon_pid=$!

	# Gate on the DST side settling — every mirror pane running a renderer —
	# the way the mirror-dims test above does. SRC's width alone is not a proxy
	# for "the daemon is done": it reaches 100 the moment the daemon sizes its
	# own control client, which is before any mirror pane exists (#449), so
	# dst_dims below would be read against a window not yet shaped.
	want_panes="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
	for _ in $(seq 1 60); do
		got_panes="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null | grep -c "$RENDERER_PROBE")" || got_panes=0
		[ "$got_panes" -eq "$want_panes" ] && break
		sleep 0.1
	done

	# Capture dims before killing (teardown kills DST session on SIGTERM).
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
	# And the remote actually shrank to the local width (convergence, not no-op).
	[ "$($SRC display-message -p -t rem -F '#{window_width}' 2>/dev/null)" -eq 100 ]
}

# Regression for #201: a human client attached to the same remote session used
# to win the size negotiation outright — under window-size latest,
# clients_calculate_size skips every client but w->latest, so the bridge's
# whole-client "refresh-client -C WxH" was ignored and the mirror painted a
# screen of the human's size into a pane of ours (garbled, overlapping text).
# The per-window form is a clamp applied after that calculation, but only for
# a window that participates in sizing at all — under aggressive-resize,
# clients_calculate_size_skip_client drops any client whose session doesn't
# currently have that window selected, discarding the clamp outright for
# every other mirrored window (#478), which is why the bridge must also opt
# each mirrored window out of aggressive-resize.
@test "daemon clamps the remote even with a bigger human client attached" {
	# OBS is a third tmux server used purely as a pty host: its pane runs a
	# real `attach` to SRC, which is the only way to give SRC an attached
	# client (of a known size) inside bats.
	OBS="tmux -L m2obs"
	$OBS kill-server 2>/dev/null || true

	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	# The "human": 160 wide, and the last client to touch the window, so
	# w->latest is theirs and not the bridge's.
	$OBS new-session -d -s obs -x 160 -y 50 "$SRC attach -t rem"
	for _ in $(seq 1 40); do
		n="$($SRC list-clients -t rem 2>/dev/null | wc -l | tr -d ' ')"
		[ "$n" -ge 1 ] && break
		sleep 0.1
	done
	[ "$n" -ge 1 ] # the pty client really attached; otherwise this proves nothing
	human_w="$($SRC display-message -p -t rem -F '#{window_width}' 2>/dev/null)"
	[ "$human_w" -eq 160 ] # and it owns the window size before the bridge starts

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dh.sock" \
		>"$BATS_TEST_TMPDIR/dh.log" 2>&1 &
	daemon_pid=$!

	for _ in $(seq 1 60); do
		w="$($SRC display-message -p -t rem -F '#{window_width}' 2>/dev/null || echo 0)"
		[ "$w" -eq 100 ] && break
		sleep 0.1
	done

	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	$OBS kill-server 2>/dev/null || true

	# Clamped to the mirror's width despite the wider client owning latest...
	[ "$w" -eq 100 ]
	# ...and the local mirror was fitted to the remote, so the pane the
	# renderer paints into is exactly the screen it receives.
	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
}

# Regression for #185: a SIGTERM'd daemon must run teardown, not leave its unix
# socket + pidfile behind (which blocks the next launch from binding) and not
# orphan the local mirror session. teardown otherwise runs only on %exit/EOF.
@test "daemon removes socket + pidfile and kills the local session on SIGTERM" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	sock="$BATS_TEST_TMPDIR/dt.sock"
	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$sock" >"$BATS_TEST_TMPDIR/dt.log" 2>&1 &
	daemon_pid=$!

	# Wait until it has bound the socket AND written its pidfile.
	for _ in $(seq 1 50); do
		[ -S "$sock" ] && [ -f "$sock.pid" ] && break
		sleep 0.1
	done
	[ -S "$sock" ]
	[ -f "$sock.pid" ]

	kill -TERM "$daemon_pid"
	wait "$daemon_pid" 2>/dev/null || true

	[ ! -e "$sock" ]
	[ ! -e "$sock.pid" ]
	run $DST has-session -t host-sess
	[ "$status" -ne 0 ]
}

# Regression for #183: with the default pause-after, live %output produced
# AFTER the renderer is wired must still paint into the mirror. The dims-only
# cases above never read pane CONTENT, so they can't catch a frozen stream:
# tmux pauses the pane ~1s after attach and the %pause/%continue re-seed must
# actually resume output. Assert real content lands in the local pane.
@test "daemon paints live remote output after the renderer is wired (pause-after default)" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/dp.sock" \
		>"$BATS_TEST_TMPDIR/dp.log" 2>&1 &
	daemon_pid=$!

	# Gate: renderer wired (daemon in its main loop, pause-after armed).
	for _ in $(seq 1 50); do
		cmd="$($DST list-panes -t host-sess:1 -F '#{pane_current_command}' 2>/dev/null)"
		[[ $cmd == *"$RENDERER_PROBE"* ]] && break
		sleep 0.1
	done

	# Output produced strictly after wiring — past the initial seed, so only the
	# live stream (or a %continue re-seed) can paint it.
	$SRC send-keys -t rem 'echo LIVEPAINT_9F3Q' Enter

	painted=no
	for _ in $(seq 1 50); do
		out="$($DST capture-pane -p -t host-sess:1 2>/dev/null)"
		[[ $out == *LIVEPAINT_9F3Q* ]] && {
			painted=yes
			break
		}
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$painted" = yes ]
}

# pane_map prints TARGET's panes in pane_index order, one id per line: the
# remote's own #{pane_id} for SRC, the mirror's #{@bridge_pane} carrier for DST.
# Comparing the two ORDERED lists asserts the mirror's core invariant — local
# pane i renders remote pane i — and sidesteps the base-index difference between
# the servers (SRC/DST run pane-base-index 1; the daemon forces 0 on every mirror
# window), since list-panes emits in index order either way.
pane_map() {
	$1 list-panes -t "$2" -F "$3"
}

# bridge_up starts a daemon mirroring SRC session "rem" into DST "host-sess" and
# blocks until the mirror is actually live. Sets `daemon_pid` and `sock`.
#
# It deliberately does NOT gate on pane_current_command containing the literal
# "renderer": that substring never survives macOS's MAXCOMLEN truncation (see
# setup()'s RENDERER_PROBE comment) — the same reason PR #233 switched its own
# gate to an observable paint, and #481 introduced RENDERER_PROBE for the
# sites where a truncation-safe prefix match is the right fix. bridge_up uses
# stronger observables instead, ones that need no process-name assumption at
# all:
#   1. every mirror pane carries @bridge_pane, which the daemon stamps itself;
#   2. output typed on the remote actually paints into the mirror, which is what
#      proves the daemon reached its main read loop and the router is wired — the
#      precondition every case here needs before it touches the remote.
#
# Both waits are bounded by ONE deadline, BRIDGE_UP_BUDGET_SECS, and the moment it
# passes the call fails with bridge_up_failed diagnostics. A CI stall must cost
# this suite seconds per case, not minutes: nine cases each silently burning a
# long per-observable timeout is how a missing observable turns into a
# half-hour job.
#
# 12s is ~20x the measured need, so it is a stall detector and not a race to
# beat. Measured for 1/2/3-pane mirrors: both observables arrive within 75-161ms
# idle, and within 325-596ms with the CPU oversubscribed 2x — the slowest case
# being a 2-pane mirror under load at 596ms.
BRIDGE_UP_BUDGET_SECS=12

# watchResize ticks once a second (resizePollInterval), then a control-stream
# round-trip and reconcile must land on every mirrored window. Contended CI
# needs real headroom beyond one tick — sized from the mechanism, not nudged
# until green. Must stay below resizeFallbackInterval (30s): that path is what
# the client-resized hook gate exists to avoid waiting for.
# Sizing: 1s poll + ~1s RT/reconcile × ~10x contended headroom ≈ 20s.
RESIZE_CONVERGE_BUDGET_SECS=20

# Flat BRIDGE_UP_BUDGET_SECS was measured for bridge_up's single-window path.
# The #478 cases mirror N windows/panes and assert session-wide renderer count;
# give each pane a small add-on so the gate stays honest as those tests grow.
BRIDGE_UP_PER_PANE_SECS=3

bridge_up() {
	local want_panes="$1" tag="$2"
	shift 2 # anything left is passed through to the daemon (e.g. --reflow)
	local marker="BRIDGEUP_${tag}_$$"
	local deadline=$((SECONDS + BRIDGE_UP_BUDGET_SECS))
	sock="$BATS_TEST_TMPDIR/$tag.sock"
	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$sock" "$@" \
		>"$BATS_TEST_TMPDIR/$tag.log" 2>&1 &
	daemon_pid=$!

	local stamped=0
	while [ "$SECONDS" -lt "$deadline" ]; do
		stamped="$($DST list-panes -t host-sess:1 -F '#{@bridge_pane}' 2>/dev/null | grep -c '^%' || true)"
		[ "$stamped" -eq "$want_panes" ] && break
		sleep 0.1
	done
	if [ "$stamped" -ne "$want_panes" ]; then
		bridge_up_failed "$tag" "only $stamped/$want_panes mirror panes carry @bridge_pane after ${BRIDGE_UP_BUDGET_SECS}s"
		return 1
	fi

	# Re-send the marker each round: output produced while setup's own reply reader
	# is still running is dropped rather than routed, so a single send can be lost.
	while [ "$SECONDS" -lt "$deadline" ]; do
		$SRC send-keys -t rem "printf '$marker\\n'" Enter
		local _inner
		for _inner in $(seq 1 10); do
			if mirror_contains "$want_panes" "$marker"; then
				return 0
			fi
			sleep 0.1
		done
	done
	bridge_up_failed "$tag" "mirror never painted the startup marker within ${BRIDGE_UP_BUDGET_SECS}s"
	return 1
}

# mirror_contains reports whether any of the first $1 mirror panes shows $2.
# capture-pane takes one pane, and the remote's active pane (hence the pane the
# marker lands in) is not necessarily index 0 — split-window makes the new pane
# active — so check them all.
mirror_contains() {
	local panes="$1" needle="$2" i
	for ((i = 0; i < panes; i++)); do
		if $DST capture-pane -p -t "host-sess:1.$i" 2>/dev/null | grep -q "$needle"; then
			return 0
		fi
	done
	return 1
}

# bridge_up_failed prints why the gate gave up plus the daemon's own log, so a
# CI failure here is diagnosable without a re-run.
bridge_up_failed() {
	printf 'bridge_up(%s): %s\n--- daemon log ---\n' "$1" "$2" >&3
	tail -40 "$BATS_TEST_TMPDIR/$1.log" >&3 2>/dev/null || true
}

# @bridge_state=disconnected is stamped at reattach entry, but a warm
# --test-local reconnect can clear it within a few ms. Poll tightly so darwin
# CI catches the transient stamp (100ms sleeps miss it reliably).
wait_bridge_disconnected() {
	local tag="$1" log="${2:-}"
	local state="" i
	for i in $(seq 1 200); do
		state="$($DST show-options -v -t host-sess -q @bridge_state 2>/dev/null || true)"
		[ "$state" = disconnected ] && return 0
		if [ "$i" -le 50 ]; then
			sleep 0.01
		else
			sleep 0.02
		fi
	done
	printf 'wait_bridge_disconnected(%s): last @bridge_state=%q\n--- daemon log ---\n' "$tag" "$state" >&3
	[ -n "$log" ] && tail -60 "$log" >&3 2>/dev/null || true
	return 1
}

# Regression for the pre-existing reconcile hole M2.3 had to close: layout
# traversal order means a split of a NON-LAST pane is a mid-list INSERT
# (measured: %0 %1 %2 split at %0 -> %0 %3 %1 %2), which the old three-case
# reconcile (identical / tail-append / tail-removal) classified as an
# "unsupported pane reshuffle" and skipped — leaving the mirror silently stale.
@test "daemon reconciles a mid-list remote split (non-last pane)" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40

	bridge_up 3 dmi

	# Split the FIRST remote pane: a mid-list insert, not a tail-append.
	first="$($SRC list-panes -t rem -F '#{pane_id}' | head -1)"
	$SRC split-window -v -t "$first"

	for _ in $(seq 1 60); do
		src_map="$(pane_map "$SRC" rem '#{pane_id}')"
		dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
		[ "$src_map" = "$dst_map" ] && break
		sleep 0.15
	done

	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	# 4 panes, in the remote's order, each wired to the right remote pane.
	[ "$(printf '%s\n' "$src_map" | wc -l)" -eq 4 ]
	[ "$src_map" = "$dst_map" ]
	[ "$src_dims" = "$dst_dims" ]
}

# The mirror-image hole: killing a non-last pane is a mid-list REMOVAL.
@test "daemon reconciles a mid-list remote kill (non-last pane)" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40

	bridge_up 3 dmk

	# Kill the MIDDLE remote pane.
	mid="$($SRC list-panes -t rem -F '#{pane_id}' | sed -n 2p)"
	$SRC kill-pane -t "$mid"

	for _ in $(seq 1 60); do
		src_map="$(pane_map "$SRC" rem '#{pane_id}')"
		dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
		[ "$src_map" = "$dst_map" ] && break
		sleep 0.15
	done

	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$(printf '%s\n' "$src_map" | wc -l)" -eq 2 ]
	[ "$src_map" = "$dst_map" ]
	# the killed pane is gone from the mirror's carriers
	survivor_hit="$(printf '%s\n' "$dst_map" | grep -cx "$mid" || true)"
	[ "$survivor_hit" -eq 0 ]
}

# A remote swap-pane permutes the pane set without changing its membership —
# neither a tail-append nor a tail-removal, so the old reconcile skipped it and
# each pane kept painting at its old cell. Asserts IDENTITY (which local pane
# carries which remote pane) and CONTENT (the marker moved with its pane), not
# just geometry: dims alone cannot tell a correct permutation from a no-op.
@test "daemon reconciles a remote swap-pane, content following the pane" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40

	bridge_up 2 dsw

	# Mark the FIRST remote pane, and wait for the marker to reach the mirror.
	first="$($SRC list-panes -t rem -F '#{pane_id}' | head -1)"
	$SRC send-keys -t "$first" 'echo SWAPMARK_7K2' Enter
	for _ in $(seq 1 60); do
		out="$($DST capture-pane -p -t host-sess:1.0 2>/dev/null)"
		[[ $out == *SWAPMARK_7K2* ]] && break
		sleep 0.15
	done
	[[ $out == *SWAPMARK_7K2* ]]

	# Swap it with the other pane on the remote.
	$SRC swap-pane -t "$first" -D

	for _ in $(seq 1 60); do
		src_map="$(pane_map "$SRC" rem '#{pane_id}')"
		dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
		[ "$src_map" = "$dst_map" ] && [ "$(printf '%s\n' "$src_map" | head -1)" != "$first" ] && break
		sleep 0.15
	done

	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
	# the marked pane is now second on both sides; its content must be there too
	moved="$($DST capture-pane -p -t host-sess:1.1 2>/dev/null)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	# the permutation really happened on the remote...
	[ "$(printf '%s\n' "$src_map" | head -1)" != "$first" ]
	# ...the mirror's carrier order follows it...
	[ "$src_map" = "$dst_map" ]
	# ...and the content rode along with its pane.
	[[ $moved == *SWAPMARK_7K2* ]]
}

# #544: the remote tmux answers a terminal QUERY the occupant asked (here CSI
# 6n, cursor position) straight into the remote pane's input — but the query
# bytes are also pane OUTPUT, so without keyneg's strip they'd cross the bridge
# and land in the LOCAL mirror pane's pty too, where the local tmux parses them
# as a second question and answers AGAIN, desyncing the remote occupant's
# escape parser (#338/#544). A query paints no cells, so capture-pane is green
# whether or not the byte crossed — pipe-pane on the mirror pane is the only
# instrument that actually sees it.
@test "keyneg strips a terminal query before it reaches the local mirror pane" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	bridge_up 1 kn1

	# Expanded HERE, at send time: the DST server's own environment has no $f,
	# so this has to be a double-quoted expansion, not single-quoted.
	f="$BATS_TEST_TMPDIR/kn1.pipe"
	$DST pipe-pane -o -t host-sess:1.0 "cat >> $f"

	$SRC send-keys -t rem "printf '\\033[6n%s\\n' MARKER" Enter

	# pipe-pane writes asynchronously; poll for MARKER before asserting the
	# query's absence, or an empty/not-yet-written file makes that check
	# meaningless.
	seen=no
	for _ in $(seq 1 60); do
		grep -q MARKER "$f" 2>/dev/null && {
			seen=yes
			break
		}
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$seen" = yes ]
	# The query byte itself never reached the local pty: only the remote
	# answered it, so the local tmux never had cause to reply a second time.
	run ! grep -F -- $'\033[6n' "$f"
}

# === Bridge sixel relay (R8): the raster policy gate ===
#
# A sixel crossing the bridge is either dropped (no client sixel capability to
# paint it, the default) or relayed bare, gated by Relay.Sixel() — a value
# read continuously via #{I/f:sixel} client interrogation (R6), not derived
# once from --termfeatures, and published to the remote SESSION as
# OG_RELAY_GRAPHICS (R5) — so a program there can tell whether handing the
# terminal a sixel directly will actually reach it.
#
# capture-pane cannot see any of this: tmux's own DCS parser eats a sixel, so
# it reads as green whether or not the bytes crossed. pipe-pane on the mirror
# pane — the instrument the keyneg query test above already uses — is what
# actually sees the raw bytes.
#
# Size alone does not reach C1's overflow/discard arm: drainOutput
# concatenates every queued FrameOutput before one Scanner.Feed call, so a
# burst delivered whole decodes as one complete raster and the overflow arm is
# never entered. What reaches it is the SPLIT: the introducer plus >64 KiB of
# body must land in one Feed call and the closing tail plus ST in a later,
# separate one — so the first sees an incomplete, over-budget sequence
# (forcing the discard-to-terminator path) and the second sees only the
# terminator. The scanner unit tests (picker/remotebridge/graphics/
# scan_test.go) are the primary proof of that overflow/discard behaviour;
# this bats test is only the integration witness, since its timing is not
# fully under the test's control.
#
# The split is forced with a `sleep` INSIDE one remote command line, not two
# separate send-keys calls: bash prints a fresh PS1 prompt (and, before
# `stty -echo` even matters, toggles bracketed-paste with `\e[?2004h`/`l`)
# between any two top-level commands, and either one landing on the pty
# between the tildes and the ST would corrupt assertion (b)'s byte-identical
# check — measured, not hypothesised: an earlier two-send-keys draft of this
# helper leaked exactly the next PS1 prompt into that gap. A single command
# has no such gap: `stty -echo` only needs to suppress this one line's own
# keystroke echo, and the mid-command `sleep` still gives the daemon's pump
# goroutine a real, separately-drained batch to react to before the tail is
# even produced.
#
# The marker is the pane shell's own $$ (its PID), substituted only when the
# line actually RUNS — never a literal value typed anywhere. The shell echoes
# a typed line before running it (echo is only silenced starting mid-line, by
# that same line's own `stty -echo`), so a literal marker, or one assigned by
# an earlier `export` line, would satisfy the caller's poll the instant it was
# typed — a whole second before the command reaches its `sleep 1` and
# produces anything, let alone the tail — and the caller would then kill the
# daemon out from under a sixel that was never actually written. Measured,
# not hypothesised: two earlier drafts (a literal string, then an `export`ed
# one) both did exactly this. "$$" sidesteps it: the echoed line shows the
# literal two characters "$$", and only the command's actual output — after
# the sleep, after the tail — shows the expanded PID, so the caller (which
# reads the same PID off tmux's own #{pane_pid}, never off the pty) cannot
# match early.
send_straddled_sixel() {
	$SRC send-keys -t rem "stty -echo; printf '\\033Pq'; head -c 70000 /dev/zero | tr '\\0' '~'; sleep 1; printf '~~~\\033\\\\'; printf 'SIXELDONE_%s\\n' \"\$\$\"" Enter
}

# expected_sixel_bytes writes the exact bytes send_straddled_sixel produces to
# $1 — the echoed command line itself is excluded, since `stty -echo` (the
# first thing that command runs) suppresses every byte after it.
expected_sixel_bytes() {
	printf '\033Pq' >"$1"
	head -c 70000 /dev/zero | tr '\0' '~' >>"$1"
	printf '~~~\033\134' >>"$1"
}

# relay_env echoes the remote session's copy of the relay variable.
relay_env() {
	$SRC show-environment -t rem OG_RELAY_GRAPHICS 2>/dev/null || true
}

# (a) — relay off, the RED-first assertion (spec R8): an oversized sixel must
# not reach the mirror pane's pty at all, not the introducer and not any of
# the body. This is a teeth-check, not just a positive case: swapping this
# test's --sixel 0/absent for --sixel 1 (assertion (b)'s value below) makes it
# fail, which is how it is known the absence check below is not vacuously
# green against an empty or not-yet-written pipe file.
@test "sixel relay off: an oversized sixel never reaches the mirror pane's pty" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	bridge_up 1 gxoff --termfeatures ''

	f="$BATS_TEST_TMPDIR/gxoff.pipe"
	$DST pipe-pane -o -t host-sess:1.0 "cat >> $f"

	# The remote pane's own PID, read via tmux rather than the pty, is what
	# send_straddled_sixel's command will print as "$$" once it actually runs.
	marker="SIXELDONE_$($SRC display-message -p -t rem -F '#{pane_pid}')"
	send_straddled_sixel

	# pipe-pane writes asynchronously; poll for the marker before asserting
	# the sixel's absence, or an empty/not-yet-written file makes that check
	# meaningless (same reasoning as the keyneg test above).
	seen=no
	for _ in $(seq 1 60); do
		grep -q "$marker" "$f" 2>/dev/null && {
			seen=yes
			break
		}
		sleep 0.15
	done

	# (c): the daemon's publish is unconditional, empty value included (see
	# daemon.go's comment on the send(RelayEnvCmd(...)) call) — so the remote
	# session's copy is SET, not unset, and reads back empty rather than
	# absent.
	relay_env="$(relay_env)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$seen" = yes ]
	[ "$relay_env" = "OG_RELAY_GRAPHICS=" ]
	# Not the DCS introducer...
	run ! grep -F -- $'\033Pq' "$f"
	# ...and not a run of its body either (a lone '~' is just the typed
	# command line's own literal quote-tilde-quote, harmless).
	run ! grep -E -- '~{50,}' "$f"
}

# (b) — relay on: the same bytes appear, byte-identical. This half is
# meaningful only as (a)'s paired opposite on the same harness with the gate
# flipped: with nothing to filter it, a bare sixel already reaches the mirror
# pty today, so on its own this assertion is vacuously green and proves
# nothing about the gate.
@test "sixel relay on: the same oversized sixel reaches the mirror pane's pty byte-identical" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	bridge_up 1 gxon --termfeatures sixel --sixel 1

	f="$BATS_TEST_TMPDIR/gxon.pipe"
	$DST pipe-pane -o -t host-sess:1.0 "cat >> $f"

	marker="SIXELDONE_$($SRC display-message -p -t rem -F '#{pane_pid}')"
	send_straddled_sixel

	seen=no
	for _ in $(seq 1 60); do
		grep -q "$marker" "$f" 2>/dev/null && {
			seen=yes
			break
		}
		sleep 0.15
	done

	relay_env="$(relay_env)"

	exp="$BATS_TEST_TMPDIR/gxon.expected"
	expected_sixel_bytes "$exp"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$seen" = yes ]
	[ "$relay_env" = "OG_RELAY_GRAPHICS=sixel" ]
	expected="$(cat "$exp")"
	grep -qF -- "$expected" "$f"
}

# (c) — the DYNAMIC half of R8/#574: the two tests above bake the capability in
# at daemon startup via --sixel. This one proves it instead FOLLOWS
# whichever client is attached to the LOCAL mirror session (host-sess) right
# now — attach a non-sixel viewer, confirm the drop; switch to a sixel-capable
# one, confirm the relay flips on with no re-dial (acceptance 3), and that no
# control-client replacement happened (the other half of acceptance 4: a
# capability-only change is Half 1, never Half 2's dial-verify-swap).
#
# OBS is the same third-server pty-host pattern the "bigger human client"
# test above uses, just attaching to DST's mirror session instead of SRC.
@test "the relay capability follows whichever client is attached to the mirror session" {
	OBS="tmux -L m2obs"
	$OBS kill-server 2>/dev/null || true

	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	bridge_up 1 gxv --termfeatures ''

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]

	# A non-sixel viewer attaches to the MIRROR session (not the remote).
	$OBS new-session -d -s obsA -x 100 -y 30 "env TERM=xterm-256color $DST attach -t host-sess"
	for _ in $(seq 1 40); do
		[ "$($DST list-clients -t host-sess 2>/dev/null | grep -c '^')" -ge 1 ] && break
		sleep 0.1
	done
	[ "$($DST list-clients -t host-sess 2>/dev/null | grep -c '^')" -ge 1 ]

	f1="$BATS_TEST_TMPDIR/gxv1.pipe"
	$DST pipe-pane -o -t host-sess:1.0 "cat >> $f1"

	marker1="SIXELDONE_$($SRC display-message -p -t rem -F '#{pane_pid}')"
	send_straddled_sixel
	seen1=no
	for _ in $(seq 1 60); do
		grep -q "$marker1" "$f1" 2>/dev/null && {
			seen1=yes
			break
		}
		sleep 0.15
	done
	[ "$seen1" = yes ]
	relay_env="$(relay_env)"
	[ "$relay_env" = "OG_RELAY_GRAPHICS=" ]
	run ! grep -F -- $'\033Pq' "$f1"

	# Switch the viewer to a sixel-capable terminal. Kill the old pty host's
	# SESSION, not its server, and reuse the same m2obs server for the new
	# one: killing the server and immediately re-creating it on the same
	# socket races its teardown, which surfaces as `new-session` failing with
	# "server exited unexpectedly" (measured — the isolated command works
	# fine with a wait in between, so the flag order is not the problem).
	#
	# The old client must be GONE before the new one's capability can win:
	# the gate is the AND across every attached client, so an overlap would
	# hold sixel false for as long as the 256color client stayed.
	#
	# Close the old pipe first — pipe-pane -o TOGGLES an already-open pipe off
	# rather than replacing its target (measured), so re-using -o without
	# closing would silently keep writing to $f1.
	$OBS kill-session -t obsA 2>/dev/null || true
	$DST pipe-pane -t host-sess:1.0
	f2="$BATS_TEST_TMPDIR/gxv2.pipe"
	$DST pipe-pane -o -t host-sess:1.0 "cat >> $f2"
	$OBS new-session -d -s obsB -x 100 -y 30 "env TERM=foot $DST -T sixel attach -t host-sess"
	# Poll on the identity itself, not the client count: the count is already
	# satisfied by the client we are replacing.
	for _ in $(seq 1 40); do
		[ "$($DST list-clients -t host-sess -F '#{client_termname}' 2>/dev/null | grep -c '^foot$')" -ge 1 ] && break
		sleep 0.1
	done
	[ "$($DST list-clients -t host-sess -F '#{client_termname}' 2>/dev/null | grep -c '^foot$')" -ge 1 ]
	# ...and that it is the ONLY one, or the AND cannot flip.
	[ "$($DST list-clients -t host-sess 2>/dev/null | grep -c '^')" -eq 1 ]

	for _ in $(seq 1 40); do
		relay_env="$(relay_env || true)"
		[ "$relay_env" = "OG_RELAY_GRAPHICS=sixel" ] && break
		sleep 0.15
	done

	marker2="SIXELDONE_$($SRC display-message -p -t rem -F '#{pane_pid}')"
	send_straddled_sixel
	seen2=no
	for _ in $(seq 1 60); do
		grep -q "$marker2" "$f2" 2>/dev/null && {
			seen2=yes
			break
		}
		sleep 0.15
	done

	new_transport="$(transport_child)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	$OBS kill-server 2>/dev/null || true

	# The capability flipped...
	[ "$relay_env" = "OG_RELAY_GRAPHICS=sixel" ]
	# ...and the drop policy actually followed it: the sixel bytes now reach
	# the mirror pane's pty, byte-identical, not just the env var.
	[ "$seen2" = yes ]
	exp="$BATS_TEST_TMPDIR/gxv.expected"
	expected_sixel_bytes "$exp"
	expected="$(cat "$exp")"
	grep -qF -- "$expected" "$f2"
	# No control-client replacement happened: this is Half 1 only (acceptance 4).
	[ "$new_transport" = "$old_transport" ]
}

# === M2.3: structural input (ctl -> daemon -> remote -> mirror) ===
#
# These drive the ctl binary directly against the daemon's socket, which is the
# right seam here: the bats servers run vanilla configs with no tmux-og
# keybindings, so there is nothing for a gate to intercept. The tmux-config half
# of M2.3 (the if-shell gates on @bridge_win/@bridge_pane) is therefore NOT
# covered by these tests — see tests/tmux-next38-readiness.bats for the parts of
# it that CI can see, and the PR body for what only a two-machine run can.

# remote_pane_of prints the remote pane id the mirror's local pane $1 renders,
# i.e. the @bridge_pane carrier a real keybind would pass to ctl.
remote_pane_of() {
	$DST display-message -p -t "host-sess:1.$1" -F '#{@bridge_pane}'
}

@test "ctl split-h splits the REMOTE pane and the mirror follows" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c1

	# The daemon stamps the socket on the mirror session, so a keybind can find it.
	[ "$($DST show-options -v -t host-sess @bridge_sock)" = "$sock" ]
	# ...and tags the window, which is what gates the bindings.
	[ "$($DST show-options -w -v -t host-sess:1 @bridge_win)" = "1" ]

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]

	run "$CTL" --sock "$sock" split-h "$pane"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		src_n="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
		dst_n="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"
		[ "$src_n" -eq 2 ] && [ "$dst_n" -eq 2 ] && break
		sleep 0.15
	done

	src_n="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	# The split happened on the REMOTE, not just locally.
	[ "$src_n" -eq 2 ]
	# ...and the mirror wired the new pane to it, at matching dims.
	[ "$src_map" = "$dst_map" ]
	[ "$src_dims" = "$dst_dims" ]
}

@test "ctl zoom zooms the REMOTE window and the mirror zooms with it" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -v -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c1

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]

	# Z-parse, zoom-ON direction: a zoom made directly on the remote — no ctl
	# request, so nothing schedules a reconcile but the %layout-change tmux
	# emits for it carries the flag on — from a settled unzoomed mirror. A
	# flag misread as off would read as == local and swallow the line, and
	# the mirror would never zoom.
	$SRC resize-pane -Z -t rem:1.1
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		[ "$src_z" = 1 ] && [ "$dst_z" = 1 ] && [ "$src_dims" = "$dst_dims" ] && break
		sleep 0.15
	done
	[ "$src_z" = 1 ]
	[ "$dst_z" = 1 ]
	[ "$src_dims" = "$dst_dims" ]

	# ...and back down, clearing the ON state before exercising the ctl path.
	$SRC resize-pane -Z -t rem:1.1
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		[ "$src_z" = 0 ] && [ "$dst_z" = 0 ] && break
		sleep 0.15
	done
	[ "$src_z" = 0 ]
	[ "$dst_z" = 0 ]

	run "$CTL" --sock "$sock" zoom "$pane"
	[ "$status" -eq 0 ]

	# Both observables in one wait: the flag AND the dims. The dims are the point
	# of routing zoom to the remote — the REMOTE pane is what grew, so the
	# program in it renders at the new size, where a local-only zoom leaves the
	# mirror pane bigger than the remote pane it renders (measured: dst 150x39
	# against src 150x19) and those extra rows are dead space. They settle a beat
	# after the flag does, so asserting them outside the loop races the reconcile.
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		[ "$src_z" = 1 ] && [ "$dst_z" = 1 ] && [ "$src_dims" = "$dst_dims" ] && break
		sleep 0.15
	done
	[ "$src_z" = 1 ]
	[ "$dst_z" = 1 ]
	[ "$src_dims" = "$dst_dims" ]

	# Z-parse, zoom-OFF (unzoom) direction: a zoom made directly on the
	# remote — no ctl request, so nothing schedules a reconcile but the
	# %layout-change tmux emits for it — follows too, this time against a
	# ctl-zoomed window (the ON direction above ran before any ctl call).
	$SRC resize-pane -Z -t rem:1.1
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		[ "$src_z" = 0 ] && [ "$dst_z" = 0 ] && break
		sleep 0.15
	done
	[ "$src_z" = 0 ]
	[ "$dst_z" = 0 ]

	# ...and the bind's own toggle still works, rather than latching.
	run "$CTL" --sock "$sock" zoom "$pane"
	[ "$status" -eq 0 ]
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		[ "$src_z" = 1 ] && [ "$dst_z" = 1 ] && break
		sleep 0.15
	done
	[ "$src_z" = 1 ]
	[ "$dst_z" = 1 ]

	run "$CTL" --sock "$sock" zoom "$pane"
	[ "$status" -eq 0 ]
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		[ "$src_z" = 0 ] && [ "$dst_z" = 0 ] && break
		sleep 0.15
	done

	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_z" = 0 ]
	[ "$dst_z" = 0 ]
	[ "$src_dims" = "$dst_dims" ]
}

@test "ctl resize resizes the REMOTE pane and the mirror converges" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -v -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c2

	pane="$(remote_pane_of 0)"
	before="$($SRC display-message -p -t "$pane" -F '#{pane_height}')"

	run "$CTL" --sock "$sock" resize "$pane" U 5
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		after="$($SRC display-message -p -t "$pane" -F '#{pane_height}')"
		[ "$after" != "$before" ] && [ "$(sorted_dims "$DST" host-sess:1)" = "$(sorted_dims "$SRC" rem)" ] && break
		sleep 0.15
	done

	after="$($SRC display-message -p -t "$pane" -F '#{pane_height}')"
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$after" != "$before" ] # the remote really resized
	[ "$src_dims" = "$dst_dims" ]
}

@test "ctl swap permutes the REMOTE panes and the mirror's wiring follows" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c3

	first="$(remote_pane_of 0)"

	run "$CTL" --sock "$sock" swap "$first" D
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		src_map="$(pane_map "$SRC" rem '#{pane_id}')"
		dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"
		[ "$(printf '%s\n' "$src_map" | head -1)" != "$first" ] && [ "$src_map" = "$dst_map" ] && break
		sleep 0.15
	done

	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$(printf '%s\n' "$src_map" | head -1)" != "$first" ]
	[ "$src_map" = "$dst_map" ]
}

@test "ctl kill-pane kills the REMOTE pane and the mirror loses it" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c4

	victim="$(remote_pane_of 1)"

	run "$CTL" --sock "$sock" kill-pane "$victim"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		src_n="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
		dst_n="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"
		[ "$src_n" -eq 1 ] && [ "$dst_n" -eq 1 ] && break
		sleep 0.15
	done

	src_n="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
	src_map="$(pane_map "$SRC" rem '#{pane_id}')"
	dst_map="$(pane_map "$DST" host-sess:1 '#{@bridge_pane}')"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_n" -eq 1 ]
	[ "$src_map" = "$dst_map" ]
}

@test "ctl new-window creates a REMOTE window and the mirror gains one" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c5

	pane="$(remote_pane_of 0)"

	run "$CTL" --sock "$sock" new-window "$pane"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		src_w="$($SRC list-windows -t rem -F '#{window_id}' | wc -l)"
		dst_w="$($DST list-windows -t host-sess -F '#{window_id}' | wc -l)"
		[ "$src_w" -eq 2 ] && [ "$dst_w" -eq 2 ] && break
		sleep 0.15
	done

	src_w="$($SRC list-windows -t rem -F '#{window_id}' | wc -l)"
	dst_w="$($DST list-windows -t host-sess -F '#{window_id}' | wc -l)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_w" -eq 2 ]
	[ "$dst_w" -eq 2 ]
}

@test "ctl rename renames the REMOTE window and @window_bridge_name follows" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c6

	pane="$(remote_pane_of 0)"

	run "$CTL" --sock "$sock" rename "$pane" "ctl renamed"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		src_name="$($SRC display-message -p -t rem:1 -F '#{window_name}')"
		dst_name="$($DST show-options -w -v -t host-sess:1 @window_bridge_name 2>/dev/null || true)"
		[ "$src_name" = "ctl renamed" ] && [ "$dst_name" = "ctl renamed" ] && break
		sleep 0.15
	done

	src_name="$($SRC display-message -p -t rem:1 -F '#{window_name}')"
	dst_name="$($DST show-options -w -v -t host-sess:1 @window_bridge_name 2>/dev/null || true)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_name" = "ctl renamed" ]
	[ "$dst_name" = "ctl renamed" ]
}

# Guards the two label regressions in Config.reflow's doc: a mirror window
# labeled from the launcher's cwd, and a remote rename that never repaints.
@test "daemon forces a reflow after mirroring and again after a rename" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40

	local stub="$BATS_TEST_TMPDIR/reflow-stub"
	local hits="$BATS_TEST_TMPDIR/reflow-hits"
	printf '#!/bin/sh\nprintf "%%s\\n" "$*" >>"%s"\n' "$hits" >"$stub"
	chmod +x "$stub"
	: >"$hits" # so the counts below read a file even when the daemon never fires

	bridge_up 1 c14 --reflow "$stub"

	for _ in $(seq 1 60); do
		[ -s "$hits" ] && break
		sleep 0.15
	done
	local after_mirror
	after_mirror="$(wc -l <"$hits")"

	$SRC rename-window -t rem:1 "renamed remotely"
	for _ in $(seq 1 60); do
		[ "$(wc -l <"$hits")" -gt "$after_mirror" ] && break
		sleep 0.15
	done
	local after_rename
	after_rename="$(wc -l <"$hits")"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$after_mirror" -ge 1 ]
	[ "$after_rename" -gt "$after_mirror" ]
	# --force is what gets past reflow's window-count:width cache on a rename.
	grep -q -- '--force host-sess' "$hits"
}

@test "ctl focus moves the REMOTE active pane without oscillating" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c7

	target="$(remote_pane_of 1)"
	$SRC select-pane -t "$(remote_pane_of 0)"

	run "$CTL" --sock "$sock" focus "$target" 1
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		active="$($SRC display-message -p -t rem -F '#{pane_id}')"
		[ "$active" = "$target" ] && break
		sleep 0.15
	done

	active="$($SRC display-message -p -t rem -F '#{pane_id}')"
	# Settle, then confirm it stayed put rather than ping-ponging.
	sleep 1
	still="$($SRC display-message -p -t rem -F '#{pane_id}')"
	dst_panes="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$active" = "$target" ]
	[ "$still" = "$target" ]
	[ "$dst_panes" -eq 2 ]
}

@test "ctl carousel targets its REMOTE source and a second toggle closes the viewer" {
	# Must be exported BEFORE the first $SRC command: the "remote" tmux server
	# inherits this environment when it starts.
	stub="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$stub"
	# carouselResolveScript (ctl.go) probes `tmux-claude-images --resolve` first
	# — the real script's side-effect-free seam that prints MODE\tKEY\tMANIFEST.
	# Point it at a real non-empty file so the manifest check passes.
	manifest="$BATS_TEST_TMPDIR/manifest"
	echo img >"$manifest"
	# /bin/sh, not /usr/bin/env bash: the Linux nix build sandbox has no
	# /usr/bin/env, so an env shebang leaves the stub unexecutable there and the
	# assertion below fails with an empty capture rather than a wrong one.
	cat >"$stub/tmux-claude-images" <<-EOF
		#!/bin/sh
		if [ "\$1" = --resolve ]; then
			printf 'tmux\tkey\t%s\n' "$manifest"
			exit 0
		fi
		source="\$TMUX_PANE"
		printf '%s %s\n' "\$source" "\$AEYE_BRIDGED" >>"$BATS_TEST_TMPDIR/toggled"
		viewer="\$(tmux list-panes -a -F '#{pane_id} #{@claude_img_src}' 2>>"$BATS_TEST_TMPDIR/stub.err" | awk -v source="\$source" '\$2 == source { print \$1; exit }')"
		if [ -n "\$viewer" ]; then
			tmux kill-pane -t "\$viewer" 2>>"$BATS_TEST_TMPDIR/stub.err"
		else
			viewer="\$(tmux split-window -d -P -F '#{pane_id}' -t "\$source" 'sleep 60' 2>>"$BATS_TEST_TMPDIR/stub.err")"
			tmux set-option -p -t "\$viewer" @claude_img_src "\$source" 2>>"$BATS_TEST_TMPDIR/stub.err"
		fi
	EOF
	chmod +x "$stub/tmux-claude-images"
	export PATH="$stub:$PATH"

	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c9

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]
	# `%0` is a normal first remote pane id, not a format fallback: the bridge
	# stamped it on this local pane and it is a member of the remote pane map.
	[[ $pane == %* ]]
	$SRC list-panes -t rem -F '#{pane_id}' | grep -Fx "$pane"

	run "$CTL" --sock "$sock" carousel "$pane"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		remote_panes="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
		viewer="$($SRC list-panes -t rem -F '#{pane_id} #{@claude_img_src}' | awk -v source="$pane" '$2 == source { print $1; exit }')"
		[ "$remote_panes" -eq 2 ] && [ -n "$viewer" ] && break
		sleep 0.15
	done
	if [ "$remote_panes" -ne 2 ]; then
		echo "carousel stub output: $(cat "$BATS_TEST_TMPDIR/toggled" 2>/dev/null || true)" >&3
		cat "$BATS_TEST_TMPDIR/stub.err" >&3 2>/dev/null || true
		false
	fi
	[ -n "$viewer" ]

	# This is the remote pane the local renderer for the viewer carries. Sending
	# the second request with it models prefix+I while the viewer is active.
	for _ in $(seq 1 60); do
		viewer_local="$($DST list-panes -t host-sess:1 -F '#{@bridge_pane}' | grep -Fx "$viewer" || true)"
		[ -n "$viewer_local" ] && break
		sleep 0.15
	done
	[ -n "$viewer_local" ]
	# The renderer option is written before the ctl state's asynchronous layout
	# reconcile updates its pane map, so wait for that map rather than treating a
	# just-created viewer as immediately commandable.
	for _ in $(seq 1 60); do
		run "$CTL" --sock "$sock" carousel "$viewer"
		[ "$status" -eq 0 ] && break
		sleep 0.15
	done
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		remote_panes="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
		[ "$remote_panes" -eq 1 ] && break
		sleep 0.15
	done
	got="$(cat "$BATS_TEST_TMPDIR/toggled" 2>/dev/null || true)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	# Both invocations target the source pane: the first opens the viewer and the
	# second closes that same viewer rather than opening a nested carousel.
	if [ "$got" != "$pane 1
$pane 1" ]; then
		echo "stub captured '$got', want two source-pane bridged toggles for '$pane'" >&2
		false
	fi
	[ "$remote_panes" -eq 1 ]
}

@test "q in a mirrored carousel viewer reaches the REMOTE process and dismisses it" {
	stub="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$stub"
	cat >"$stub/tmux-claude-images" <<-EOF
		#!/bin/sh
		viewer="\$(tmux split-window -d -P -F '#{pane_id}' -t "\$TMUX_PANE" "/bin/sh -c 'head -n 1 >$BATS_TEST_TMPDIR/viewer-input'")"
		tmux set-option -p -t "\$viewer" @claude_img_src "\$TMUX_PANE"
	EOF
	chmod +x "$stub/tmux-claude-images"
	export PATH="$stub:$PATH"

	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 cq
	source="$(remote_pane_of 0)"
	run "$CTL" --sock "$sock" carousel "$source"
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		viewer="$($SRC list-panes -t rem -F '#{pane_id} #{@claude_img_src}' | awk -v source="$source" '$2 == source { print $1; exit }')"
		local_viewer="$($DST list-panes -t host-sess:1 -F '#{pane_id} #{@bridge_pane}' | awk -v viewer="$viewer" '$2 == viewer { print $1; exit }')"
		[ -n "$viewer" ] && [ -n "$local_viewer" ] && break
		sleep 0.15
	done
	[ -n "$viewer" ]
	[ -n "$local_viewer" ]

	for _ in $(seq 1 60); do
		local_viewer="$($DST list-panes -t host-sess:1 -F '#{pane_id} #{@bridge_pane}' | awk -v viewer="$viewer" '$2 == viewer { print $1; exit }')"
		if [ -n "$local_viewer" ]; then
			run $DST send-keys -t "$local_viewer" q Enter
		fi
		remote_panes="$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)"
		local_panes="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)"
		[ -f "$BATS_TEST_TMPDIR/viewer-input" ] && [ "$remote_panes" -eq 1 ] && [ "$local_panes" -eq 1 ] && break
		sleep 0.15
	done
	input="$(tr -d '\n' <"$BATS_TEST_TMPDIR/viewer-input" 2>/dev/null || true)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	[ "$input" = q ]
	[ "$remote_panes" -eq 1 ]
	[ "$local_panes" -eq 1 ]
}

# The queue-safety guarantee: a gated keypress against a dead daemon must fail
# fast and non-zero, never stall tmux's command queue on a dial that hangs.
@test "ctl against a dead socket fails fast and non-zero" {
	dead="$BATS_TEST_TMPDIR/dead.sock"
	start=$(date +%s)
	run "$CTL" --sock "$dead" split-h '%1'
	elapsed=$(($(date +%s) - start))
	[ "$status" -ne 0 ]
	[ "$elapsed" -lt 5 ]
	[[ $output == *"unreachable"* ]]
}

# An unmirrored pane id must be refused rather than acted on.
@test "ctl refuses a pane this bridge does not mirror" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c8

	run "$CTL" --sock "$sock" split-h '%999'
	status_seen="$status"
	output_seen="$output"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$status_seen" -ne 0 ]
	[[ $output_seen == *"not mirrored"* ]]
}

# Agent status: a control-mode client renders no status line, so the remote's
# own #() pollers never run for a bridged session — claude-status-update stamps
# the state on the pane instead, and the daemon re-keys it onto the LOCAL pane
# id, which is what every local consumer reads.
@test "daemon ships the remote's agent status into the local claude-status tree" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"

	$SRC new-session -d -s rem -x 120 -y 34
	$DST new-session -d -s host-sess -x 120 -y 34

	remote_pane="$($SRC list-panes -t rem -F '#{pane_id}')"
	$SRC set -p -t "$remote_pane" @claude_status "waiting $(date +%s) 1"
	$SRC set -p -t "$remote_pane" @claude_task "ship it"

	bridge_up 1 cst

	local_pane="$($DST list-panes -t host-sess:1 -F '#{pane_id}')"
	pane_file="$CLAUDE_STATUS_DIR/panes/${local_pane#%}"
	# The poll rides the main loop's drain, so it needs the stream to wake:
	# keep producing remote output until the file lands.
	for _ in $(seq 1 40); do
		[ -f "$pane_file" ] && break
		$SRC send-keys -t rem "printf 'tick\\n'" Enter
		sleep 0.2
	done
	body="$(cat "$pane_file" 2>/dev/null || true)"
	task="$(cat "$CLAUDE_STATUS_DIR/tasks/${local_pane#%}" 2>/dev/null || true)"
	# The mirror pane runs a renderer, so its own command says nothing;
	# @bridge_proc carries the remote's, which is what the icons are built from.
	remote_proc="$($SRC list-panes -t rem -F '#{pane_current_command}')"
	bridge_proc="$($DST show-options -p -t "$local_pane" -qv @bridge_proc)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[[ $body == *"state=waiting"* ]]
	[[ $body == *"session=host-sess"* ]]
	[[ $body == *"unseen=1"* ]]
	[ "$task" = "ship it" ]
	[ -n "$remote_proc" ]
	[ "$bridge_proc" = "$remote_proc" ]
	# The bridge owns what it wrote: SIGTERM teardown takes it away again.
	[ ! -f "$pane_file" ]
}

# The daemon is the sole producer of the @bridge_* window namespace: it polls
# the remote's window options and stamps sanitized copies onto the mirrors. It
# never writes @crew_*/@window_label_*/@pr_*, which tmux-reflow-windows owns on
# every window of the mirror session, mirrors included.
@test "daemon ships the remote's window labels onto the mirror windows" {
	$SRC new-session -d -s rem -x 120 -y 34
	$DST new-session -d -s host-sess -x 120 -y 34

	$SRC set -w -t rem:1 @crew_name nova
	$SRC set -w -t rem:1 @crew_color '#89b4fa'
	$SRC set -w -t rem:1 @pr_number 123
	$SRC set -w -t rem:1 @pr_state open
	$SRC set -w -t rem:1 @window_pr_plain ' PR #123'
	$SRC set -w -t rem:1 @window_label_id 'GH #460'
	# The one free-form field, and last in the read format: the '|' must land
	# inside it rather than shifting the row, and the markup must not survive.
	$SRC set -w -t rem:1 @window_label_rest_long ' a #[fg=red]title | piped'

	# The nine bridge-state options the enrich card reads in mirror mode (#598).
	# @pr_title carries a literal '|': unlike @window_label_rest_long above, this
	# field IS wrapped in the remote's '#{s/[|]/ /:…}', so it is the other
	# end-to-end proof that substitution actually fires — here as one space, not
	# a dropped pipe.
	$SRC set -w -t rem:1 @issue_provider linear
	$SRC set -w -t rem:1 @issue_id ENG-460
	$SRC set -w -t rem:1 @issue_url 'https://linear.app/factify/issue/ENG-460'
	$SRC set -w -t rem:1 @pr_url 'https://github.com/noamsto/lazytmux/pull/460'
	$SRC set -w -t rem:1 @pr_draft 1
	$SRC set -w -t rem:1 @branch feat/460-card
	$SRC set -w -t rem:1 @worktree /home/rem/wt/460
	$SRC set -w -t rem:1 @issue_title 'card reads bridge state'
	$SRC set -w -t rem:1 @pr_title 'fix|the thing'

	bridge_up 1 lbl

	# The bare-mirror half needs a window created AFTER bridge_up: bridge_up
	# waits on the first window, which is the one stamped above.
	$SRC new-window -t rem

	# Gate on the mirror of that second window existing too — reconcileWindows
	# runs on the main loop's next pass, so a missing window would satisfy the
	# unset assertion below for the wrong reason.
	for _ in $(seq 1 40); do
		crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
		bare_win="$($DST show-options -w -t host-sess:2 -qv @bridge_win 2>/dev/null || true)"
		[ "$crew" = "nova" ] && [ "$bare_win" = "1" ] && break
		$SRC send-keys -t rem:1 "printf 'tick\\n'" Enter
		sleep 0.2
	done

	crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
	color="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_color 2>/dev/null || true)"
	pr_number="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_number 2>/dev/null || true)"
	pr_state="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_state 2>/dev/null || true)"
	pr_plain="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_plain 2>/dev/null || true)"
	label_id="$($DST show-options -w -t host-sess:1 -qv @bridge_label_id 2>/dev/null || true)"
	label_rest="$($DST show-options -w -t host-sess:1 -qv @bridge_label_rest_long 2>/dev/null || true)"
	issue_provider="$($DST show-options -w -t host-sess:1 -qv @bridge_issue_provider 2>/dev/null || true)"
	issue_id="$($DST show-options -w -t host-sess:1 -qv @bridge_issue_id 2>/dev/null || true)"
	issue_url="$($DST show-options -w -t host-sess:1 -qv @bridge_issue_url 2>/dev/null || true)"
	pr_url="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_url 2>/dev/null || true)"
	pr_draft="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_draft 2>/dev/null || true)"
	branch="$($DST show-options -w -t host-sess:1 -qv @bridge_branch 2>/dev/null || true)"
	dir="$($DST show-options -w -t host-sess:1 -qv @bridge_dir 2>/dev/null || true)"
	issue_title="$($DST show-options -w -t host-sess:1 -qv @bridge_issue_title 2>/dev/null || true)"
	pr_title="$($DST show-options -w -t host-sess:1 -qv @bridge_pr_title 2>/dev/null || true)"
	bare_win="$($DST show-options -w -t host-sess:2 -qv @bridge_win 2>/dev/null || true)"
	# An empty remote value UNSETS the local option. `show-options -qv` returns
	# empty for unset and for "" alike, so list what the window actually holds;
	# @bridge_win is the daemon's own mirror marker, not a label copy.
	bare_labels="$($DST show-options -w -t host-sess:2 2>/dev/null | grep '^@bridge_' | grep -v '^@bridge_win ' || true)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$crew" = "nova" ]
	[ "$color" = "#89b4fa" ]
	[ "$pr_number" = "123" ]
	[ "$pr_state" = "open" ]
	# The leading space is load-bearing for reflow's pr_colw padding.
	[ "$pr_plain" = " PR #123" ]
	[ "$label_id" = "GH #460" ]
	[ "$label_rest" = " a title  piped" ]
	[ "$issue_provider" = "linear" ]
	[ "$issue_id" = "ENG-460" ]
	[ "$issue_url" = "https://linear.app/factify/issue/ENG-460" ]
	[ "$pr_url" = "https://github.com/noamsto/lazytmux/pull/460" ]
	[ "$pr_draft" = "1" ]
	[ "$branch" = "feat/460-card" ]
	[ "$dir" = "/home/rem/wt/460" ]
	[ "$issue_title" = "card reads bridge state" ]
	[ "$pr_title" = "fix the thing" ]
	[ "$bare_win" = "1" ]
	[ -z "$bare_labels" ]
}

# The two cases that can tell a subscription from a backstop read: after the
# mirror has settled, neither creates a window or a pane — so the registry
# generation is unchanged — and the next backstop read is 30s away, well outside
# the budget below. Nothing but a %subscription-changed can carry the second
# value.
@test "a remote label change reaches the mirror on a subscription, not a poll" {
	$SRC new-session -d -s rem -x 120 -y 34
	$DST new-session -d -s host-sess -x 120 -y 34
	$SRC set -w -t rem:1 @crew_name nova

	bridge_up 1 subl

	# Settle first: the shipper's opening read is due at daemon start, so this
	# value proves nothing on its own.
	for _ in $(seq 1 40); do
		crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
		[ "$crew" = "nova" ] && break
		sleep 0.2
	done
	[ "$crew" = "nova" ]

	# The discriminator. Deliberately no send-keys: the notification is itself
	# the stream traffic that wakes the loop, so this asserts the push, not a
	# poll riding somebody else's output.
	$SRC set -w -t rem:1 @crew_name orbit
	for _ in $(seq 1 40); do
		crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
		[ "$crew" = "orbit" ] && break
		sleep 0.2
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$crew" = "orbit" ]
}

@test "a remote agent stamp reaches the local state tree on a subscription" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"

	$SRC new-session -d -s rem -x 120 -y 34
	$DST new-session -d -s host-sess -x 120 -y 34
	remote_pane="$($SRC list-panes -t rem -F '#{pane_id}')"
	$SRC set -p -t "$remote_pane" @claude_status "waiting $(date +%s) 1"

	bridge_up 1 suba

	local_pane="$($DST list-panes -t host-sess:1 -F '#{pane_id}')"
	pane_file="$CLAUDE_STATUS_DIR/panes/${local_pane#%}"
	for _ in $(seq 1 40); do
		[ -f "$pane_file" ] && break
		sleep 0.2
	done
	[ -f "$pane_file" ]

	# Pane scope (%*) rather than the window scope above, and a second
	# subscription: worth proving on the wire separately.
	$SRC set -p -t "$remote_pane" @claude_status "error $(date +%s) 1"
	for _ in $(seq 1 40); do
		body="$(cat "$pane_file" 2>/dev/null || true)"
		[[ $body == *"state=error"* ]] && break
		sleep 0.2
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[[ $body == *"state=error"* ]]
}

# Screen-scraped agents (pi, codex, cursor) have no hook, so agent-detect's
# statefile.Writer stamps @agent_screen instead of @claude_status (#635). The
# daemon must carry it to screen/<local_pane_id>, independently of panes/ —
# which a screen-only pane never gets.
@test "a remote screen-scraped stamp reaches the local screen tree on a subscription" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"

	$SRC new-session -d -s rem -x 120 -y 34
	$DST new-session -d -s host-sess -x 120 -y 34
	remote_pane="$($SRC list-panes -t rem -F '#{pane_id}')"
	$SRC set -p -t "$remote_pane" @agent_screen "processing $(date +%s) bg=1"

	bridge_up 1 scrn

	local_pane="$($DST list-panes -t host-sess:1 -F '#{pane_id}')"
	screen_file="$CLAUDE_STATUS_DIR/screen/${local_pane#%}"
	pane_file="$CLAUDE_STATUS_DIR/panes/${local_pane#%}"
	for _ in $(seq 1 40); do
		[ -f "$screen_file" ] && break
		sleep 0.2
	done
	[ -f "$screen_file" ]
	# A screen-only pane never gets a hook-driven panes/ file.
	[ ! -f "$pane_file" ]

	body="$(cat "$screen_file" 2>/dev/null || true)"
	[[ $body == *"state=processing"* ]]
	[[ $body == *"bg=1"* ]]

	# A second stamp proves the subscription, not a one-shot backstop read.
	$SRC set -p -t "$remote_pane" @agent_screen "idle $(date +%s)"
	for _ in $(seq 1 40); do
		body="$(cat "$screen_file" 2>/dev/null || true)"
		[[ $body == *"state=idle"* ]] && break
		sleep 0.2
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[[ $body == *"state=idle"* ]]
	# bg=1 belonged to the first stamp only; the second carried no flags.
	[[ $body != *"bg="* ]]
}

# run_detach runs og-remote-detach against $1 under a `tmux` that is pinned
# to the DST server: the script calls a bare `tmux` (correct in production), and
# the absolute path inside the stub keeps it from re-entering itself. DETACH is
# the store path of the script; only tests/ exists in the check sandbox.
run_detach() {
	local real_tmux stub detach
	real_tmux="$(command -v tmux)"
	stub="$BATS_TEST_TMPDIR/detachbin"
	mkdir -p "$stub"
	printf '#!/bin/sh\nexec %s -L m2dst "$@"\n' "$real_tmux" >"$stub/tmux"
	chmod +x "$stub/tmux"
	detach="${DETACH:-$BATS_TEST_DIRNAME/../scripts/og-remote-detach.sh}"
	run env PATH="$stub:$PATH" bash "$detach" "$1"
}

@test "detach drops the mirror and leaves the REMOTE session running" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 c12

	run_detach host-sess
	[ "$status" -eq 0 ]

	for _ in $(seq 1 60); do
		kill -0 "$daemon_pid" 2>/dev/null || break
		sleep 0.15
	done

	# Read the mirror's fate BEFORE the cleanup kill: that kill would run the
	# very teardown under test, so asserting after it passes either way — and an
	# unconditional `wait` on a daemon that ignored the detach hangs the test
	# instead of failing it.
	daemon_alive=1
	kill -0 "$daemon_pid" 2>/dev/null || daemon_alive=0
	mirror_gone=0
	$DST has-session -t =host-sess 2>/dev/null || mirror_gone=1

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$daemon_alive" -eq 0 ]
	# A killed last session takes the DST server with it, which fails has-session
	# the same way.
	[ "$mirror_gone" -eq 1 ]

	# The remote kept its session and both panes: teardown closed a control-mode
	# client, it did not kill anything over there.
	run $SRC has-session -t =rem
	[ "$status" -eq 0 ]
	[ "$($SRC list-panes -t rem -F '#{pane_id}' | wc -l)" -eq 2 ]
}

@test "detach drops the mirror even when the daemon can no longer tear itself down" {
	$SRC new-session -d -s rem -x 150 -y 40
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 1 c13

	# SIGKILL leaves the socket, the pidfile and the mirror session behind: the
	# state a wedged daemon presents, minus the wait.
	kill -KILL "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	run_detach host-sess
	[ "$status" -eq 0 ]

	mirror_gone=0
	$DST has-session -t =host-sess 2>/dev/null || mirror_gone=1
	[ "$mirror_gone" -eq 1 ]

	run $SRC has-session -t =rem
	[ "$status" -eq 0 ]
}

# #396: a control-mode client only receives %output for the session it is
# attached to. A mirror pane's keystrokes reach the REMOTE shell, where $TMUX is
# set, so `sesh connect` runs switch-client with no -c and tmux resolves
# "current client" to the daemon — the only client the bridged session has. The
# mirror then freezes: input still lands, nothing repaints. The daemon must pin
# itself back, and hand the session it was switched to off to a mirror of its
# own.
@test "a remote switch-client is pinned back, the mirror repaints, and the session hands off" {
	$SRC new-session -d -s rem -x 100 -y 30
	$SRC new-session -d -s other -x 100 -y 30 # what `sesh connect` would land on
	$DST new-session -d -s host-sess -x 100 -y 30

	# Stub stands in for og-remote-open: records the hand-off argv instead
	# of starting a second daemon. /bin/sh, not /usr/bin/env: the nix build
	# sandbox has no /usr/bin, so an env shebang never execs.
	open_stub="$BATS_TEST_TMPDIR/remote-open-stub"
	cat >"$open_stub" <<EOF
#!/bin/sh
printf '%s\n' "\$@" >"$BATS_TEST_TMPDIR/handoff.args"
EOF
	chmod +x "$open_stub"

	bridge_up 1 pin --host lab --remote-open "$open_stub"

	# The excursion, exactly as sesh performs it.
	$SRC switch-client -t other

	back=no
	for _ in $(seq 1 60); do
		[[ "$($SRC list-clients -F '#{client_session}' | head -1)" == rem ]] && {
			back=yes
			break
		}
		sleep 0.1
	done

	# Output produced after the pin: only a live stream restored to our session
	# can paint it.
	$SRC send-keys -t rem 'echo PINNED_7K2M' Enter
	painted=no
	for _ in $(seq 1 60); do
		mirror_contains 1 PINNED_7K2M && {
			painted=yes
			break
		}
		sleep 0.15
	done

	handoff=""
	for _ in $(seq 1 40); do
		[[ -s "$BATS_TEST_TMPDIR/handoff.args" ]] && {
			handoff="$(tr '\n' ' ' <"$BATS_TEST_TMPDIR/handoff.args")"
			break
		}
		sleep 0.1
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$back" = yes ]
	[ "$painted" = yes ]
	[ "$handoff" = "lab other " ]
}

# m2_pane_gate_failed prints why a renderer-pane readiness gate gave up, plus
# both sides' pane lists and the daemon's own log, so a CI-only failure here is
# diagnosable without a re-run.
m2_pane_gate_failed() {
	local log="$1" got="$2" want="$3"
	printf 'm2 pane gate: got %s/%s renderer panes\n--- SRC panes ---\n' "$got" "$want" >&3
	$SRC list-panes -s -t rem -F '#{window_index} #{pane_id} #{pane_current_command}' >&3 2>&1 || true
	printf -- '--- DST panes ---\n' >&3
	$DST list-panes -s -t host-sess -F '#{window_index} #{pane_id} #{pane_current_command}' >&3 2>&1 || true
	printf -- '--- daemon log ---\n' >&3
	tail -60 "$log" >&3 2>/dev/null || true
}

# Regression for #478: tmux-og sets `aggressive-resize on`
# (config/tmux.conf.nix), so every remote window inherits it, and tmux then
# sizes a window only from clients whose session currently has that window
# selected. The bridge holds ONE control client on the mirrored session, so
# every mirrored window except the remote's current one had its
# `refresh-client -C @N:WxH` silently discarded and sat at whatever size it
# last held. Mirror two windows and assert EVERY remote window converges, not
# just the current one.
@test "daemon converges every mirrored window, not just the remote's current one" {
	$SRC new-session -d -s rem -x 120 -y 40
	$SRC new-window -t 'rem:{end}' -a
	# new-window selects what it creates; put the remote back on window 1 so
	# window 2 is the one that used to be skipped.
	$SRC select-window -t rem:1
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/d478.sock" \
		>"$BATS_TEST_TMPDIR/d478.log" 2>&1 &
	daemon_pid=$!

	# Gate on the MIRROR settling — one renderer pane per remote pane across
	# every mirrored window. A source-side width is not a proxy: SRC reaches
	# 100 as soon as the daemon sizes its own control client (#449), before any
	# mirror pane exists. Budget scales with pane count: flat BRIDGE_UP was
	# measured for bridge_up's single-window path.
	want_panes="$($SRC list-panes -s -t rem -F '#{pane_id}' | wc -l)"
	for _ in $(seq 1 "$(((BRIDGE_UP_BUDGET_SECS + want_panes * BRIDGE_UP_PER_PANE_SECS) * 10))"); do
		got_panes="$($DST list-panes -s -t host-sess -F '#{pane_current_command}' 2>/dev/null | grep -c "$RENDERER_PROBE")" || got_panes=0
		[ "$got_panes" -eq "$want_panes" ] && break
		sleep 0.1
	done
	# Fail here, not below, when the mirror itself never came up — a bring-up
	# timeout must not be reported as a convergence failure.
	[ "$got_panes" -eq "$want_panes" ] || m2_pane_gate_failed "$BATS_TEST_TMPDIR/d478.log" "$got_panes" "$want_panes"
	[ "$got_panes" -eq "$want_panes" ]

	# Poll every remote window down to the local size. Setup-path converge
	# (not watchResize) so BRIDGE_UP_BUDGET_SECS is the right floor — but fail
	# inside this gate on expiry rather than falling through to a bare width
	# assert that looks like a different failure.
	deadline=$((SECONDS + BRIDGE_UP_BUDGET_SECS))
	whs=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		whs="$($SRC list-windows -t rem -F '#{window_width}x#{window_height}' | sort -u)"
		[ "$(printf '%s\n' "$whs" | wc -l)" -eq 1 ] && [ "${whs%x*}" = 100 ] && break
		sleep 0.1
	done
	if ! { [ "$(printf '%s\n' "$whs" | wc -l)" -eq 1 ] && [ "${whs%x*}" = 100 ]; }; then
		echo "SRC width converge timeout after ${BRIDGE_UP_BUDGET_SECS}s: wanted unique width 100, got:" >&3
		$SRC list-windows -t rem -F '#{window_index} #{window_width}x#{window_height}' >&3
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		return 1
	fi
	src_whs="$($SRC list-windows -t rem -F '#{window_index} #{window_width}x#{window_height}')"
	n_windows="$($SRC list-windows -t rem -F '#{window_id}' | wc -l)"
	ref_wh="$($SRC display-message -p -t rem:1 -F '#{window_width}x#{window_height}' 2>/dev/null)"
	# The local mirror needs one more round trip after SRC converges — the
	# daemon reacts to the resulting %layout-change and re-fits each DST pane —
	# so poll for parity too instead of comparing a single snapshot of each
	# side. Capture before killing: teardown drops the mirror session.
	deadline=$((SECONDS + BRIDGE_UP_BUDGET_SECS))
	src_dims=""
	dst_dims=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		src_dims="$($SRC list-panes -s -t rem -F '#{pane_width}x#{pane_height}' | sort)"
		dst_dims="$($DST list-panes -s -t host-sess -F '#{pane_width}x#{pane_height}' | sort)"
		[ "$src_dims" = "$dst_dims" ] && break
		sleep 0.1
	done
	if [ "$src_dims" != "$dst_dims" ]; then
		echo "DST parity timeout after ${BRIDGE_UP_BUDGET_SECS}s:" >&3
		echo "  src_dims=$src_dims" >&3
		echo "  dst_dims=$dst_dims" >&3
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		return 1
	fi

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$n_windows" -eq 2 ]
	[ "${ref_wh%x*}" = 100 ]
	# Every window at the SAME WxH as window 1 — a width-only check would miss
	# an asymmetric per-window bug (width converges, height doesn't, or the
	# reverse).
	stragglers="$(printf '%s\n' "$src_whs" | awk -v ref="$ref_wh" '$2 != ref')"
	[ -z "$stragglers" ] || {
		echo "windows that never converged to $ref_wh: $stragglers" >&2
		false
	}
	# And the local mirror's panes match the remote's, window for window.
	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
}

# Regression for #478 (resize leg): AggressiveResizeOffCmd is a one-time,
# unconditional opt-out sent at the top of setupWindow and persists as a tmux
# window option — watchResize has no aggressive-resize handling of its own;
# it only re-issues ConvergeCmd for windows already in the registry. So this
# covers the same invariant as the setup-leg test above, reached through a live
# client resize rather than the daemon's startup path.
# It does not (and cannot) exercise the reg.add-before-opt-out goroutine
# race; that is covered deterministically by
# TestSetupWindowCapsAWindowWatchResizeAlreadyRecorded in
# picker/remotebridge/daemon/setupwindow_test.go.
@test "daemon re-converges every mirrored window after a client resize" {
	OBS="tmux -L m2obs"
	$OBS kill-server 2>/dev/null || true

	$SRC new-session -d -s rem -x 120 -y 40
	$SRC new-window -t 'rem:{end}' -a
	# new-window selects what it creates; put the remote back on window 1 so
	# window 2 is the one that used to be skipped.
	$SRC select-window -t rem:1
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/d478r.sock" \
		>"$BATS_TEST_TMPDIR/d478r.log" 2>&1 &
	daemon_pid=$!

	# A real pty-hosted client attached to the LOCAL mirror (not the remote):
	# watchResize reads clientArea off the mirror session, so only a client
	# actually attached there can nudge it.
	$OBS new-session -d -s obs -x 100 -y 30 "$DST attach -t host-sess"
	for _ in $(seq 1 40); do
		clients="$($DST list-clients -t host-sess 2>/dev/null | wc -l | tr -d ' ')"
		[ "$clients" -ge 1 ] && break
		sleep 0.1
	done
	[ "$clients" -ge 1 ]

	# Gate on the MIRROR settling before resizing — one renderer pane per
	# remote pane across every mirrored window, same gate as the setup-leg test.
	# Budget scales with pane count: flat BRIDGE_UP was measured for
	# bridge_up's single-window path.
	want_panes="$($SRC list-panes -s -t rem -F '#{pane_id}' | wc -l)"
	for _ in $(seq 1 "$(((BRIDGE_UP_BUDGET_SECS + want_panes * BRIDGE_UP_PER_PANE_SECS) * 10))"); do
		got_panes="$($DST list-panes -s -t host-sess -F '#{pane_current_command}' 2>/dev/null | grep -c "$RENDERER_PROBE")" || got_panes=0
		[ "$got_panes" -eq "$want_panes" ] && break
		sleep 0.1
	done
	# Fail here, not below, when the mirror itself never came up — a bring-up
	# timeout must not be reported as a resize failure.
	[ "$got_panes" -eq "$want_panes" ] || m2_pane_gate_failed "$BATS_TEST_TMPDIR/d478r.log" "$got_panes" "$want_panes"
	[ "$got_panes" -eq "$want_panes" ]

	# registerResizeHook (daemon.go) only wires client-resized/window-resized on
	# host-sess AFTER reconcileWindows, which runs after the per-window setup
	# loop above — so "every renderer pane is up" does not imply "a resize is
	# observable yet". A resize fired in that gap is not lost (watchResize's
	# resizeFallbackInterval still catches it) but that fallback is 30s, well
	# past this test's poll budget below — so gate on the hook itself.
	# Not a resize-converge wait: BRIDGE_UP is enough to see the hook land.
	for _ in $(seq 1 "$((BRIDGE_UP_BUDGET_SECS * 10))"); do
		$DST show-hooks -t host-sess 2>/dev/null | grep -q '^client-resized' && break
		sleep 0.1
	done
	$DST show-hooks -t host-sess 2>/dev/null | grep -q '^client-resized'

	# The gesture: resize the attached client.
	$OBS resize-window -t obs -x 90 -y 28

	# Poll SRC-side window widths: the production invariant under test is that
	# every remote window receives the new client cap (aggressive-resize used
	# to drop refresh-client for non-current windows). DST pane-dim parity
	# below is the settle-last mirror gate, not a substitute for this check.
	# RESIZE_CONVERGE_BUDGET_SECS — watchResize's own floor, not BRIDGE_UP.
	deadline=$((SECONDS + RESIZE_CONVERGE_BUDGET_SECS))
	whs=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		whs="$($SRC list-windows -t rem -F '#{window_width}x#{window_height}' | sort -u)"
		[ "$(printf '%s\n' "$whs" | wc -l)" -eq 1 ] && [ "${whs%x*}" = 90 ] && break
		sleep 0.1
	done
	if ! { [ "$(printf '%s\n' "$whs" | wc -l)" -eq 1 ] && [ "${whs%x*}" = 90 ]; }; then
		echo "SRC resize converge timeout after ${RESIZE_CONVERGE_BUDGET_SECS}s: wanted unique width 90, got:" >&3
		$SRC list-windows -t rem -F '#{window_index} #{window_width}x#{window_height}' >&3
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		$OBS kill-server 2>/dev/null || true
		return 1
	fi
	src_whs="$($SRC list-windows -t rem -F '#{window_index} #{window_width}x#{window_height}')"
	n_windows="$($SRC list-windows -t rem -F '#{window_id}' | wc -l)"
	ref_wh="$($SRC display-message -p -t rem:1 -F '#{window_width}x#{window_height}' 2>/dev/null)"
	# The local mirror needs one more round trip after SRC converges — the
	# daemon reacts to the resulting %layout-change and re-fits each DST pane —
	# so poll for parity too instead of comparing a single snapshot of each
	# side. Capture before killing: teardown drops the mirror session.
	# Post-SRC-converge DST re-fit can lag SRC under load; use
	# RESIZE_CONVERGE_BUDGET_SECS — not BRIDGE_UP.
	deadline=$((SECONDS + RESIZE_CONVERGE_BUDGET_SECS))
	src_dims=""
	dst_dims=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		src_dims="$($SRC list-panes -s -t rem -F '#{pane_width}x#{pane_height}' | sort)"
		dst_dims="$($DST list-panes -s -t host-sess -F '#{pane_width}x#{pane_height}' | sort)"
		[ "$src_dims" = "$dst_dims" ] && break
		sleep 0.1
	done
	if [ "$src_dims" != "$dst_dims" ]; then
		echo "DST parity timeout after ${RESIZE_CONVERGE_BUDGET_SECS}s:" >&3
		echo "  src_dims=$src_dims" >&3
		echo "  dst_dims=$dst_dims" >&3
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		$OBS kill-server 2>/dev/null || true
		return 1
	fi

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	$OBS kill-server 2>/dev/null || true

	[ "$n_windows" -eq 2 ]
	[ "${ref_wh%x*}" = 90 ]
	# Every window at the SAME WxH as window 1 — a width-only check would miss
	# an asymmetric per-window bug (width converges, height doesn't, or the
	# reverse).
	stragglers="$(printf '%s\n' "$src_whs" | awk -v ref="$ref_wh" '$2 != ref')"
	[ -z "$stragglers" ] || {
		echo "windows that never converged to $ref_wh: $stragglers" >&2
		false
	}
	# And the local mirror's panes match the remote's, window for window.
	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
}

# #482: a dropped control connection must not destroy the mirror. Below, the
# DROP is always a SIGKILL of the transport child (the `tmux -L m2src -C
# attach-session` the daemon's own Dial spawned), found by matching on
# "attach-session" rather than blindly taking the daemon's first child: a
# transient `tmux -L m2dst set-option ...` the daemon forks for a local-tmux
# call is also, briefly, one of its children.
#
# The distinction is load-bearing (measured on the pinned next-3.8 tmux):
# `detach-client`/`kill-server` end the control client with a terminal %exit,
# while only killing the transport out from under a live stdin/stdout pipe
# produces the bare EOF that is a DROP — see the design spec's "Test strategy".
#
# `ps -Ao`, not pgrep: nixpkgs' `procps` on darwin is unixtools' shim, which
# ships ps/sysctl/top/watch and NO pgrep, so a pgrep-based probe returns empty
# on macOS and every case below fails at its first assertion. Same portability
# rules as picker's psArgs — `-A` (POSIX), never `-e` (BSD ps reads that as
# "show environment"), and no GNU-only `--no-headers`: the header row's PPID
# column is the string "PPID", which no numeric pid ever equals.
transport_child() {
	ps -Ao pid,ppid,args 2>/dev/null |
		awk -v parent="$daemon_pid" '$2 == parent && /attach-session/ {print $1; exit}'
}

@test "a control-connection drop leaves the mirror standing and reattaches to the same remote server" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 drc

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]
	kill -9 "$old_transport"

	# @bridge_state is stamped BEFORE the first re-dial attempt, so it must
	# appear within about one status tick of the drop.
	wait_bridge_disconnected drc "$BATS_TEST_TMPDIR/drc.log"

	# The mirror itself must survive: same session, same window, same pane —
	# nothing torn down by the drop.
	run $DST has-session -t =host-sess
	[ "$status" -eq 0 ]
	[ "$($DST list-panes -t host-sess:1 -F '#{pane_id}' | wc -l)" -eq 1 ]

	# Content produced on SRC while disconnected is not buffered for the
	# control stream — it lands in the pane's own screen regardless of
	# whether any client is attached, so this is what the reattach's
	# capture-pane reseed must pick up. Sent now, asserted only after
	# reconnect: reaching the mirror is proof the reseed ran, not mere
	# window survival.
	$SRC send-keys -t rem 'echo DROP_RECONNECT_4X8P' Enter

	# Reattach: a NEW control client, distinct from the one just killed.
	new_transport=""
	for _ in $(seq 1 80); do
		candidate="$(transport_child)"
		[ -n "$candidate" ] && [ "$candidate" != "$old_transport" ] && {
			new_transport="$candidate"
			break
		}
		sleep 0.1
	done
	[ -n "$new_transport" ]
	run $SRC list-clients -t rem
	[ "$status" -eq 0 ]

	# Cleared only once the reattach repair (resume + reconcile + reseed) has
	# actually completed — not on the bare re-attach.
	for _ in $(seq 1 80); do
		state="$($DST show-options -v -t host-sess -q @bridge_state 2>/dev/null || true)"
		[ -z "$state" ] && break
		sleep 0.1
	done
	[ -z "$state" ]

	painted=no
	for _ in $(seq 1 60); do
		mirror_contains 1 DROP_RECONNECT_4X8P && {
			painted=yes
			break
		}
		sleep 0.15
	done
	# R5's second half: repair() re-asserts the capability unconditionally on
	# every reconnect, since the outage is the one stretch in which a change
	# had no live connection to publish on.
	relay_env="$(relay_env)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$painted" = yes ]
	[ "$relay_env" = "OG_RELAY_GRAPHICS=" ]
}

@test "a control-connection drop into a different tmux server tears the mirror down" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 dds

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]

	# SIGKILL first: kill-server against the still-live control client would
	# itself be seen as a terminal %exit (measured on next-3.8), reaching
	# teardown without ever touching the identity check this test is named
	# for. Only killing the TRANSPORT produces the bare-EOF drop that the
	# reconnect loop retries — the identity check runs on that retry's dial.
	kill -9 "$old_transport"
	# Recreate immediately, minimizing the window in which the daemon's own
	# backoff (jittered, up to 500ms) could dial the OLD, still-live server
	# first — which would reattach, legitimately match, and leave the
	# identity-mismatch path unreached. A same-named session on a freshly
	# started server (a different tmux server pid, even though $0 is reused) is
	# exactly the case a session-id-only identity check would wave through.
	# The fresh server also renumbers panes from %0, so its pane ids collide
	# with the ones this mirror's registry still holds. That leak window
	# (attach to identity reply) is not assertable here — teardown kills the
	# mirror session milliseconds later — so
	# TestReattachDropsOutputFromAnUnverifiedConnection pins it instead.
	$SRC kill-server 2>/dev/null || true
	$SRC new-session -d -s rem -x 100 -y 30

	# Positive evidence of the mismatch — the daemon's own stderr line — not
	# "the mirror is gone" alone: if the race above went the other way the
	# mirror would end up gone anyway, via the harness's own kill-server, and
	# the test would be a silent false-green.
	mismatch=no
	for _ in $(seq 1 100); do
		grep -q "different tmux server" "$BATS_TEST_TMPDIR/dds.log" 2>/dev/null && {
			mismatch=yes
			break
		}
		sleep 0.1
	done

	wait "$daemon_pid" 2>/dev/null || true

	[ "$mismatch" = yes ]
	[ ! -e "$sock" ]
	[ ! -e "$sock.pid" ]
	run $DST has-session -t =host-sess
	[ "$status" -ne 0 ]
}

@test "a pane paused when the connection drops resumes and keeps repainting after reconnect" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 dpp

	# Default pause-after is 1s and needs no backlog to fire (a fresh attach
	# pauses on its own ~1s in, per the pre-existing "pause-after default"
	# test above) — give it time to actually pause this pane BEFORE the drop.
	# %pause is per-control-client state a fresh client will never see
	# %continue for, so reattach must resume() the sink before the reseed is
	# enqueued into it, or the pane freezes for the life of the daemon — worse
	# than teardown (#482).
	sleep 2

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]
	kill -9 "$old_transport"

	state="disconnected"
	for _ in $(seq 1 100); do
		state="$($DST show-options -v -t host-sess -q @bridge_state 2>/dev/null || true)"
		[ -z "$state" ] && break
		sleep 0.1
	done
	[ -z "$state" ]

	# First write after reconnect: proves the resumed sink repaints at all.
	$SRC send-keys -t rem 'echo PAUSED_REPAINT_A9K2' Enter
	painted_a=no
	for _ in $(seq 1 60); do
		mirror_contains 1 PAUSED_REPAINT_A9K2 && {
			painted_a=yes
			break
		}
		sleep 0.15
	done

	# A second, later write: proves it KEEPS repainting rather than having
	# been resumed just long enough for the reseed's own one-shot capture.
	$SRC send-keys -t rem 'echo PAUSED_REPAINT_B3M7' Enter
	painted_b=no
	for _ in $(seq 1 60); do
		mirror_contains 1 PAUSED_REPAINT_B3M7 && {
			painted_b=yes
			break
		}
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$painted_a" = yes ]
	[ "$painted_b" = yes ]
}

@test "a mirror resized during the outage converges the remote to the new size after reconnect" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 drz

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]
	kill -9 "$old_transport"

	wait_bridge_disconnected drz "$BATS_TEST_TMPDIR/drz.log"

	# Resize the LOCAL mirror while disconnected. watchResize keeps polling
	# through the outage and records the new size into the converger before
	# it can ever send it (the send fails closed with no live connection) —
	# so a converger merely carried across the reconnect, rather than reset,
	# would believe the remote already has this size and never resend it
	# (#482). resize-window sticks on the detached DST session (no attached
	# client to override it under window-size latest).
	$DST resize-window -t host-sess:1 -x 120 -y 40

	for _ in $(seq 1 100); do
		state="$($DST show-options -v -t host-sess -q @bridge_state 2>/dev/null || true)"
		[ -z "$state" ] && break
		sleep 0.1
	done
	[ -z "$state" ]

	# The right size, not 80 columns: an unset/never-resent converger would
	# leave the remote at its ORIGINAL 100x30, not tmux's control-client
	# default — this asserts the CONVERGED size actually landed.
	dims=""
	for _ in $(seq 1 60); do
		dims="$($SRC display-message -p -t rem -F '#{window_width}x#{window_height}' 2>/dev/null)"
		[ "$dims" = "120x40" ] && break
		sleep 0.1
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$dims" = "120x40" ]
}

# #482 × #487: a mirror's local window can die during the outage — the one
# stretch in which nothing can discover it, since the live path notices a dead
# local window only on a %layout-change pass that fails against it, and the
# stream those arrive on is down. The reattach repair owns that recovery.
#
# Two remote windows so the kill leaves the mirror session standing: emptying
# the registry is a teardown, which would pass a broken daemon for the wrong
# reason.
@test "a mirror window killed during the outage is retired and rebuilt by the reattach repair" {
	$SRC new-session -d -s rem -x 100 -y 30
	$SRC new-window -t 'rem:{end}' -a
	$SRC select-window -t rem:1
	$DST new-session -d -s host-sess -x 100 -y 30

	"$DAEMON" --test-local --src-socket m2src --dst-socket m2dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$BATS_TEST_TMPDIR/lwo.sock" \
		>"$BATS_TEST_TMPDIR/lwo.log" 2>&1 &
	daemon_pid=$!

	want_panes="$($SRC list-panes -s -t rem -F '#{pane_id}' | wc -l)"
	for _ in $(seq 1 "$(((BRIDGE_UP_BUDGET_SECS + want_panes * BRIDGE_UP_PER_PANE_SECS) * 10))"); do
		got_panes="$($DST list-panes -s -t host-sess -F '#{pane_current_command}' 2>/dev/null | grep -c "$RENDERER_PROBE")" || got_panes=0
		[ "$got_panes" -eq "$want_panes" ] && break
		sleep 0.1
	done
	[ "$got_panes" -eq "$want_panes" ] || m2_pane_gate_failed "$BATS_TEST_TMPDIR/lwo.log" "$got_panes" "$want_panes"
	[ "$got_panes" -eq "$want_panes" ]

	doomed="$($DST list-windows -t host-sess -F '#{window_id}' | tail -1)"
	[ -n "$doomed" ]

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]
	kill -9 "$old_transport"

	wait_bridge_disconnected lwo "$BATS_TEST_TMPDIR/lwo.log"

	# The kill lands while there is no stream to report it on.
	$DST kill-window -t "$doomed"
	[ "$($DST list-windows -t host-sess -F '#{window_id}' | wc -l)" -eq 1 ]

	# Both remote windows mirrored again, each replacement carrying
	# @bridge_win — the stamp only mirrorNewWindow writes, so its presence is
	# what says the rebuild went through reconcileWindows rather than some
	# half-built window left behind.
	ids=""
	rebuilt=no
	for _ in $(seq 1 120); do
		ids="$($DST list-windows -t host-sess -F '#{window_id} #{@bridge_win}' 2>/dev/null || true)"
		[ "$(printf '%s\n' "$ids" | grep -c ' 1$')" -eq 2 ] && {
			rebuilt=yes
			break
		}
		sleep 0.1
	done
	if [ "$rebuilt" != yes ]; then
		printf -- '--- DST windows ---\n%s\n--- daemon log ---\n' "$ids" >&3
		tail -60 "$BATS_TEST_TMPDIR/lwo.log" >&3 2>/dev/null || true
	fi

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$rebuilt" = yes ]
	# A rebuild, not the corpse: tmux never reuses a live id, so the killed one
	# reappearing would mean the entry was never retired.
	if printf '%s\n' "$ids" | grep -q "^$doomed "; then
		echo "the killed window $doomed is still in the mirror: $ids" >&2
		false
	fi
}

# #482 × #491: the label poll rides a coarse ticker, a remote window option
# changing nothing the control stream reports. That ticker is session-lifetime,
# and a reconnect must neither lose it nor build a second one.
#
# Two halves, and the second is the one that needs it: a label set DURING the
# outage lands on the repair pass's first loop iteration, while one set after
# it, on an otherwise silent mirror, can only arrive on a tick.
@test "window labels keep tracking the remote across a reconnect" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 lbr

	old_transport="$(transport_child)"
	[ -n "$old_transport" ]
	kill -9 "$old_transport"

	wait_bridge_disconnected lbr "$BATS_TEST_TMPDIR/lbr.log"

	# Set while there is no stream to carry it.
	$SRC set -w -t rem:1 @crew_name nova

	crew=""
	for _ in $(seq 1 200); do
		crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
		[ "$crew" = nova ] && break
		sleep 0.1
	done
	[ "$crew" = nova ]

	# Now the ticker's half. Nothing below writes to a pane, so the control
	# stream stays silent and only a tick can bring the loop back around.
	$SRC set -w -t rem:1 @crew_name pine

	for _ in $(seq 1 300); do
		crew="$($DST show-options -w -t host-sess:1 -qv @bridge_crew_name 2>/dev/null || true)"
		[ "$crew" = pine ] && break
		sleep 0.1
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$crew" = pine ]
}

# The zoom test above asserts the flag and the pane dims agree; this one
# asserts the mirror's SCREEN does, which flags and dims can converge without
# (#511).
@test "the mirror repaints zoomed content at the zoomed geometry" {
	$SRC new-session -d -s rem -x 150 -y 40
	# The alternate screen (\033[?1049h) is what makes a wrong-geometry paint
	# permanent: it has no history and is never reflowed, where the main screen
	# pulls scrolled lines back out of history as a pane grows and rejoins
	# wrapped rows as it widens (both measured), healing the very damage this
	# test looks for. 40 lines is more than the ~20-row unzoomed pane holds.
	# sleep 300 keeps the pane alive — SRC_CONF sets no remain-on-exit, so a
	# command that finished would take the pane, and the 2-pane mirror, with it;
	# bounded, since BSD sleep on the darwin leg rejects `sleep infinity`. sh -c
	# because the pane's shell is whatever default-shell resolves to.
	$SRC split-window -v -t rem \
		"sh -c 'printf \"\\033[?1049h\"; i=1; while [ \$i -le 40 ]; do printf \"ZOOMFILL_%02d\\n\" \$i; i=\$((i+1)); done; sleep 300'"
	# bridge_up send-keys its startup marker to the active pane, which
	# split-window just made the content pane — and the marker would land in the
	# very alternate grid this test byte-compares. Aim it at the shell instead.
	$SRC select-pane -t rem:1.1

	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 zct

	# split-window -v puts the new pane at layout index 1, so the content pane
	# is remote rem:1.2 / local host-sess:1.1 (both servers run
	# pane-base-index 1; the daemon stamps 0 on mirror windows). It has to be
	# this pane: index 0 is the shell, on the main screen, which self-heals.
	pane="$(remote_pane_of 1)"
	[ -n "$pane" ]

	# Gate on the fill crossing, separately from the zoom below: a fill that
	# never arrived is a different bug, and worth failing as one.
	filled=no
	for _ in $(seq 1 60); do
		if $DST capture-pane -p -t host-sess:1.1 2>/dev/null | grep -q ZOOMFILL_40; then
			filled=yes
			break
		fi
		sleep 0.15
	done
	[ "$filled" = yes ]

	# Match before zooming, so a red compare below can only be the zoom.
	# $(...) strips trailing blank rows from both sides, making this an equality
	# over content rows — don't "fix" that with -J or a sentinel line.
	src_screen="$($SRC capture-pane -p -t rem:1.2)"
	dst_screen="$($DST capture-pane -p -t host-sess:1.1)"
	[ "$dst_screen" = "$src_screen" ]

	run "$CTL" --sock "$sock" zoom "$pane"
	[ "$status" -eq 0 ]

	# Both captures are re-read every iteration, not once after the dims
	# settle: content is the observable that lags here, since a zoomed pane's
	# capture carries one line per row including trailing blanks (measured: 23
	# lines for a 23-row pane holding 10), and painting those overflow rows
	# into a pane still at the old size scrolls the content off for good.
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		src_screen="$($SRC capture-pane -p -t rem:1.2)"
		dst_screen="$($DST capture-pane -p -t host-sess:1.1)"
		[ "$src_z" = 1 ] && [ "$dst_z" = 1 ] && [ "$src_dims" = "$dst_dims" ] && [ "$dst_screen" = "$src_screen" ] && break
		sleep 0.15
	done

	# The screens are already captured above: teardown takes the DST session
	# down with the daemon, so they cannot be read after this.
	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_z" = 1 ]
	[ "$dst_z" = 1 ]
	[ "$src_dims" = "$dst_dims" ]
	[ "$dst_screen" = "$src_screen" ]
}

# argv survives a bare respawn-pane; pane -e does not. spawnRenderer now
# passes sock + remote pane id as renderer arguments, so Respawn is a
# reconnect, not a repair event — env no longer dies at dial. remain-on-exit
# still stamps so a genuine crash cannot take the session (#547).
@test "a respawned mirror pane leaves the session standing and the mirror recovers" {
	# The real host's value, not this suite's: DST_CONF turns remain-on-exit ON
	# globally so panes outlive daemon exit for the other cases' assertions,
	# which is exactly what would mask the window stamp under test here.
	printf 'set -g base-index 1\nset -g pane-base-index 1\nset -g status on\nset -g pane-border-status top\nset -g remain-on-exit off\nset -g renumber-windows on\n' >"$DST_CONF"

	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 respawn

	# The stamp, against a global that says otherwise — asserting the option
	# rather than only its effect, since the effect below is also what a lucky
	# race would produce.
	[ "$($DST show-options -gv remain-on-exit)" = off ]
	win="$($DST list-windows -t host-sess -F '#{window_id}' | head -1)"
	[ "$($DST show-options -wv -t "$win" remain-on-exit)" = on ]

	$DST respawn-pane -k -t host-sess:1.0

	# The regression, checked before the sweep can repair anything: the gesture
	# used to take the server with it, so this ran against nothing at all.
	$DST has-session -t '=host-sess'
	[ "$($DST list-windows -t host-sess -F '#{window_id}' | wc -l)" -eq 1 ]

	# Recovery is the same window: argv reconnects the renderer in place.
	# A heal rebuild would mint a new window id.
	marker="RESPAWNRECONNECT_$$"
	healed=no
	deadline=$((SECONDS + BRIDGE_UP_BUDGET_SECS))
	while [ "$SECONDS" -lt "$deadline" ]; do
		$SRC send-keys -t rem "printf '$marker\\n'" Enter
		for _ in $(seq 1 10); do
			if mirror_contains 1 "$marker"; then
				healed=yes
				break 2
			fi
			sleep 0.1
		done
	done
	if [ "$healed" != yes ]; then
		printf -- '--- DST panes ---\n%s\n--- daemon log ---\n' \
			"$($DST list-panes -s -t host-sess -F '#{window_id}|#{pane_id}|#{pane_dead}|#{@bridge_pane}' 2>&1)" >&3
		tail -60 "$BATS_TEST_TMPDIR/respawn.log" >&3 2>/dev/null || true
	fi

	win_after="$($DST list-windows -t host-sess -F '#{window_id}' | head -1)"
	dead="$($DST display-message -p -t host-sess:1.0 '#{pane_dead}')"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$healed" = yes ]
	[ "$win_after" = "$win" ]
	[ "$dead" = 0 ]
}

# Crash net: SIGKILL the renderer process (not respawn). remain-on-exit holds
# a corpse; healDeadRenderers then rebuilds a live mirror.
@test "killing a renderer process leaves the session standing and heal restores the mirror" {
	printf 'set -g base-index 1\nset -g pane-base-index 1\nset -g status on\nset -g pane-border-status top\nset -g remain-on-exit off\nset -g renumber-windows on\n' >"$DST_CONF"

	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 killrnd

	[ "$($DST show-options -gv remain-on-exit)" = off ]
	win="$($DST list-windows -t host-sess -F '#{window_id}' | head -1)"
	[ "$($DST show-options -wv -t "$win" remain-on-exit)" = on ]

	pid="$($DST display-message -p -t host-sess:1.0 '#{pane_pid}')"
	kill -KILL "$pid"

	$DST has-session -t '=host-sess'

	saw_corpse=no
	for _ in $(seq 1 50); do
		if [ "$($DST display-message -p -t host-sess:1.0 '#{pane_dead}' 2>/dev/null)" = 1 ]; then
			saw_corpse=yes
			break
		fi
		sleep 0.1
	done
	if [ "$saw_corpse" != yes ]; then
		printf -- '--- no corpse ---\n%s\n--- daemon log ---\n' \
			"$($DST list-panes -s -t host-sess -F '#{window_id}|#{pane_id}|#{pane_dead}|#{@bridge_pane}|#{pane_pid}' 2>&1)" >&3
		tail -60 "$BATS_TEST_TMPDIR/killrnd.log" >&3 2>/dev/null || true
	fi

	# mainLoopTickInterval is 5s; heal runs on that sweep. 20s covers a
	# contended tick plus resetWindow's spawn/hello/seed.
	marker="KILLHEAL_$$"
	healed=no
	deadline=$((SECONDS + 20))
	while [ "$SECONDS" -lt "$deadline" ]; do
		$SRC send-keys -t rem "printf '$marker\\n'" Enter
		for _ in $(seq 1 10); do
			if mirror_contains 1 "$marker"; then
				healed=yes
				break 2
			fi
			sleep 0.1
		done
	done
	if [ "$healed" != yes ]; then
		printf -- '--- DST panes ---\n%s\n--- daemon log ---\n' \
			"$($DST list-panes -s -t host-sess -F '#{window_id}|#{pane_id}|#{pane_dead}|#{@bridge_pane}' 2>&1)" >&3
		tail -60 "$BATS_TEST_TMPDIR/killrnd.log" >&3 2>/dev/null || true
	fi

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$saw_corpse" = yes ]
	[ "$healed" = yes ]
}

# === Bridge graphics identity: control-client replacement (#574, H2) ===
#
# Half 1 (above) proves the capability follows the viewer with no re-dial.
# These prove the other half: the daemon's OWN control client to the remote
# is rebuilt (dial, verify, swap) so the remote genuinely sees a NEW viewer's
# termname — not just a local env var — and that this only ever happens on
# the carousel gesture, never on a bare session switch.

# control_termname reads the daemon's own SRC client — the only one with
# client_control_mode=1 on this session — which is what TERM= on its dial argv
# (--test-local: localCtlCmdEnv; ssh: sshControlArgs) actually landed on. This
# is the "the remote genuinely sees the viewer's identity" proof acceptance 1
# asks for, as opposed to a local-only assertion on OG_RELAY_GRAPHICS.
control_termname() {
	$SRC list-clients -t rem -F '#{client_control_mode}|#{client_termname}' 2>/dev/null |
		awk -F'|' '$1 == "1" { print $2; exit }'
}

# attach_pty_client starts (or replaces) the single OBS pty client viewing
# host-sess, carrying $1 as TERM (and, if given, $2 as -T terminal-features).
# OBS is killed first: list-clients assertions below need exactly one
# non-control client on host-sess, and a stale pty from an earlier phase of
# the same test would make that two.
attach_pty_client() {
	local term="$1" feats="${2:-}"
	tmux -L m2obs kill-server 2>/dev/null || true
	# kill-server returns before the socket is actually torn down, and starting
	# a new server on that same socket inside the window fails outright with
	# "server exited unexpectedly" — measured, and it looks like a bad flag
	# rather than a race, so wait the old server out rather than sleeping a
	# guessed interval.
	for _ in $(seq 1 40); do
		tmux -L m2obs list-sessions >/dev/null 2>&1 || break
		sleep 0.1
	done
	if [ -n "$feats" ]; then
		tmux -L m2obs new-session -d -s obs -x 100 -y 30 "env TERM=$term $DST -T $feats attach -t host-sess"
	else
		tmux -L m2obs new-session -d -s obs -x 100 -y 30 "env TERM=$term $DST attach -t host-sess"
	fi
	# Poll the requested IDENTITY, not the client count: replacing a viewer
	# leaves the count already satisfied by the client on its way out, so a
	# count poll returns before the new termname is the one attached — and
	# every assertion downstream is then racing the handover.
	for _ in $(seq 1 40); do
		[ "$($DST list-clients -t host-sess -F '#{client_termname}' 2>/dev/null | grep -c "^$term\$")" -ge 1 ] &&
			[ "$($DST list-clients -t host-sess 2>/dev/null | grep -c '^')" -eq 1 ] && return 0
		sleep 0.1
	done
	return 1
}

# Acceptance 1 end-to-end, plus its latency discriminator. attach_pty_client
# runs BEFORE bridge_up so seedView (cmd/daemon/main.go) resolves the attached
# client and the daemon's very first dial already carries that termname — no
# gesture needed for the FIRST assertion, which is the "remote genuinely sees
# the viewer's identity" half of acceptance 1.
#
# The pair is tmux-256color -> foot, NOT the xterm-kitty -> foot the feature is
# actually about, and that is deliberate: tmux REFUSES to start a client for a
# TERM it cannot resolve in terminfo (measured: exit 1, no client attached), and
# xterm-kitty's entry ships with kitty rather than ncurses, so it is absent in
# the nix check sandbox — an xterm-kitty viewer fails there while passing in a
# dev shell. tmux-256color is this config's own default-terminal, so its entry
# must exist wherever tmux runs at all, and foot's is in ncurses. Nothing here
# depends on either NAME: the kitty/ghostty prefix test lives in aeye's
# chooseRelayBackend, out of this repo, while what this test proves is that the
# advertised termname follows the viewer and reaches the remote.
@test "the carousel gesture replaces the control client to match a new viewer, within 2s" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	attach_pty_client tmux-256color
	bridge_up 1 gxr1

	[ "$(control_termname)" = tmux-256color ]

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]

	# Switch the viewer to a different terminal — the SAME gesture the human
	# makes by attaching from a different machine/terminal.
	attach_pty_client foot

	run "$CTL" --sock "$sock" carousel "$pane"
	[ "$status" -ne 0 ]
	[[ $output == *"press again"* ]]

	# The timing discriminator (acceptance 1): without viewReplacer's dedicated
	# wake-up channel (step 8), the replacement raised by the press above would
	# only be picked up on runConn's next mainLoopTickInterval tick (5s) — so
	# landing within 2s here is real evidence the channel fired rather than the
	# loop falling back to its poll. Budget enforced by iteration count
	# (20 x 0.1s), not wall-clock reads, since $SECONDS is only 1s-granular.
	replaced=no
	for _ in $(seq 1 20); do
		[ "$(control_termname)" = foot ] && {
			replaced=yes
			break
		}
		sleep 0.1
	done
	if [ "$replaced" != yes ]; then
		tail -60 "$BATS_TEST_TMPDIR/gxr1.log" >&3 2>/dev/null || true
	fi

	# The replacement has landed, so the same gesture now submits quietly.
	run "$CTL" --sock "$sock" carousel "$pane"
	carousel_status="$status"

	# The mirror is still alive on the ORIGINAL pane: the replacement did not
	# cost the mirror, and the carousel's own new pane (or its no-binary
	# fallback message pane) is a second pane, not a teardown of the first.
	marker="GXR1_LIVE_$$"
	$SRC send-keys -t rem "printf '$marker\\n'" Enter
	live=no
	for _ in $(seq 1 60); do
		$DST capture-pane -p -t host-sess:1.0 2>/dev/null | grep -q "$marker" && {
			live=yes
			break
		}
		sleep 0.15
	done

	final_termname="$(control_termname)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	tmux -L m2obs kill-server 2>/dev/null || true

	[ "$replaced" = yes ]
	[ "$carousel_status" -eq 0 ]
	[ "$final_termname" = foot ]
	[ "$live" = yes ]
}

# Acceptance 6: ordinary session switching on the SAME terminal must never
# raise a replacement. transport_child's PID is the direct witness — a
# replacement always spawns a fresh attach-session child before closing the
# old one, so an unchanged PID across the gesture is proof none was raised,
# stronger than only checking client_termname (which a same-terminal switch
# also never changes, but for a less interesting reason).
@test "an ordinary session switch on the same terminal never replaces the control client" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	$DST new-session -d -s other -x 100 -y 30

	attach_pty_client foot
	bridge_up 1 gxr6

	[ "$(control_termname)" = foot ]
	old_transport="$(transport_child)"
	[ -n "$old_transport" ]

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]

	# The ordinary switch: the SAME client (same terminal, same TERM) moves to
	# a different local session and back. client-session-changed (one of the
	# hooks step 5 added) fires on this, so it exercises the exact path a
	# genuine terminal change uses — with no identity change behind it.
	client_name="$($DST list-clients -t host-sess -F '#{client_name}')"
	[ -n "$client_name" ]
	$DST switch-client -c "$client_name" -t other
	$DST switch-client -c "$client_name" -t host-sess

	# Give the watcher's 1s-ticked resolve a moment to run and settle, same
	# budget style as the resize-converge tests use.
	for _ in $(seq 1 20); do
		sleep 0.1
	done

	run "$CTL" --sock "$sock" carousel "$pane"
	carousel_status="$status"
	carousel_output="$output"

	new_transport="$(transport_child)"
	termname="$(control_termname)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	tmux -L m2obs kill-server 2>/dev/null || true

	# No replacement: same transport child, same termname throughout...
	[ "$new_transport" = "$old_transport" ]
	[ "$termname" = foot ]
	# ...and the gesture was never nacked either (R8's other half).
	[ "$carousel_status" -eq 0 ]
	[[ $carousel_output != *"press again"* ]]
}

# Acceptance 7, offline: a replacement whose DIAL fails must leave the mirror
# live and the old identity advertised — no teardown, no killed session.
# Under --test-local the dial is a fixed `tmux -L m2src -C attach-session`, so
# the way to fail it deterministically is to make that exact command unable to
# connect while the EXISTING control client — already connected, its fd long
# past any path lookup — keeps working: rename the m2src socket file away.
#
# A plain unlink would also break every OTHER $SRC command bats itself would
# issue from here on: has-session, display-message and send-keys are all
# brand-new client connections too, not a persistent one, so there would be
# nothing left to assert against except $DST. Renaming instead of removing
# keeps that door open — moved back before the end, the path resolves again
# and normal $SRC introspection (and teardown's own kill-server) works.
#
# Liveness while the socket is unreachable is proved without any $SRC command
# after the move: a remote command scheduled with a `sleep` BEFORE the move
# produces its output AFTER it, and that output still crosses the daemon's
# already-open connection to reach the mirror pane — the same "sleep inside
# one remote command" trick send_straddled_sixel uses to straddle a Feed
# boundary, applied here to straddle the socket's unavailability instead.
@test "a replacement whose dial fails leaves the mirror live and the old identity advertised" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30

	# Attached before the daemon starts, so seedView resolves it and the very
	# first dial already carries tmux-256color — Advertised starts there.
	attach_pty_client tmux-256color
	bridge_up 1 gxr7

	pane="$(remote_pane_of 0)"
	[ -n "$pane" ]
	old_transport="$(transport_child)"
	[ -n "$old_transport" ]

	# Switch the viewer: Desired becomes foot, Advertised stays tmux-256color —
	# exactly what raises a replacement on the next carousel press.
	attach_pty_client foot

	SOCK="$TMUX_TMPDIR/tmux-$(id -u)/m2src"
	[ -S "$SOCK" ]

	# Scheduled while $SRC can still reach the remote by path; its output
	# lands on the daemon's still-open connection well after the move below.
	marker="GXR7_LIVE_$$"
	$SRC send-keys -t rem "sh -c 'sleep 2; printf \"$marker\\n\"'" Enter

	mv "$SOCK" "$SOCK.bak"

	run "$CTL" --sock "$sock" carousel "$pane"
	press1_status="$status"
	press1_output="$output"

	sleep 0.3
	# Repeatable, not a one-shot latch: replacer.done() clears inFlight on
	# every outcome (replacer.go), so a second press while Advertised is
	# still stale raises — and fails — again rather than getting stuck
	# "in flight" forever.
	run "$CTL" --sock "$sock" carousel "$pane"
	press2_status="$status"
	press2_output="$output"

	live=no
	for _ in $(seq 1 40); do
		$DST capture-pane -p -t host-sess:1.0 2>/dev/null | grep -q "$marker" && {
			live=yes
			break
		}
		sleep 0.15
	done

	new_transport="$(transport_child)"

	# Restored before any further $SRC command — including teardown's own
	# kill-server — needs the path back.
	mv "$SOCK.bak" "$SOCK"
	final_termname="$(control_termname)"
	dst_alive_status=0
	$DST has-session -t '=host-sess' || dst_alive_status=$?

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true
	tmux -L m2obs kill-server 2>/dev/null || true

	[ "$press1_status" -ne 0 ]
	[[ $press1_output == *"press again"* ]]
	[ "$press2_status" -ne 0 ]
	[[ $press2_output == *"press again"* ]]
	[ "$live" = yes ]
	# The daemon's own control client to the remote never died or got
	# replaced...
	[ "$new_transport" = "$old_transport" ]
	# ...and the identity it advertises is still what it always was.
	[ "$final_termname" = tmux-256color ]
	# No teardown: the mirror session stood throughout.
	[ "$dst_alive_status" -eq 0 ]
}
# === #570: layout-change notification carries the answer, not just a poke ===

# A select-layout of the window's own current layout emits two identical
# %layout-change lines (layout-custom.c:289, cmd-select-layout.c:142); the
# notification carries #{window_layout} and the zoom flag, so neither may cost
# the remote a display-message. A -h window, so the positive control's -L
# resize below moves a cell (-L on a -v split is a tmux no-op).
@test "a no-op %layout-change costs the remote nothing" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 noop

	$SRC set -g @dm 0
	# The hook fires for the daemon's own reads too — that is what is counted.
	# Its set runs in the control client's queue as flag-0 %begin/%end blocks,
	# which claimSeq treats as inert (#276).
	$SRC set-hook -g after-display-message "set -gF @dm '#{e|+:#{@dm},1}'"

	# Quiesce: wait until the counter holds still across 10 samples 0.15s
	# apart, so the seeds' own cursor reads (which tick it too) have drained
	# before the baseline below is taken.
	prev="$($SRC show-options -gv @dm)"
	stable=0
	for _ in $(seq 1 100); do
		sleep 0.15
		cur="$($SRC show-options -gv @dm)"
		if [ "$cur" = "$prev" ]; then
			stable=$((stable + 1))
		else
			stable=0
			prev="$cur"
		fi
		[ "$stable" -ge 10 ] && break
	done
	[ "$stable" -ge 10 ]

	layout="$($SRC list-windows -t rem -F '#{window_layout}')"
	before="$($SRC show-options -gv @dm)"
	$SRC select-layout -t rem "$layout"

	# Nothing on the daemon's 5s maintenance tick issues a remote
	# display-message: the sweep and both shipper backstops read
	# list-windows/list-panes, and reseedDropped/reseedReshaped act only on
	# pending work, which the quiesce drained.
	sleep 1.5
	after="$($SRC show-options -gv @dm)"
	src_dims="$(sorted_dims "$SRC" rem)"
	dst_dims="$(sorted_dims "$DST" host-sess:1)"
	[ "$after" = "$before" ]
	[ "$src_dims" = "$dst_dims" ]

	# Positive control: a real geometry change must still converge the mirror
	# and still cost at least one display-message (the seeds' cursor reads and
	# the trailing readLayout), so a dead daemon cannot pass the flat counter.
	pre_dims="$(sorted_dims "$SRC" rem)"
	$SRC resize-pane -L -t rem:1.1 3
	for _ in $(seq 1 60); do
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		dm="$($SRC show-options -gv @dm)"
		[ "$src_dims" != "$pre_dims" ] && [ "$dst_dims" = "$src_dims" ] && [ "$dm" -gt "$after" ] && break
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_dims" != "$pre_dims" ]
	[ "$dst_dims" = "$src_dims" ]
	[ "$dm" -gt "$after" ]
}

# Several remote resizes in immediate succession, no pane-count change and no
# zoom: the mirror's dims must converge and its content repaint at the final
# geometry. Convergence only — which line of the burst was stale is not
# observable from outside.
@test "a burst of remote geometry changes converges the mirror" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 gburst

	# Paint a marker into rem:1.1, as the #231 test does, so a later content
	# compare against host-sess:1.0 is meaningful.
	marker="GEOBURST_$$"
	painted=no
	for _ in $(seq 1 20); do
		$SRC send-keys -t rem:1.1 "printf '$marker\\n'" Enter
		for _ in $(seq 1 10); do
			out="$($DST capture-pane -p -t host-sess:1.0 2>/dev/null)"
			[[ $out == *$marker* ]] && {
				painted=yes
				break 2
			}
			sleep 0.1
		done
	done
	[ "$painted" = yes ]

	for _ in $(seq 1 5); do
		$SRC resize-pane -L -t rem:1.1 3
	done

	# Content converges strictly after dims — select-layout precedes the
	# seeds in the pass — so poll on the content equality itself, with dims
	# as the pre-gate.
	for _ in $(seq 1 60); do
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		if [ "$src_dims" = "$dst_dims" ]; then
			src_screen="$($SRC capture-pane -p -t rem:1.1)"
			dst_screen="$($DST capture-pane -p -t host-sess:1.0)"
			[ "$src_screen" = "$dst_screen" ] && break
		fi
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_dims" = "$dst_dims" ]
	[ "$src_screen" = "$dst_screen" ]
}

# Split immediately followed by kill-pane on the remote, once unzoomed and once
# zoomed: DST's pane dims and zoom flag must converge to SRC's.
@test "a split immediately killed converges the mirror, zoomed or not" {
	$SRC new-session -d -s rem -x 150 -y 40
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 150 -y 40
	bridge_up 2 splitkill # a 2-pane base: window_zoom refuses a 1-pane window

	new="$($SRC split-window -v -t rem -P -F '#{pane_id}')"
	$SRC kill-pane -t "$new"
	for _ in $(seq 1 60); do
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		[ "$src_dims" = "$dst_dims" ] && break
		sleep 0.15
	done
	[ "$src_dims" = "$dst_dims" ]

	run "$CTL" --sock "$sock" zoom "$(remote_pane_of 0)"
	[ "$status" -eq 0 ]
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		[ "$src_z" = 1 ] && [ "$dst_z" = 1 ] && break
		sleep 0.15
	done
	[ "$src_z" = 1 ]
	[ "$dst_z" = 1 ]

	new="$($SRC split-window -v -t rem -P -F '#{pane_id}')"
	$SRC kill-pane -t "$new"
	for _ in $(seq 1 60); do
		src_z="$($SRC display-message -p -t rem '#{window_zoomed_flag}')"
		dst_z="$($DST display-message -p -t host-sess:1 '#{window_zoomed_flag}')"
		src_dims="$(sorted_dims "$SRC" rem)"
		dst_dims="$(sorted_dims "$DST" host-sess:1)"
		[ "$src_z" = "$dst_z" ] && [ "$src_dims" = "$dst_dims" ] && break
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ "$src_z" = "$dst_z" ]
	[ "$src_dims" = "$dst_dims" ]
}

# #645: the pin bump (#652) made `#{window_layout}` over a control client
# without `refresh-client -f new-layouts` report v1 tiled-only — the remote's
# float vanished from every read the daemon does. Fails on main post-#652:
# no floating pane is ever created on DST.
@test "a remote float is mirrored as a local float" {
	$SRC new-session -d -s rem -x 100 -y 30
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 1 float1

	# Created after the mirror is already live, so this exercises
	# reconcileFloats' add path, not the initial setup. The daemon's local
	# float is sized from the remote's inner cell (list-panes' pane_width x
	# pane_height), not these outer -x/-y/-X/-Y flags — compare inner cells.
	$SRC new-pane -d -t rem -x 40 -y 10 -X 5 -Y 3

	src_float="" dst_float=""
	for _ in $(seq 1 60); do
		src_float="$($SRC list-panes -t rem -f '#{pane_floating_flag}' -F '#{pane_width}x#{pane_height}')"
		dst_float="$($DST list-panes -t host-sess:1 -f '#{pane_floating_flag}' -F '#{pane_width}x#{pane_height}')"
		[ -n "$dst_float" ] && [ "$src_float" = "$dst_float" ] && break
		sleep 0.15
	done

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ -n "$src_float" ]
	[ "$src_float" = "$dst_float" ]
}

# #535: a float that is the USER's, not the daemon's, opened directly on
# DST inside the mirror window. applyLayout's drop-mirrored-floats step and
# the local-cells short-circuit used to exist only to route around the local
# `select-layout` refusing a v1 string behind any float; now that the v1
# tiled-only string is itself the float-tolerant form on the pinned local
# server, that workaround is gone, so a reshape behind this float applies
# straight through and the float is never touched.
@test "a reshape behind a local user float applies (#535)" {
	$SRC new-session -d -s rem -x 100 -y 30
	$SRC split-window -h -t rem
	$DST new-session -d -s host-sess -x 100 -y 30
	bridge_up 2 userfloat

	# The user's float, created after bridge_up's gate already proves both
	# renderer panes are live and wired.
	user_float="$($DST new-pane -d -P -F '#{pane_id}' -t host-sess:1 -x 20 -y 5)"
	[ -n "$user_float" ]

	$SRC resize-pane -t rem.1 -x 30
	for _ in $(seq 1 60); do
		src_dims="$(sorted_tiled_dims "$SRC" rem)"
		dst_dims="$(sorted_tiled_dims "$DST" host-sess:1)"
		[ "$src_dims" = "$dst_dims" ] && break
		sleep 0.15
	done

	# Still there, at its original pane id — reconcile reshaped the tiled
	# panes without killing the user's float (the drop-and-readd workaround
	# no longer runs).
	still_there="$($DST list-panes -t host-sess:1 -F '#{pane_id}' | grep -Fxc "$user_float" || true)"

	kill "$daemon_pid" 2>/dev/null || true
	wait "$daemon_pid" 2>/dev/null || true

	[ -n "$src_dims" ]
	[ "$src_dims" = "$dst_dims" ]
	[ "$still_there" -eq 1 ]
}
