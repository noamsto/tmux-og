#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for tmux's default menus re-bound on a mirror window
# (#769): a real keypress and a real right-click, delivered by a real attached
# client, driving the real emitted conf. Same reasoning and the same harness
# shape as tests/rename-bind-integration.bats (#367) — a conf grep can only say
# the text is there, and a key/mouse binding fires only for an attached client.

setup() {
	IN="mb769-in-${BATS_TEST_NUMBER}-$$"
	OUT="mb769-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TMUX_RAW="${TMUX_RAW:?set TMUX_RAW to the raw pinned binary}"
	CONF="${CONF:?set CONF to the emitted tmux.conf}"
	STOCK_MENUS="${STOCK_MENUS:?set STOCK_MENUS to generator/render/stockmenus.txt}"
	STOCK_DRAGS="${STOCK_DRAGS:?set STOCK_DRAGS to generator/render/stockdrags.txt}"
	CTL_MAIN_GO="${CTL_MAIN_GO:?set CTL_MAIN_GO to picker/remotebridge/cmd/ctl/main.go}"
	CTL_GO="${CTL_GO:?set CTL_GO to picker/remotebridge/daemon/ctl.go}"
	# The remote pane id the gate carries. One constant: setup() stamps it and
	# write_argv() builds the expected wire payload from it, so they cannot drift.
	BRIDGE_PANE='%42'
	# The window-menu Rename item prefills from this; a fixed value here lets
	# most tests submit it unedited, and the hostile-rename test overrides it.
	BRIDGE_WINDOW_NAME='mirror-window'
	CTL="${CTL:?set CTL to the built og-remote-bridge-ctl}"
	CTL_PROTOCOL_VERSION="${CTL_PROTOCOL_VERSION:?set CTL_PROTOCOL_VERSION to wire.CtlProtocolVersion}"

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	SHELL="$(command -v bash)"
	export SHELL
	# The poller and sweep monitor hooks fire inside this test server, and the
	# sweep reaches two functions that delete files under these dirs — whose
	# defaults are the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	SOCK="$BATS_TEST_TMPDIR/d.sock"
	REC_DIR="$BATS_TEST_TMPDIR/rec"
	REC_WORK="$BATS_TEST_TMPDIR/rec-work"
	mkdir -p "$REC_DIR" "$REC_WORK"
	start_recorder

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat keys and repaint the status line the menu
	# assertions read.
	inner set-option -g @splash_shown 1

	# Flip the bridge gate: @bridge_win on the window, @bridge_pane on the PANE
	# (production stamps it there; a window stamp only resolves by inheritance
	# and would leave the gate a possible false negative), @bridge_sock on the
	# SESSION.
	inner set-option -t s @bridge_sock "$SOCK"
	WIN="$(inner display-message -p -t s: '#{window_id}')"
	inner set-option -w -t "$WIN" @bridge_win 1
	inner set-option -w -t "$WIN" @window_bridge_name "$BRIDGE_WINDOW_NAME"
	PANE="$(inner list-panes -t s: -F '#{pane_id}' | head -1)"
	inner set-option -p -t "$PANE" @bridge_pane "$BRIDGE_PANE"
	[ "$(inner display-message -p -t "$PANE" -F '#{&&:#{@bridge_win},#{@bridge_pane}}')" = 1 ]
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	[[ -n ${RECORDER_PID:-} ]] && kill "$RECORDER_PID" 2>/dev/null
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# The recording stub standing in for the bridge daemon on @bridge_sock. One
# copy per connection (socat fork); it must answer with a FrameCtlAck, or ctl
# reports "does not speak the ctl protocol" through `display-message -t
# <client>` and overwrites the status line the menu assertions read.
start_recorder() {
	local rec="$BATS_TEST_TMPDIR/recorder.sh"
	cat >"$rec" <<-'EOF'
		set -uo pipefail
		w="$(mktemp -d "$REC_WORK/w.XXXXXX")"
		dd bs=1 count=5 status=none of="$w/hdr" || exit 0
		[[ $(stat -c %s "$w/hdr") -eq 5 ]] || exit 0
		read -r -a b < <(od -An -tu1 -N5 "$w/hdr")
		printf '%s' "${b[0]}" >"$w/type"
		n=$((b[1] * 16777216 + b[2] * 65536 + b[3] * 256 + b[4]))
		: >"$w/payload"
		if ((n > 0)); then
			dd bs=1 count="$n" status=none of="$w/payload" || exit 0
		fi
		# Rename into place only once both files are complete, so a reader that
		# sees the directory sees a whole frame.
		mv "$w" "$REC_DIR/frame.${w##*/}"
		printf '\007'
		head -c 4 /dev/zero
	EOF
	export REC_DIR REC_WORK
	socat "UNIX-LISTEN:$SOCK,fork" "EXEC:bash $rec" &
	RECORDER_PID=$!
	local deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		[[ -S $SOCK ]] && return 0
		sleep 0.1
	done
	printf 'recorder socket never appeared at %s\n' "$SOCK" >&2
	return 1
}

# A real attached client for the inner server: its keys and mouse events come
# from a second server's pane, which is where a keypress or click can actually
# reach the key table.
attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	OPANE="$(outer list-panes -F '#{pane_id}' | head -1)"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
	PREFIX="$(inner show-options -gv prefix)"
}

send() { outer send-keys -t "$OPANE" "$@"; }

press_prefix_key() { # key
	send "$PREFIX"
	sleep 0.2
	send "$1"
	sleep 0.3
}

# A right click (SGR mouse button 2) at a 1-based column/row. Raw bytes, not
# `-l`: the client's extended keys re-encode a literal ESC, so a printf'd
# sequence would never reach the pane as the mouse report it needs to be. Only
# the press is sent — the release closes whatever menu the press just opened.
click() { # col row
	# shellcheck disable=SC2046 # word-split on purpose: -H takes one hex byte per argument
	outer send-keys -t "$OPANE" -H $(printf '\e[<2;%d;%dM' "$1" "$2" | od -An -tx1)
	sleep 0.3
}

screen() { outer capture-pane -p -t "$OPANE"; }

# A plain `!` in front of a pipeline is a bats trap (SC2314): bats does not see
# the negation, only the pipeline's own exit status. Wrapping it in a function
# keeps the `!` where the shell actually applies it, and the call site stays
# a single simple command bats can fail on.
screen_lacks() { # text
	! screen | grep -qF "$1"
}

wait_for_prompt() { # expected prompt text
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		screen | grep -qF "(run-shell) $1" && return 0
		sleep 0.1
	done
	printf 'timed out waiting for prompt "(run-shell) %s"; screen was:\n' "$1" >&2
	screen >&2
	return 1
}

# A menu opened via press_prefix_key/click is drawn asynchronously; on a slow
# runner a blind keypress can land before it's on screen and read back as
# literal pane input. Poll instead of a single-shot screen grep.
wait_for_screen() { # expected text
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		screen | grep -qF "$1" && return 0
		sleep 0.1
	done
	printf 'timed out waiting for screen text "%s"; screen was:\n' "$1" >&2
	screen >&2
	return 1
}

wait_for_frame() {
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		compgen -G "$REC_DIR/frame.*/payload" >/dev/null && return 0
		sleep 0.1
	done
	printf 'timed out waiting for a ctl frame in %s\n' "$REC_DIR" >&2
	return 1
}

clear_frames() { rm -rf "${REC_DIR:?}"/frame.*; }

# A `#(...)` job is asynchronous — format_job_get substitutes the previous
# (empty) value and the job lands later — so "it never ran" is only a claim
# once the whole window has elapsed with nothing there. The positive budget is
# sized for CI CPU contention, where the click → display-menu → run-shell chain
# exceeds the old 5s and a payload that DID fire read as a miss (#832);
# negative `run !` callers pass the smaller window (they waited for the menu to
# render already). A timeout dumps the client screen for diagnosis.
SENTINEL_BUDGET_SECS=30
SENTINEL_NEGATIVE_SECS=10

wait_for_sentinel() { # path [budget_secs]
	local budget="${2:-$SENTINEL_BUDGET_SECS}"
	local deadline=$((SECONDS + budget))
	while ((SECONDS < deadline)); do
		[[ -e $1 ]] && return 0
		sleep 0.1
	done
	printf 'wait_for_sentinel("%s") timed out after %ss; screen was:\n' "$1" "$budget" >&2
	screen >&2
	return 1
}

# The single recorded frame's payload file. Fails loudly on 0 or 2+, so an
# extra connection can never be mistaken for the one under test.
sole_payload() {
	local -a d
	mapfile -t d < <(compgen -G "$REC_DIR/frame.*/payload")
	[ "${#d[@]}" -eq 1 ]
	printf '%s' "${d[0]}"
}

# The EncodeArgv payload for one ctl request, written straight to a file: bash
# cannot hold a NUL, so it never touches a variable. args are the words after
# the gated pane's own @bridge_pane, which every mirror item sends first.
write_argv() { # file verb [arg...]
	local file=$1 verb=$2
	shift 2
	{
		printf '%s\000%s\000%s' "$CTL_PROTOCOL_VERSION" "$verb" "$BRIDGE_PANE"
		local a
		for a in "$@"; do
			printf '\000%s' "$a"
		done
	} >"$file"
}

# Byte-exact comparison against the EncodeArgv shape produced above.
assert_wire_argv() { # payload_file verb [arg...]
	local payload_file=$1 verb=$2
	shift 2
	local want="$BATS_TEST_TMPDIR/want"
	write_argv "$want" "$verb" "$@"
	if ! cmp -s "$want" "$payload_file"; then
		printf 'wire argv mismatch for verb [%s] args [%s]\nwant:\n' "$verb" "$*" >&2
		od -c "$want" >&2
		printf 'got:\n' >&2
		od -c "$payload_file" >&2
		return 1
	fi
}

# --- 1: prefix < (window menu) -------------------------------------------

@test "prefix < Kill and New Window reach the remote" {
	attach_client

	press_prefix_key '<'
	wait_for_screen 'Kill'
	send X
	wait_for_frame
	assert_wire_argv "$(sole_payload)" kill-window
	clear_frames

	press_prefix_key '<'
	wait_for_screen 'New Window'
	send w
	wait_for_frame
	assert_wire_argv "$(sole_payload)" new-window
}

@test "prefix < Rename prompts with the bridge name and sends it unedited" {
	attach_client

	press_prefix_key '<'
	wait_for_screen 'Rename'
	send n
	wait_for_prompt "$BRIDGE_WINDOW_NAME"
	send Enter
	wait_for_frame
	assert_wire_argv "$(sole_payload)" rename "$BRIDGE_WINDOW_NAME"
}

# --- 2: prefix > (pane menu) ----------------------------------------------

@test "prefix > Split/Kill/Zoom/Swap all reach the remote and locals stay put" {
	attach_client
	# Zoom and Swap Up/Down are DISABLED stock menu items ("-" prefixed label)
	# until the window has a second pane, so this second local pane has to
	# exist for the whole test, not just the swap items. It carries its own
	# @bridge_pane, distinct from the pressed-in pane's %42, so a swap frame
	# can only read back as %42 if ctl always sends the CURRENT pane's id.
	inner split-window -d -t "$PANE"
	local p2
	p2="$(inner list-panes -t s: -F '#{pane_id}' | tail -1)"
	inner set-option -p -t "$p2" @bridge_pane '%43'
	local wins panes
	wins="$(inner list-windows -t s: | wc -l)"
	panes="$(inner list-panes -t s: | wc -l)"

	press_prefix_key '>'
	wait_for_screen 'Horizontal Split'
	send h
	wait_for_frame
	assert_wire_argv "$(sole_payload)" split-h
	clear_frames

	press_prefix_key '>'
	wait_for_screen 'Vertical Split'
	send v
	wait_for_frame
	assert_wire_argv "$(sole_payload)" split-v
	clear_frames

	press_prefix_key '>'
	wait_for_screen 'Kill'
	send X
	wait_for_frame
	assert_wire_argv "$(sole_payload)" kill-pane
	clear_frames

	press_prefix_key '>'
	wait_for_screen 'Zoom'
	send z
	wait_for_frame
	assert_wire_argv "$(sole_payload)" zoom
	clear_frames

	press_prefix_key '>'
	wait_for_screen 'Swap Up'
	send u
	wait_for_frame
	assert_wire_argv "$(sole_payload)" swap U
	clear_frames

	press_prefix_key '>'
	wait_for_screen 'Swap Down'
	send d
	wait_for_frame
	assert_wire_argv "$(sole_payload)" swap D

	[ "$(inner list-windows -t s: | wc -l)" -eq "$wins" ]
	[ "$(inner list-panes -t s: | wc -l)" -eq "$panes" ]
}

# --- 3: mouse ---------------------------------------------------------------

@test "a right-click in the pane area opens the pane menu; Kill reaches the remote" {
	attach_client
	click 20 20
	wait_for_screen 'Kill'
	send X
	wait_for_frame
	assert_wire_argv "$(sole_payload)" kill-pane
}

@test "a right-click on the current window's tab opens the window menu; Kill reaches the remote" {
	attach_client
	# Row 1 is the session pill, row 2 the window list; the current window's
	# tab starts right after the "╰─ 1: " gutter.
	click 5 2
	wait_for_screen 'Kill'
	send X
	wait_for_frame
	assert_wire_argv "$(sole_payload)" kill-window
}

# --- 4: hostile rename -------------------------------------------------------

@test "menu rename carries a hostile name through the wire and never executes it" {
	attach_client
	local sentinel="$BATS_TEST_TMPDIR/sentinel"
	local hostile="a'b;c#d}e f#(touch $sentinel)"
	inner set-option -w -t "$WIN" @window_bridge_name "$hostile"

	press_prefix_key '<'
	wait_for_screen 'Rename'
	send n
	wait_for_prompt "$hostile"
	send Enter
	wait_for_frame

	run ! wait_for_sentinel "$sentinel" "$SENTINEL_NEGATIVE_SECS"

	assert_wire_argv "$(sole_payload)" rename "$hostile"
}

# --- 4b: session menu Detach --------------------------------------------

@test "session menu Detach falls back to kill-session with no daemon pid file" {
	attach_client
	# A second session that must survive: with `s` the only session,
	# kill-session ends the server, and a crashed server would read `has-session`
	# as false too — a false pass. keep proves the fallback targeted `s` alone.
	inner new-session -d -s keep

	click 3 1
	wait_for_screen 'Detach'
	wait_for_screen 'New Window'
	wait_for_screen 'Renumber'
	screen_lacks 'Rename'
	send w
	wait_for_frame
	assert_wire_argv "$(sole_payload)" new-window
	[ "$(inner list-windows -t s: | wc -l)" -eq 1 ]
	clear_frames

	click 3 1
	wait_for_screen 'Detach'
	send d
	local deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		inner has-session -t =s 2>/dev/null || break
		sleep 0.2
	done
	run inner has-session -t =s
	[ "$status" -ne 0 ]
	run inner has-session -t =keep
	[ "$status" -eq 0 ]
	run compgen -G "$REC_DIR/frame.*/payload"
	[ "$status" -ne 0 ]
}

# --- 5: hidden and relabelled items ------------------------------------------

@test "hidden and relabelled items in the mirror menus" {
	attach_client

	press_prefix_key '<'
	wait_for_screen 'Kill'
	screen | grep -qF 'Rename'
	# #784: the window menu's Respawn now asks the remote to respawn the window
	# (it is no longer hidden).
	screen | grep -qF 'Respawn'
	screen_lacks 'Mark'
	screen_lacks 'Swap Marked'
	screen_lacks 'New After'
	send Escape
	sleep 0.2

	press_prefix_key '>'
	wait_for_screen 'Reconnect'
	# Both gestures are present: Respawn restarts the REMOTE program, Reconnect
	# redials the local renderer (#547).
	screen | grep -qF 'Respawn'
	screen | grep -qF 'Reconnect'
	screen_lacks 'Mark'
	screen_lacks 'Float'
}

# --- 6: non-mirror parity ----------------------------------------------------

@test "a plain window's menus stay entirely local" {
	attach_client
	# -P -F reads back the new window's id directly: with a client already
	# attached, automatic-rename can overwrite the -n name before a lookup by
	# name gets to it (it races enrichment's rename of the mirror window too).
	local plainwin
	plainwin="$(inner new-window -d -t s: -P -F '#{window_id}')"
	# A new window carries no @bridge_win of its own; unset it explicitly so
	# this test does not depend on that default.
	inner set-option -uw -t "$plainwin" @bridge_win
	inner select-window -t "$plainwin"
	sleep 0.3

	press_prefix_key '>'
	wait_for_screen 'Respawn'
	screen | grep -qF 'Mark'
	send Escape
	sleep 0.2

	local wins
	wins="$(inner list-windows -t s: | wc -l)"

	press_prefix_key '<'
	wait_for_screen 'Kill'
	send X
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[ "$(inner list-windows -t s: | wc -l)" -eq $((wins - 1)) ] && break
		sleep 0.1
	done
	[ "$(inner list-windows -t s: | wc -l)" -eq $((wins - 1)) ]
	run compgen -G "$REC_DIR/frame.*/payload"
	[ "$status" -ne 0 ]
}

# --- 7: stock tripwire --------------------------------------------------

@test "stock tripwire: the pinned binary's own menu and drag binds and version match what the conf assumes" {
	local raw_out="$BATS_TEST_TMPDIR/raw-stock.txt"
	local rawT="$BATS_TEST_TMPDIR/raw-tmux"
	mkdir -p "$rawT"
	local line table key stock
	for stock in "$STOCK_MENUS" "$STOCK_DRAGS"; do
		while IFS= read -r line; do
			table="$(printf '%s' "$line" | awk '{print $3}')"
			key="$(printf '%s' "$line" | awk '{print $4}')"
			TMUX_TMPDIR="$rawT" "$TMUX_RAW" -f /dev/null -L rawsm list-keys -T "$table" "$key"
		done <"$stock" >"$raw_out"
		diff "$stock" "$raw_out"
	done
	TMUX_TMPDIR="$rawT" "$TMUX_RAW" -L rawsm kill-server 2>/dev/null || true

	local conf_version raw_version
	conf_version="$(grep -m1 -oE '%if "#\{==:#\{version\},[^}]+\}"' "$CONF" | sed -E 's/.*version\},([^}]+)\}.*/\1/')"
	local verT="$BATS_TEST_TMPDIR/ver-tmux"
	mkdir -p "$verT"
	TMUX_TMPDIR="$verT" "$TMUX_RAW" -f /dev/null -L versm new-session -d
	raw_version="$(TMUX_TMPDIR="$verT" "$TMUX_RAW" -L versm display-message -p '#{version}')"
	TMUX_TMPDIR="$verT" "$TMUX_RAW" -L versm kill-server 2>/dev/null || true
	[ "$raw_version" = "$conf_version" ]
}

# --- 8: version gate ----------------------------------------------------

@test "the %if block gates the menu binds on the pinned tmux version" {
	local block="$BATS_TEST_TMPDIR/menublock.conf"
	sed -n '/^%if "#{==:#{version},/,/^%endif$/p' "$CONF" >"$block"
	[ -s "$block" ]

	local gT="$BATS_TEST_TMPDIR/gate-tmux"
	mkdir -p "$gT"
	TMUX_TMPDIR="$gT" "$TMUX_RAW" -f /dev/null -L gatesm new-session -d
	TMUX_TMPDIR="$gT" "$TMUX_RAW" -L gatesm source-file "$block"
	local got
	got="$(TMUX_TMPDIR="$gT" "$TMUX_RAW" -L gatesm list-keys -T prefix '<')"
	TMUX_TMPDIR="$gT" "$TMUX_RAW" -L gatesm kill-server 2>/dev/null || true
	printf '%s' "$got" | grep -qF 'if-shell -F'

	# Flip the version literal so the gate never matches; every one of the ten
	# bindings must fall back to its raw stock default, byte for byte. Read
	# from the block itself, not hardcoded, so a future stockMenuVersion bump
	# can't leave this substitution a silent no-op.
	local block_version
	block_version="$(head -1 "$block" | sed -E 's/.*version\},([^}]+)\}.*/\1/')"
	local other="$BATS_TEST_TMPDIR/menublock-other.conf"
	sed "s/${block_version//./\\.}/next-0.0/" "$block" >"$other"
	local gT2="$BATS_TEST_TMPDIR/gate-tmux2"
	mkdir -p "$gT2"
	TMUX_TMPDIR="$gT2" "$TMUX_RAW" -f /dev/null -L gatesm2 new-session -d
	TMUX_TMPDIR="$gT2" "$TMUX_RAW" -L gatesm2 source-file "$other"
	local line table key line_got
	while IFS= read -r line; do
		table="$(printf '%s' "$line" | awk '{print $3}')"
		key="$(printf '%s' "$line" | awk '{print $4}')"
		line_got="$(TMUX_TMPDIR="$gT2" "$TMUX_RAW" -L gatesm2 list-keys -T "$table" "$key")"
		[ "$line_got" = "$line" ]
	done < <(cat "$STOCK_MENUS" "$STOCK_DRAGS")
	TMUX_TMPDIR="$gT2" "$TMUX_RAW" -L gatesm2 kill-server 2>/dev/null || true
}

# --- 9: verb cross-check -------------------------------------------------

@test "every verb the menu block sends is a ctl verb ctl.go knows" {
	local block="$BATS_TEST_TMPDIR/menublock-verbs.conf"
	# The FIRST gated block only: the drag block after it sends a ctl-side
	# gesture, checked by the next test.
	sed -n '/^%if "#{==:#{version},/,/^%endif$/{p;/^%endif$/q}' "$CONF" >"$block"
	local verbs
	verbs="$(grep -oP -- '--sock=#+\{q:@bridge_sock\}\s+\K[a-z-]+' "$block" | sort -u)"
	[ -n "$verbs" ]
	local v
	while IFS= read -r v; do
		grep -qF "\"$v\": {" "$CTL_GO"
	done <<<"$verbs"
}

@test "the drag block sends only drag, which ctl resolves into the daemon's float-geom or tile-layout" {
	local block="$BATS_TEST_TMPDIR/dragblock-verbs.conf"
	# The SECOND gated block: count %if openings and print only the second.
	awk '/^%if "#\{==:#\{version\},/ { n++ } n == 2 { print } n == 2 && /^%endif$/ { exit }' "$CONF" >"$block"
	[ -s "$block" ]
	grep -q 'MouseDrag1Border' "$block"
	local verbs
	verbs="$(grep -oP -- '--sock=#+\{q:@bridge_sock\}\s+\K[a-z-]+' "$block" | sort -u)"
	[ "$verbs" = drag ]
	grep -qF '"drag"' "$CTL_MAIN_GO"
	grep -qF '"float-geom": {' "$CTL_GO"
	grep -qF '"tile-layout": {' "$CTL_GO"
}
