#!/usr/bin/env bats
# #925: the home-manager reloadTmux activation must find a server started
# under `TMUX_TMPDIR=$XDG_RUNTIME_DIR` (the startupSession systemd unit sets
# `TMUX_TMPDIR=%t`), not only the default `/tmp`-rooted socket.
#
# Each test runs the activation body lifted verbatim out of
# modules/home-manager.nix against real scratch servers. A thin tmux wrapper
# redirects the activation's "TMUX_TMPDIR unset" (default) candidate from the
# real `/tmp/tmux-$UID/default` -- which a local bats run must never touch --
# onto a scratch dir; everything else about the socket handling is stock tmux.

MODULE="$BATS_TEST_DIRNAME/../modules/home-manager.nix"

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to a tmux binary}"
	# `-f /dev/null` means no tmux-og hook ever runs, but keep the
	# wrapped-tmux suite isolation contract.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR"

	export HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$HOME"
	# A nested tmux (we are likely running inside one) must not steer the
	# scratch servers or the activation body.
	unset TMUX TMUX_TMPDIR XDG_RUNTIME_DIR
	SOCKET_DIRS=()
}

teardown() {
	local d
	for d in "${SOCKET_DIRS[@]}"; do
		# -u TMUX: with $TMUX set, tmux ignores TMUX_TMPDIR and targets the
		# socket named in $TMUX (the caller's live server).
		env -u TMUX TMUX_TMPDIR="$d" timeout --foreground 30s "$TMUX_BIN" kill-server 2>/dev/null || true
	done
	return 0
}

# isolated_tmux <default-dir> -- a wrapper that gives the activation's
# `unset TMUX_TMPDIR` candidate its own scratch socket parent, so the test
# never starts a server in the real /tmp. An explicit TMUX_TMPDIR passes
# through untouched.
isolated_tmux() {
	local default_dir=$1
	TMUX_WRAPPER="$BATS_TEST_TMPDIR/tmux.sh"
	cat >"$TMUX_WRAPPER" <<EOF
#!$BASH
# Drop $TMUX so the scratch TMUX_TMPDIR is authoritative; otherwise tmux
# would connect to whatever live server the caller's pane is attached to.
unset TMUX
if [ -z "\${TMUX_TMPDIR:-}" ]; then
	export TMUX_TMPDIR="$default_dir"
fi
exec "$TMUX_BIN" "\$@"
EOF
	chmod +x "$TMUX_WRAPPER"
}

# reload_body <conf> -- prints the reloadTmux activation script with its Nix
# store placeholders resolved to test doubles. $TMUX_WRAPPER and $REFLOW_BIN
# must be set.
reload_body() {
	local conf=$1
	sed -n "/reloadTmux = lib.hm.dag.entryAfter/,/^          '';$/p" "$MODULE" |
		sed '1d;$d' |
		# Undo the Nix indented-string escape: `''${` in the module is the
		# literal `${` the activation actually runs.
		sed "s/''\${/\${/g" |
		sed \
			-e "s#\${pkgs.tmux}/bin/tmux#$TMUX_WRAPPER#" \
			-e "s#\${tmuxConfig.script.tmux-reflow-windows}/bin/tmux-reflow-windows#$REFLOW_BIN#" \
			-e "s#\${tmuxConfig.tmuxConf}#$conf#"
}

# start_server <socket-parent-dir> -- starts a config-less scratch server at
# <dir>/tmux-<uid>/default and records the dir for teardown.
start_server() {
	local dir=$1
	mkdir -p "$dir"
	SOCKET_DIRS+=("$dir")
	env -u TMUX TMUX_TMPDIR="$dir" timeout --foreground 30s "$TMUX_BIN" -f /dev/null new-session -d -s main
}

marker() {
	env -u TMUX TMUX_TMPDIR="$1" timeout --foreground 30s "$TMUX_BIN" show-options -gqv @reload_marker 2>/dev/null || true
}

reflow_stub() {
	export REFLOW_LOG="$BATS_TEST_TMPDIR/reflow.log"
	: >"$REFLOW_LOG"
	REFLOW_BIN="$BATS_TEST_TMPDIR/reflow-stub.sh"
	cat >"$REFLOW_BIN" <<EOF
#!$BASH
printf '%s %s\n' "\${TMUX_TMPDIR:-<unset>}" "\$1" >>"$REFLOW_LOG"
EOF
	chmod +x "$REFLOW_BIN"
}

write_marker_conf() {
	local conf=$1 value=$2
	printf 'set -g @reload_marker %s\n' "$value" >"$conf"
}

@test "server under XDG_RUNTIME_DIR is reloaded when TMUX_TMPDIR is unset" {
	local xdg="$BATS_TEST_TMPDIR/xdg" default="$BATS_TEST_TMPDIR/default" conf="$BATS_TEST_TMPDIR/conf.tmux"
	write_marker_conf "$conf" xdg
	start_server "$xdg"
	# The default candidate has no server here, so only XDG is reloaded.
	isolated_tmux "$default"
	reflow_stub
	reload_body "$conf" >"$BATS_TEST_TMPDIR/reload.sh"

	run env -u TMUX -u TMUX_TMPDIR XDG_RUNTIME_DIR="$xdg" bash "$BATS_TEST_TMPDIR/reload.sh"

	[ "$status" -eq 0 ]
	[ "$(marker "$xdg")" = xdg ]
	[ -z "$(marker "$default")" ]
	[ "$(cat "$REFLOW_LOG")" = "$xdg main" ]
}

@test "the default TMUX_TMPDIR-unset candidate is still reloaded" {
	local xdg="$BATS_TEST_TMPDIR/xdg" default="$BATS_TEST_TMPDIR/default" conf="$BATS_TEST_TMPDIR/conf.tmux"
	write_marker_conf "$conf" default
	start_server "$default"
	# XDG has no server; the default candidate must carry the reload.
	isolated_tmux "$default"
	reflow_stub
	reload_body "$conf" >"$BATS_TEST_TMPDIR/reload.sh"

	run env -u TMUX -u TMUX_TMPDIR XDG_RUNTIME_DIR="$xdg" bash "$BATS_TEST_TMPDIR/reload.sh"

	[ "$status" -eq 0 ]
	[ "$(marker "$default")" = default ]
	[ -z "$(marker "$xdg")" ]
	[ "$(cat "$REFLOW_LOG")" = "<unset> main" ]
}

@test "both live servers are reloaded in one activation" {
	local xdg="$BATS_TEST_TMPDIR/xdg" default="$BATS_TEST_TMPDIR/default" conf="$BATS_TEST_TMPDIR/conf.tmux"
	write_marker_conf "$conf" both
	start_server "$xdg"
	start_server "$default"
	isolated_tmux "$default"
	reflow_stub
	reload_body "$conf" >"$BATS_TEST_TMPDIR/reload.sh"

	run env -u TMUX -u TMUX_TMPDIR XDG_RUNTIME_DIR="$xdg" bash "$BATS_TEST_TMPDIR/reload.sh"

	[ "$status" -eq 0 ]
	[ "$(marker "$xdg")" = both ]
	[ "$(marker "$default")" = both ]
	# One reflow per server: default first, then XDG.
	[ "$(cat "$REFLOW_LOG")" = "<unset> main
$xdg main" ]
}

@test "an inherited TMUX_TMPDIR is the only candidate" {
	local other="$BATS_TEST_TMPDIR/other" xdg="$BATS_TEST_TMPDIR/xdg" default="$BATS_TEST_TMPDIR/default" conf="$BATS_TEST_TMPDIR/conf.tmux"
	write_marker_conf "$conf" other
	start_server "$other"
	start_server "$xdg"
	isolated_tmux "$default"
	reflow_stub
	reload_body "$conf" >"$BATS_TEST_TMPDIR/reload.sh"

	run env -u TMUX TMUX_TMPDIR="$other" XDG_RUNTIME_DIR="$xdg" bash "$BATS_TEST_TMPDIR/reload.sh"

	[ "$status" -eq 0 ]
	[ "$(marker "$other")" = other ]
	# An explicit TMUX_TMPDIR wins: neither XDG nor the default is touched.
	[ -z "$(marker "$xdg")" ]
	[ -z "$(marker "$default")" ]
	[ "$(cat "$REFLOW_LOG")" = "$other main" ]
}

@test "no live server anywhere is a silent no-op" {
	local conf="$BATS_TEST_TMPDIR/conf.tmux"
	write_marker_conf "$conf" none
	isolated_tmux "$BATS_TEST_TMPDIR/default"
	reflow_stub
	reload_body "$conf" >"$BATS_TEST_TMPDIR/reload.sh"

	run env -u TMUX -u TMUX_TMPDIR XDG_RUNTIME_DIR="$BATS_TEST_TMPDIR/xdg" bash "$BATS_TEST_TMPDIR/reload.sh"

	[ "$status" -eq 0 ]
	[ ! -s "$REFLOW_LOG" ]
}
