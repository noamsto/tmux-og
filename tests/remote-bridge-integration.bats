#!/usr/bin/env bats

setup() {
	SOCK="$BATS_TEST_TMPDIR/t.sock"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	# BRIDGE points at the prebuilt og-remote-bridge binary; the nix check
	# sets it to a store path, fall back to building from source for local runs.
	if [[ -z ${BRIDGE:-} ]]; then
		BRIDGE="$BATS_TEST_TMPDIR/bridge"
		(cd "$BATS_TEST_DIRNAME/../picker" && go build -o "$BRIDGE" ./remotebridge/)
	fi
	tmux -S "$SOCK" -f /dev/null new-session -d -s src -x 80 -y 24
	tmux -S "$SOCK" send-keys -t src "printf HELLO_BRIDGE" Enter
	sleep 0.5
}

teardown() { tmux -S "$SOCK" kill-server 2>/dev/null || true; }

@test "bridge renders the remote window's current screen" {
	run timeout 5 "$BRIDGE" --ssh "" --tmux "tmux -S $SOCK" --session src --window 0 </dev/null
	[[ $output == *HELLO_BRIDGE* ]]
}

# Real-tty path: under a pty, render.Size succeeds so the bridge sends
# refresh-client and reads the real cursor — exercising the reply-block
# ordering (each command consumes its OWN reply) that the </dev/null case
# skips. Pre-fix this rendered the misconsumed cursor reply instead of the
# capture, and used refresh-client syntax that errors on next-3.8.
@test "bridge seeds multi-line content over a real tty (refresh-client path)" {
	# util-linux `script` isn't shipped on darwin; BSD script lacks -qec. This
	# pty path is Linux-only — skip where the right script(1) is absent.
	script --version 2>/dev/null | grep -q util-linux ||
		skip "util-linux script(1) unavailable"
	tmux -S "$SOCK" send-keys -t src \
		"printf 'LINE_ALPHA\\nLINE_BRAVO\\nLINE_CHARLIE\\n'" Enter
	sleep 0.5
	run timeout 5 script -qec \
		"$BRIDGE --ssh '' --tmux 'tmux -S $SOCK' --session src --window 0" /dev/null </dev/null
	[[ $output == *LINE_ALPHA* ]]
	[[ $output == *LINE_CHARLIE* ]]
}
