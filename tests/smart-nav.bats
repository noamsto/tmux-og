#!/usr/bin/env bats
# shellcheck disable=SC2016,SC2030,SC2031  # fixture script expands literals at runtime; bats @test blocks run in subshells
bats_require_minimum_version 1.5.0 # run --separate-stderr
setup() {
	LIB="$(dirname "$BATS_TEST_DIRNAME")/scripts/lib-float-cover.sh"
	# The Nix build substitutes @lib_float_cover@; do the same on a temp copy.
	SCRIPT="$BATS_TEST_TMPDIR/tmux-smart-nav.sh"
	sed "s|@lib_float_cover@|$LIB|" "$(dirname "$BATS_TEST_DIRNAME")/scripts/tmux-smart-nav.sh" >"$SCRIPT"
	STUB="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STUB"
	BASH_BIN="$(command -v bash)"
	export KITTY_LOG="$BATS_TEST_TMPDIR/kitty.log"
	: >"$KITTY_LOG"
	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	: >"$TMUX_LOG"
	printf '#!%s\necho "$*" >>"%s"\n' "$BASH_BIN" "$KITTY_LOG" >"$STUB/kitty"
	printf '%s\n' "#!$BASH_BIN" 'if [ "$1" = list-panes ]; then' '  [ "${TMUX_LIST_PANES_FAIL:-0}" = 1 ] && exit 1' '  printf "%b" "${TMUX_PANES:-}"' '  exit 0' 'fi' 'if [ "$1" = display-message ]; then printf "%s" "${TMUX_CLIENT_SOCK:-}"; exit 0; fi' >"$STUB/tmux"
	printf 'echo "$*" >>"%s"\n' "$TMUX_LOG" >>"$STUB/tmux"
	chmod +x "$STUB/kitty" "$STUB/tmux"
	export PATH="$STUB:$PATH"
}

@test "zoomed: no movement at all" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 1 1 0 %0 @0
	[ "$status" -eq 0 ]
	[ ! -s "$KITTY_LOG" ]
	[ ! -s "$TMUX_LOG" ]
}

@test "non-edge: select-pane within tmux" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "edge without KITTY_LISTEN_ON: falls back to select-pane" {
	unset KITTY_LISTEN_ON
	run bash "$SCRIPT" R right 0 1 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "edge with kitty: hand off to neighboring_window" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	run bash "$SCRIPT" R right 0 1 0 %0 @0
	grep -q '@ action neighboring_window right' "$KITTY_LOG"
	[ ! -s "$TMUX_LOG" ]
}

@test "client= arg: the client's own socket overrides the env one for the kitty call" {
	export KITTY_LISTEN_ON=unix:/tmp/server TMUX_CLIENT_SOCK=unix:/tmp/client
	printf '#!%s\necho "$KITTY_LISTEN_ON $*" >>"%s"\n' "$BASH_BIN" "$KITTY_LOG" >"$STUB/kitty"
	run bash "$SCRIPT" R right 0 1 0 %0 @0 client=/dev/pts/9
	grep -q '^unix:/tmp/client @ action neighboring_window right' "$KITTY_LOG"
	[ ! -s "$TMUX_LOG" ]
}

@test "client= arg whose client has no socket is authoritative: no hand-off" {
	export KITTY_LISTEN_ON=unix:/tmp/server TMUX_CLIENT_SOCK=
	run bash "$SCRIPT" R right 0 1 0 %0 @0 client=/dev/pts/9
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "empty client= arg (no client): no hand-off despite env socket" {
	export KITTY_LISTEN_ON=unix:/tmp/server TMUX_CLIENT_SOCK=unix:/tmp/client
	run bash "$SCRIPT" R right 0 1 0 %0 @0 client=
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "client= arg works without an env socket" {
	unset KITTY_LISTEN_ON
	export TMUX_CLIENT_SOCK=unix:/tmp/client
	run bash "$SCRIPT" R right 0 1 0 %0 @0 client=/dev/pts/9
	grep -q '@ action neighboring_window right' "$KITTY_LOG"
}

@test "failed geometry query falls back to native navigation without kitty" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_LIST_PANES_FAIL=1
	run bash "$SCRIPT" R right 0 1 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "edge with kitty failing (no neighbor): falls back to select-pane" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	printf '#!%s\nexit 1\n' "$BASH_BIN" >"$STUB/kitty"
	chmod +x "$STUB/kitty"
	run bash "$SCRIPT" R right 0 1 0 %0 @0
	grep -q 'select-pane -R' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "tiled pane enters the nearest fully directional float" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|61|0|39|20\n%2|1|0|41|5|20|10\n'
	run bash "$SCRIPT" R right 0 1 0 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "an overlapping float is reachable from a tiled pane" {
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|61|0|39|20\n%2|1|0|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "floating pane exits to the nearest tiled pane" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|41|0|39|20\n%2|1|0|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 1 1 %2 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

@test "modal floats are never navigation targets" {
	export TMUX_PANES=$'%0|0|0|0|0|40|20\n%1|0|0|41|0|39|20\n%2|1|1|30|5|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
}

@test "perpendicular overlap wins over a closer off-axis pane" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|50|0|20|10\n%2|1|0|25|20|10|10\n'
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
}

@test "nearest perpendicular candidate wins when none overlap" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|30|30|10|10\n%2|1|0|50|15|10|10\n'
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "tiled origins prefer floats on an exact tie" {
	export TMUX_PANES=$'%0|0|0|0|0|20|10\n%1|0|0|30|0|20|10\n%2|1|0|30|0|20|10\n'
	run bash "$SCRIPT" R right 0 0 0 %0 @0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "floating origins prefer tiled panes on an exact tie" {
	export TMUX_PANES=$'%0|1|0|0|0|20|10\n%1|0|0|30|0|20|10\n%2|1|0|30|0|20|10\n'
	run bash "$SCRIPT" R right 0 0 1 %0 @0
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	[ ! -s "$KITTY_LOG" ]
}

# A 100x40 window of four tiles; a float is appended by each test.
QUAD='%0|0|0|0|0|50|19\n%1|0|0|51|0|49|19\n%3|0|0|0|20|50|20\n%4|0|0|51|20|49|20\n'

# no_select: the move was a no-op (a bare `! grep` would not fail a bats test).
no_select() {
	if grep -q select-pane "$TMUX_LOG"; then return 1; fi
}

# nav <flag> <origin-floating> <origin-id>: one press with the geometry in $TMUX_PANES.
nav() {
	local dir
	case "$1" in L) dir=left ;; R) dir=right ;; U) dir=up ;; D) dir=down ;; esac
	run --separate-stderr bash "$SCRIPT" "$1" "$dir" 0 0 "$2" "$3" @0
	[ "$status" -eq 0 ]
	[ -z "$stderr" ]
}

@test "a covering float keeps focus in every direction" {
	export TMUX_PANES="${QUAD}%2|1|0|0|0|100|40\n"
	for d in L R U D; do
		: >"$TMUX_LOG"
		nav "$d" 1 %2
		no_select
		[ ! -s "$KITTY_LOG" ]
	done
}

@test "a covering float hands off to kitty at the edge" {
	export KITTY_LISTEN_ON=unix:/tmp/k
	export TMUX_PANES="${QUAD}%2|1|0|0|0|100|40\n"
	for d in L R U D; do
		: >"$KITTY_LOG"
		: >"$TMUX_LOG"
		nav "$d" 1 %2
		case "$d" in L) w=left ;; R) w=right ;; U) w=up ;; D) w=down ;; esac
		grep -q "@ action neighboring_window $w" "$KITTY_LOG"
		[ ! -s "$TMUX_LOG" ]
	done
}

@test "a smaller float exits to the visible tile on each side" {
	export TMUX_PANES="${QUAD}%2|1|0|20|5|60|30\n"
	nav L 1 %2
	grep -q 'select-pane -t %0' "$TMUX_LOG"
	: >"$TMUX_LOG"
	nav R 1 %2
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	: >"$TMUX_LOG"
	nav U 1 %2
	grep -q 'select-pane -t %0' "$TMUX_LOG"
	: >"$TMUX_LOG"
	nav D 1 %2
	grep -q 'select-pane -t %3' "$TMUX_LOG"
}

@test "columns boundary: 7 visible columns stay in the float, 8 exit" {
	# Right edge at 93 leaves columns 93..99 of the right tile: 7.
	export TMUX_PANES=$'%0|0|0|0|0|50|40\n%1|0|0|51|0|49|40\n%2|1|0|30|5|63|30\n'
	nav R 1 %2
	no_select
	export TMUX_PANES=$'%0|0|0|0|0|50|40\n%1|0|0|51|0|49|40\n%2|1|0|30|5|62|30\n'
	nav R 1 %2
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	: >"$TMUX_LOG"
	# Left edge at 7 leaves columns 0..6 of the left tile: 7.
	export TMUX_PANES=$'%0|0|0|0|0|50|40\n%1|0|0|51|0|49|40\n%2|1|0|7|5|40|30\n'
	nav L 1 %2
	no_select
	export TMUX_PANES=$'%0|0|0|0|0|50|40\n%1|0|0|51|0|49|40\n%2|1|0|8|5|40|30\n'
	nav L 1 %2
	grep -q 'select-pane -t %0' "$TMUX_LOG"
}

@test "rows boundary: 3 visible rows stay in the float, 4 exit" {
	# Bottom edge at 37 leaves rows 37..39 of the lower tile: 3.
	export TMUX_PANES=$'%0|0|0|0|0|100|19\n%1|0|0|0|20|100|20\n%2|1|0|10|2|80|35\n'
	nav D 1 %2
	no_select
	export TMUX_PANES=$'%0|0|0|0|0|100|19\n%1|0|0|0|20|100|20\n%2|1|0|10|2|80|34\n'
	nav D 1 %2
	grep -q 'select-pane -t %1' "$TMUX_LOG"
	: >"$TMUX_LOG"
	# Top edge at 3 leaves rows 0..2 of the upper tile: 3.
	export TMUX_PANES=$'%0|0|0|0|0|100|19\n%1|0|0|0|20|100|20\n%2|1|0|10|3|80|30\n'
	nav U 1 %2
	no_select
	export TMUX_PANES=$'%0|0|0|0|0|100|19\n%1|0|0|0|20|100|20\n%2|1|0|10|4|80|30\n'
	nav U 1 %2
	grep -q 'select-pane -t %0' "$TMUX_LOG"
}

@test "a tile still enters a covering float" {
	export TMUX_PANES="${QUAD}%2|1|0|0|0|100|40\n"
	nav R 0 %0
	grep -q 'select-pane -t %2' "$TMUX_LOG"
}

@test "a covering float still moves to another float" {
	export TMUX_PANES="${QUAD}%2|1|0|0|0|100|40\n%5|1|0|60|5|30|10\n"
	nav R 1 %2
	grep -q 'select-pane -t %5' "$TMUX_LOG"
}

@test "a window without floats keeps native navigation" {
	export TMUX_PANES="$QUAD"
	nav R 0 %0
	grep -q 'select-pane -R' "$TMUX_LOG"
}
