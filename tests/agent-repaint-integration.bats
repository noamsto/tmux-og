#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for the newborn-agent repaint nudge (#883): a pane whose
# size changed between creation and its first agent sighting gets one real size
# change pair after the app is up, so the app repaints in full.
#
# The agent stand-in is a copy of bash named `pi` (so pane_current_command is an
# agent command) running a probe that logs `stty size` at boot and on SIGWINCH,
# only when the size CHANGED. That is Node's rule for process.stdout 'resize':
# a signal with no size change does nothing, so only a real resize proves the
# nudge reached the app.
#
# Every probe log line is `<boot|size> W H`. "Settled" is the last line equalling
# the pane's current size: the probe logs asynchronously on WINCH, so a size
# read straight after a resize can lead its own log.

setup() {
	IN="ar883-in-${BATS_TEST_NUMBER}-$$"
	OUT="ar883-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME" "$BATS_TEST_TMPDIR/bin" "$BATS_TEST_TMPDIR/tmux"
	unset TMUX TMUX_PANE
	export TMUX_TMPDIR="$BATS_TEST_TMPDIR/tmux"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	SHELL="$(command -v bash)"
	export SHELL
	# The sweep monitor hook runs in this server and deletes files under these
	# dirs, whose defaults are the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	install -m 755 "$(command -v bash)" "$BATS_TEST_TMPDIR/bin/pi"
	export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
	write_probe

	inner new-session -d -s s -x 200 -y 50
	inner set-option -g @splash_shown 1
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# The trap is installed after the boot sample, then one more sample closes the
# window where a resize lands between them and the default SIGWINCH action
# (ignore) would swallow it.
write_probe() {
	PROBE="$BATS_TEST_TMPDIR/probe.sh"
	cat >"$PROBE" <<-'EOF'
		log=$1
		last=
		snap() {
			local h w
			read -r h w < <(stty size </dev/tty) || return 0
			[[ "$w $h" == "$last" ]] && return 0
			last="$w $h"
			printf '%s %s\n' "$1" "$last" >>"$log"
		}
		printf 'probe up\n'
		snap boot
		trap 'snap size' WINCH
		snap size
		while :; do
			sleep 0.1 &
			wait $!
		done
	EOF
}

fail() {
	printf '%s\n' "$*" >&2
	return 1
}

# A bounded poll: retry a predicate every 0.2 s for <secs>, then report its
# final verdict.
wait_for() { # secs cmd...
	local deadline=$((SECONDS + $1))
	shift
	while ((SECONDS < deadline)); do
		"$@" && return 0
		sleep 0.2
	done
	"$@"
}

count() { wc -l <"$1"; }
pane_size() { inner display-message -p -t "$1" '#{pane_width} #{pane_height}'; }
# What a zoomed pane measures: the window, less the pane-border-status row.
zoom_size() { # window
	local w h border
	read -r w h < <(inner display-message -p -t "$1" '#{window_width} #{window_height}')
	border="$(inner show-options -gv pane-border-status)"
	[[ $border == off ]] || h=$((h - 1))
	printf '%s %s\n' "$w" "$h"
}
active_pane() { inner display-message -p -t "$1" '#{pane_id}'; }
# Pane geometry, not window_layout: the layout string carries active-pane
# history, so a nudge of a background pane changes it with no geometry change.
geometry() { inner list-panes -t "$1" -F '#{pane_id}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}'; }
last_pane() { inner display-message -p -t "$1" '#{P:#{?pane_last,#{pane_id},}}'; }

booted() { grep -q '^boot ' "$1"; }
at_least() { [[ $(count "$1") -ge $2 ]]; }
settled() { [[ -s $1 && "$(tail -n1 "$1")" == *" $(pane_size "$2")" ]]; }
claimed() { [[ -z "$(inner display-message -p -t "$1" '#{@og_birth_size}')" ]]; }
stamped() { [[ -n "$(inner display-message -p -t "$1" '#{@og_birth_size}')" ]]; }
has_line() { grep -qxF "$2" "$1"; }

probe_cmd() { printf 'pi %q %q' "$PROBE" "$1"; }

new_probe_window() { # log -> "<window_id> <pane_id>"
	: >"$1"
	inner new-window -d -P -F '#{window_id} #{pane_id}' "$(probe_cmd "$1")"
}

split_probe() { # target log -> pane_id
	: >"$2"
	inner split-window -d -h -P -F '#{pane_id}' -t "$1" "$(probe_cmd "$2")"
}

# Shrink a detached window, then drop the manual window-size the resize leaves
# behind: tmux keeps the window at the new size with the option inherited, which
# is the state the nudge must put back.
resize_detached() { # window
	inner resize-window -t "$1" -x 100 -y 30
	inner set-option -wu -t "$1" window-size
}

# Exactly one nudge pair after line n0: a change to the zoomed (window) size,
# then back to the settled size, and nothing else.
expect_pair() { # log n0 zoom_size settled_size
	local log=$1 n0=$2 win=$3 want=$4 got
	got="$(tail -n +$((n0 + 1)) "$log")"
	[[ $got == "size $win"$'\n'"size $want" ]] && return 0
	printf 'expected "size %s" then "size %s" after line %s of %s, got:\n%s\n' "$win" "$want" "$n0" "$log" "$got" >&2
	return 1
}

attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
}

@test "single pane resized after launch: one real size change pair, window-size restored" {
	local log="$BATS_TEST_TMPDIR/one.log" wid pid want n0
	read -r wid pid < <(new_probe_window "$log")
	wait_for 10 booted "$log" || fail "probe never booted"
	[ "$(inner display-message -p -t "$pid" '#{pane_current_command}')" = pi ]

	inner resize-window -t "$wid" -x 100 -y 30
	inner set-option -wu -t "$wid" window-size
	wait_for 15 settled "$log" "$pid" || fail "probe never settled at the resized pane size"
	want="$(pane_size "$pid")"
	n0="$(count "$log")"

	wait_for 25 at_least "$log" $((n0 + 2)) || fail "nudge never landed: no size change after settling at $want"
	[ "$(sed -n "$((n0 + 1))p" "$log")" != "size $want" ]
	[ "$(sed -n "$((n0 + 2))p" "$log")" = "size $want" ]
	[ "$(inner display-message -p -t "$wid" '#{window_width}x#{window_height}')" = 100x30 ]
	[ -z "$(inner show-options -wqv -t "$wid" window-size)" ]
}

@test "grid not viewed: every probe gets one zoom-sized change and back, focus and geometry untouched" {
	local lead_log="$BATS_TEST_TMPDIR/lead.log" role_log="$BATS_TEST_TMPDIR/role.log"
	local wid lead role lead_want role_want win act last geom n0_lead n0_role
	read -r wid lead < <(new_probe_window "$lead_log")
	wait_for 10 booted "$lead_log" || fail "lead never booted"
	role="$(split_probe "$lead" "$role_log")"
	wait_for 10 booted "$role_log" || fail "role never booted"
	inner select-layout -t "$wid" main-vertical

	# A -d-built window has no {last}, and no select-pane sequence can restore an
	# empty one, so make a known baseline: active = lead, last = role.
	inner select-pane -t "$role"
	inner select-pane -t "$lead"

	wait_for 15 settled "$lead_log" "$lead" || fail "lead never settled"
	wait_for 15 settled "$role_log" "$role" || fail "role never settled"
	lead_want="$(pane_size "$lead")"
	role_want="$(pane_size "$role")"
	win="$(zoom_size "$wid")"
	act="$(active_pane "$wid")"
	last="$(last_pane "$wid")"
	geom="$(geometry "$wid")"
	[ "$act" = "$lead" ]
	[ "$last" = "$role" ]
	n0_lead="$(count "$lead_log")"
	n0_role="$(count "$role_log")"

	wait_for 30 at_least "$lead_log" $((n0_lead + 2)) || fail "nudge never landed on the lead"
	wait_for 30 at_least "$role_log" $((n0_role + 2)) || fail "nudge never landed on the role"
	# Settle time for any stray extra change a sibling's nudge could cause.
	sleep 2

	expect_pair "$lead_log" "$n0_lead" "$win" "$lead_want"
	expect_pair "$role_log" "$n0_role" "$win" "$role_want"
	[ "$(inner display-message -p -t "$wid" '#{window_zoomed_flag}')" = 0 ]
	[ "$(active_pane "$wid")" = "$act" ]
	[ "$(last_pane "$wid")" = "$last" ]
	[ "$(geometry "$wid")" = "$geom" ]
}

@test "newborn agent pane never resized: no nudge" {
	local log="$BATS_TEST_TMPDIR/still.log" wid pid
	read -r wid pid < <(new_probe_window "$log")
	wait_for 10 booted "$log" || fail "probe never booted"
	[ "$(count "$log")" -eq 1 ]

	# sweep 5 s + ready >= 3 s + quiet 1 s, plus slack.
	sleep 12
	[ "$(count "$log")" -eq 1 ] || fail "unresized pane got a size change: $(cat "$log")"
}

@test "viewed grid, target not active: no zoom until the target becomes active, then the nudge lands" {
	local lead_log="$BATS_TEST_TMPDIR/lead.log" role_log="$BATS_TEST_TMPDIR/role.log"
	local wid lead role win t_s
	attach_client
	read -r wid lead < <(new_probe_window "$lead_log")
	wait_for 10 booted "$lead_log" || fail "lead never booted"
	inner select-window -t "$wid"
	role="$(split_probe "$lead" "$role_log")"
	wait_for 10 booted "$role_log" || fail "role never booted"
	inner select-layout -t "$wid" main-vertical
	[ "$(active_pane "$wid")" = "$lead" ]

	# The birth stamp is what a build with the feature leaves for the sweep to
	# claim; without it the claim is trivially "done" and the assertions below
	# are the ones that fail.
	wait_for 3 stamped "$role" || true
	wait_for 20 claimed "$role" || fail "sweep never claimed the role pane"
	t_s=$SECONDS

	# The worker is in its slot wait from t_s+4 s to at least t_s+12 s.
	while ((SECONDS < t_s + 6)); do sleep 0.2; done
	wait_for 10 settled "$role_log" "$role" || fail "role never settled"
	win="$(zoom_size "$wid")"
	if has_line "$role_log" "size $win"; then
		fail "role was zoomed while another pane was active"
	fi
	[ "$(active_pane "$wid")" = "$lead" ]

	inner select-pane -t "$role"
	wait_for 4 has_line "$role_log" "size $win" || fail "nudge never landed after the role became active"
}

@test "mirror window and float window: no nudge" {
	local mlog="$BATS_TEST_TMPDIR/mirror.log" flog="$BATS_TEST_TMPDIR/float.log"
	local mwid mpane fwid fpane float geom n_m n_f
	read -r mwid mpane < <(inner new-window -d -P -F '#{window_id} #{pane_id}' 'sleep 99')
	inner set-option -w -t "$mwid" @bridge_win 1
	: >"$mlog"
	inner respawn-pane -k -t "$mpane" "$(probe_cmd "$mlog")"
	wait_for 10 booted "$mlog" || fail "mirror probe never booted"
	[ "$(inner display-message -p -t "$mpane" '#{pane_current_command}')" = pi ]

	read -r fwid fpane < <(new_probe_window "$flog")
	wait_for 10 booted "$flog" || fail "float probe never booted"
	float="$(inner new-pane -d -t "$fpane" -x 20 -y 5 -P -F '#{pane_id}' 'sleep 99')"

	resize_detached "$mwid"
	resize_detached "$fwid"
	wait_for 15 settled "$mlog" "$mpane" || fail "mirror probe never settled"
	wait_for 15 settled "$flog" "$fpane" || fail "float probe never settled"
	geom="$(inner display-message -p -t "$float" '#{pane_left} #{pane_top} #{pane_width} #{pane_height}')"
	n_m="$(count "$mlog")"
	n_f="$(count "$flog")"

	sleep 12
	[ "$(count "$mlog")" -eq "$n_m" ] || fail "mirror window was nudged: $(cat "$mlog")"
	[ "$(count "$flog")" -eq "$n_f" ] || fail "float window was nudged: $(cat "$flog")"
	[ "$(inner display-message -p -t "$float" '#{pane_left} #{pane_top} #{pane_width} #{pane_height}')" = "$geom" ]
}
