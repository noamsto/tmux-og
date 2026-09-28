#!/usr/bin/env bats
# shellcheck disable=SC2016,SC2030,SC2031  # fixture script expands literals at runtime; bats @test blocks run in subshells
setup() {
	SCRIPT="$(dirname "$BATS_TEST_DIRNAME")/scripts/tmux-smart-nav.sh"
	STUB="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STUB"
	export KITTY_LOG="$BATS_TEST_TMPDIR/kitty.log"
	: >"$KITTY_LOG"
	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	: >"$TMUX_LOG"
	printf '#!/usr/bin/env bash\necho "$*" >>"%s"\n' "$KITTY_LOG" >"$STUB/kitty"
	printf '%s\n' '#!/usr/bin/env bash' 'if [ "$1" = list-panes ]; then' '  printf "%b" "${TMUX_PANES:-}"' '  exit 0' 'fi' >"$STUB/tmux"
	printf 'echo "$*" >>"%s"\n' "$TMUX_LOG" >>"$STUB/tmux"
	chmod +x "$STUB/kitty" "$STUB/tmux"
	export PATH="$STUB:$PATH"
}

@test "zoomed: no movement at all" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 1 1 0 0 %0 @0
	[ "$status" -eq 0 ]
	[ ! -s "$KITTY_LOG" ]
	[ ! -s "$TMUX_LOG" ]
}

@test "non-edge: select-pane within tmux" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 0 0 0 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "edge without KITTY_LISTEN_ON: falls back to select-pane" {
	unset KITTY_LISTEN_ON
	run bash "$SCRIPT" R right 0 1 0 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "edge with kitty: hand off to neighboring_window" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 0 1 0 0 %0 @0
	grep -q '@ action neighboring_window right' "$KITTY_LOG"
	[ ! -s "$TMUX_LOG" ]
}

@test "edge with kitty failing (no neighbor): falls back to select-pane" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	printf '#!/usr/bin/env bash\nexit 1\n' >"$STUB/kitty"
	chmod +x "$STUB/kitty"
	run bash "$SCRIPT" R right 0 1 0 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "tiled pane enters the nearest fully directional float" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|61|0|39|20\n%2|1|0|41|5|20|10\n'
	run bash "$SCRIPT" R right 0 1 0 1 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "an overlapping float is reachable from a tiled pane" {
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|61|0|39|20\n%2|1|0|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 1 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "floating pane exits to the nearest tiled pane" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|41|0|39|20\n%2|1|0|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 1 1 1 %2 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "modal floats are never navigation targets" {
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|41|0|39|20\n%2|1|1|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 1 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
}

@test "perpendicular overlap wins over a closer off-axis pane" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|50|0|20|10\n%2|1|0|25|20|10|10\n'
	run bash "$SCRIPT" R right 0 0 0 1 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
}

@test "nearest perpendicular candidate wins when none overlap" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|30|30|10|10\n%2|1|0|50|15|10|10\n'
	run bash "$SCRIPT" R right 0 0 0 1 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "tiled origins prefer floats on an exact tie" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|30|0|20|10\n%2|1|0|30|0|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 1 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "floating origins prefer tiled panes on an exact tie" {
	export TMUX_PANES=$'%0|1|0|0|0|20|10\n%1|0|0|30|0|20|10\n%2|1|0|30|0|20|10\n'
	run bash "$SCRIPT" R right 0 0 1 1 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}
