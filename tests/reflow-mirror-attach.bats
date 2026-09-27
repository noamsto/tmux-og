#!/usr/bin/env bats
# #820: entering a remote-bridge mirror must leave the window list reflowed —
# the state right after a switch/attach must equal the state a forced reflow
# produces. Regression for the session-scoped daemon hooks that shadowed the
# config's global reflow hooks (a session hook array replaces the session's
# view of the global one, #647), so client-session-changed/client-resized never
# reached tmux-reflow-windows inside a mirror.
#
# Runs the REAL conf: the wrapped tmux on its own -L socket (two more servers
# stand in for the remote and as a pty host), with the bridge daemon's
# --test-local seam. Run through nix (`nix build
# .#checks.<system>.reflow-mirror-attach-tests`, which `nix flake check`
# covers) so TMUX_BIN/DAEMON/RENDERER/REFLOW are the built store paths; the
# local fallbacks build the picker and scrape the wrapper's baked conf.
bats_require_minimum_version 1.5.0

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built tmux-og wrapper}"
	# A short, fixed /tmp dir: tmux -L resolves to
	# "$TMUX_TMPDIR/tmux-<uid>/<name>", and $BATS_TEST_TMPDIR pushes that past
	# the 108-char unix socket limit ("File name too long").
	export TMUX_TMPDIR="/tmp/og820-$$-${BATS_TEST_NUMBER}"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export HOME="$TMUX_TMPDIR/home"
	mkdir -p "$HOME"
	# Mandatory: the wrapped conf's -B monitor hooks reach functions that
	# delete files under these dirs, whose defaults are the developer's real
	# /tmp trees.
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$TMUX_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$TMUX_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$TMUX_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR" "$OG_ENRICH_CACHE_DIR" "$OG_AGENT_USAGE_DIR" "$OG_ENRICH_LOCK_DIR"

	# The daemon execs a bare `tmux` for its local server; it must be the
	# wrapper (the real conf), not whatever tmux the harness PATH carries.
	tmux_dir="$(dirname "$TMUX_BIN")"
	export PATH="$tmux_dir:$PATH"

	if [[ -z ${DAEMON:-} ]]; then
		DAEMON="$BATS_TEST_TMPDIR/daemon"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$DAEMON" ./remotebridge/cmd/daemon)
	fi
	if [[ -z ${RENDERER:-} ]]; then
		RENDERER="$BATS_TEST_TMPDIR/renderer"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$RENDERER" ./remotebridge/cmd/renderer)
	fi
	if [[ -z ${REFLOW:-} ]]; then
		# The wrapper's baked conf names the build's tmux-reflow-windows store
		# path in its hooks; scrape it the way tests/test-display.sh scrapes
		# the conf.
		conf="$(grep -o -- '-f /nix/store/[a-z0-9]*-tmux[.]conf' "$TMUX_BIN" 2>/dev/null | head -1 | cut -d' ' -f2)"
		REFLOW="$(grep -o '/nix/store/[^ "]*tmux-reflow-windows/bin/tmux-reflow-windows' "$conf" 2>/dev/null | head -1)"
		if [[ -z $REFLOW ]]; then
			echo "REFLOW unset and not scrapable from $TMUX_BIN (a stale ./result symlink?); pass REFLOW" >&2
			return 1
		fi
	fi

	SRC="$TMUX_BIN -L og820src"
	DST="$TMUX_BIN -L og820dst"
	OBS="$TMUX_BIN -L og820obs"
	DAEMON_PID=""

	$SRC kill-server 2>/dev/null || true
	$DST kill-server 2>/dev/null || true
	$OBS kill-server 2>/dev/null || true
}

teardown() {
	if [[ -n $DAEMON_PID ]]; then
		kill "$DAEMON_PID" 2>/dev/null || true
		wait "$DAEMON_PID" 2>/dev/null || true
	fi
	$OBS kill-server 2>/dev/null || true
	$DST kill-server 2>/dev/null || true
	$SRC kill-server 2>/dev/null || true
	rm -rf "$TMUX_TMPDIR"
}

# poll <seconds> <command...> — retries until the command succeeds.
poll() {
	local deadline=$((SECONDS + $1))
	shift
	while [ "$SECONDS" -lt "$deadline" ]; do
		"$@" && return 0
		sleep 0.2
	done
	return 1
}

client_on() { # client_on <session> — 0 when a client is attached to it
	[ -n "$($DST list-clients -t "$1" -F '#{client_name}' 2>/dev/null)" ]
}

reflow_key_set() { # reflow_key_set <session>
	[ -n "$($DST display-message -t "$1" -p '#{@reflow_key}' 2>/dev/null)" ]
}

# capture_state <session> — every reflow-derived value the window list renders
# from: the session's layout key/row structure and each window's stamped label.
capture_state() {
	local sess=$1
	$DST display-message -t "$sess" -p 'key=#{@reflow_key} meta=#{@window_split},#{@window_split2},#{@window_split3},#{@window_per},#{@labels_mode},#{status}'
	$DST list-windows -t "$sess" -F '#{window_index}|#{@window_label_short}|#{@window_label_disp}|#{@window_label_id}'
	$DST show-options -t "$sess" -v 'status-format[1]' 2>/dev/null || true
}

# equals_forced_pass <session>: the state captured now must equal the state a
# forced reflow produces. @reflow_key is cleared first so the forced pass is
# provably the one that re-stamped it.
equals_forced_pass() {
	local sess=$1 before after win_count client_width
	win_count="$($DST display-message -t "$sess" -p '#{session_windows}')"
	client_width="$($DST display-message -t "$sess" -p '#{client_width}')"
	before="$(capture_state "$sess")"

	$DST set -u -t "$sess" @reflow_key

	$DST run-shell -b "$REFLOW --force $sess"
	if ! poll 10 reflow_key_set "$sess"; then
		echo "forced reflow never stamped @reflow_key (windows=$win_count width=$client_width)" >&3
		echo "--- state before: $before" >&3
		echo "--- daemon log:" >&3
		sed -n '1,40p' "$TMUX_TMPDIR/daemon.log" >&3 2>&1 || true
		return 1
	fi

	after="$(capture_state "$sess")"
	if [ "$before" != "$after" ]; then
		echo "state after the client entered the mirror differs from a forced reflow:" >&3
		diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&3 || true
		return 1
	fi
}

mirror_windows_ready() {
	[ "$($DST list-windows -t host-sess -F '#{window_id}' 2>/dev/null | wc -l)" -eq 4 ]
}

label_shipped() {
	[ -n "$($DST display-message -t host-sess:1 -p '#{@bridge_label_id}' 2>/dev/null)" ]
}

# bridge_up — a remote with four windows, a mirror session born detached (no
# client, so every reflow so far skipped on an empty width, #235), the daemon
# mirroring it, and the shipped label state settled.
bridge_up() {
	$SRC new-session -d -s rem -x 120 -y 40
	for _ in 1 2 3; do $SRC new-window -d -a -t rem; done
	for i in 1 2 3 4; do $SRC set -w -t "rem:$i" @branch "feat/w$i"; done
	$SRC set -w -t rem:1 @issue_id '#820'

	$DST new-session -d -s host-sess -n rem -x 120 -y 39
	$DST new-session -d -s other -x 120 -y 39
	# The splash popup swallows every keystroke that dismisses it; it is not
	# under test here.
	$DST set-hook -gu 'client-attached[50]'
	$DST set-hook -gu 'client-session-changed[50]'
	# The window list is asserted as option state, not as drawn output. The
	# per-tick `#()` jobs in status-format[0] run tmux-update-icons, whose own
	# forced reflows are a second reflow source that can mask this regression —
	# a mirror whose window set was just created trips one within a tick. A
	# blank global format (copied into a session-scope format by any reflow)
	# keeps them from ever running; the `-B` sweep still fires but skips
	# @bridge_win windows.
	$DST set -g status off
	$DST set -g 'status-format[0]' ''

	"$DAEMON" --test-local --src-socket og820src --dst-socket og820dst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$TMUX_TMPDIR/d.sock" \
		--reflow "$REFLOW" >"$TMUX_TMPDIR/daemon.log" 2>&1 &
	DAEMON_PID=$!

	# Every mirror window exists and the shipper has stamped a label on the
	# first one; reflow-derived options are only written by reflow, so a
	# settled shipper means the forced passes that skipped on empty width
	# happened (or are about to be no-ops).
	poll 15 mirror_windows_ready || {
		echo "mirror windows never reached four" >&3
		sed -n '1,40p' "$TMUX_TMPDIR/daemon.log" >&3 2>&1 || true
		return 1
	}
	poll 15 label_shipped || {
		echo "the label shipper never stamped @bridge_label_id" >&3
		sed -n '1,40p' "$TMUX_TMPDIR/daemon.log" >&3 2>&1 || true
		return 1
	}
	sleep 0.5
}

@test "switching a client into a mirror reflows it before any interaction" {
	bridge_up
	# Precondition: no client has ever been on the mirror, so no layout was
	# ever stamped — the first real pass is the one under test (#235).
	run reflow_key_set host-sess
	[ "$status" -ne 0 ] || {
		echo "the mirror already carries a reflow key with no client: $($DST display-message -t host-sess -p '#{@reflow_key}')" >&3
		return 1
	}

	# A client attaches to a *different* session first (the picker path), then
	# switches onto the mirror.
	$OBS new-session -d -s obs -x 120 -y 40 "$DST attach -t other"
	poll 10 client_on other || {
		echo "the pty-host client never attached to other" >&3
		return 1
	}

	$DST switch-client -t host-sess
	poll 10 client_on host-sess || {
		echo "switch-client never landed the client on host-sess" >&3
		return 1
	}

	# The bug: the switch fires client-session-changed, and a session-scoped
	# daemon hook shadowed the conf's reflow hook, so nothing reflowed until an
	# unrelated interaction.
	if ! poll 10 reflow_key_set host-sess; then
		echo "no reflow after switching into the mirror — the reported bug" >&3
		echo "--- session state: $($DST display-message -t host-sess -p 'key=#{@reflow_key} status=#{status}')" >&3
		$DST list-windows -t host-sess -F '#{window_index}|#{@window_label_short}' >&3
		return 1
	fi

	equals_forced_pass host-sess
}

@test "attaching a client to a mirror reflows it before any interaction" {
	bridge_up
	run reflow_key_set host-sess
	[ "$status" -ne 0 ] || {
		echo "the mirror already carries a reflow key with no client" >&3
		return 1
	}

	$OBS new-session -d -s obs -x 120 -y 40 "$DST attach -t host-sess"
	poll 10 client_on host-sess || {
		echo "the pty-host client never attached to host-sess" >&3
		return 1
	}

	if ! poll 10 reflow_key_set host-sess; then
		echo "no reflow after attaching to the mirror" >&3
		echo "--- session state: $($DST display-message -t host-sess -p 'key=#{@reflow_key} status=#{status}')" >&3
		return 1
	fi

	equals_forced_pass host-sess
}
