#!/usr/bin/env bats
# Live proof for #1037: a second client attaching from another machine (ssh)
# must not leave the first, local client's per-machine env overwritten, and the
# smart-nav bind must resolve the kitty socket per invoking client. Clients are
# real pty attaches (`script`), so #{I/e:VAR} reads genuine client environments.

setup() {
	script --version 2>/dev/null | grep -q util-linux || skip "no GNU-style script(1) for a pty"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	unset TMUX

	TMUX_TMPDIR="$(mktemp -d /tmp/og-mce.XXXXXX)"
	export TMUX_TMPDIR
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/status"
	export OG_ENRICH_CACHE_DIR="$TMUX_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$TMUX_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$TMUX_TMPDIR/og-enrich-lock"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}

	# A client's stdin must stay open: EOF makes `script` type ^D into the pane.
	mkfifo "$TMUX_TMPDIR/hold"
	sleep 120 >"$TMUX_TMPDIR/hold" &
	HOLD_PID=$!
	# The server starts with none of the per-machine vars (see t), whatever the
	# developer shell holds.
	t new-session -d -s main -x 80 -y 24
}

teardown() {
	[ -n "${HOLD_PID:-}" ] && kill "$HOLD_PID" 2>/dev/null
	t kill-server 2>/dev/null || true
	[ -n "${TMUX_TMPDIR:-}" ] && rm -rf "$TMUX_TMPDIR"
	return 0
}

t() {
	env -u DISPLAY -u WAYLAND_DISPLAY -u WINDOWID -u XAUTHORITY -u KITTY_LISTEN_ON \
		-u AEYE_HOST -u SSH_CONNECTION -u SSH_AUTH_SOCK \
		timeout --foreground 30s "$TMUX_BIN" -L probe "$@"
}

wait_for() {
	local remaining=$1
	shift
	while ((remaining-- > 0)); do
		"$@" && return 0
		sleep 0.2
	done
	return 1
}

client_count() { [ "$(t list-clients -F '#{client_name}' | grep -c .)" -ge "$1" ]; }
sess_env() { t show-environment -t "${2:-main}" "$1" 2>/dev/null; }
# rec <var> [session] -- the hook's per-session record of the local client's
# value. display-message resolves "<session>:" reliably; `show-options -t b`
# was seen resolving to another session.
rec() { t display-message -p -t "${2:-main}:" "#{@og_local_$1}" 2>/dev/null; }
rec_is() { [ "$(rec "$1" "${3:-main}")" = "$2" ]; }
sess_env_is() { [ "$(sess_env "$1" "${3:-main}")" = "$2" ]; }

# attach_local / attach_ssh [session] -- background pty attach carrying that
# machine's env.
attach_local() {
	env -u SSH_CONNECTION KITTY_LISTEN_ON=unix:/local DISPLAY=:0 WINDOWID=111 \
		AEYE_HOST=kitty SSH_AUTH_SOCK=/local/agent \
		script -qfc "$TMUX_BIN -L probe attach -t ${1:-main}" /dev/null <"$TMUX_TMPDIR/hold" >/dev/null 2>&1 &
}
attach_local_nokitty() {
	env -u SSH_CONNECTION -u KITTY_LISTEN_ON DISPLAY=:0 \
		script -qfc "$TMUX_BIN -L probe attach -t main" /dev/null <"$TMUX_TMPDIR/hold" >/dev/null 2>&1 &
}
# attach_local_b <session> -- a second local terminal with different values.
attach_local_b() {
	env -u SSH_CONNECTION KITTY_LISTEN_ON=unix:/localb DISPLAY=:9 WINDOWID=999 \
		AEYE_HOST=kitty \
		script -qfc "$TMUX_BIN -L probe attach -t $1" /dev/null <"$TMUX_TMPDIR/hold" >/dev/null 2>&1 &
}
attach_ssh() {
	# Lacks WINDOWID/AEYE_HOST on purpose: update-environment marks them removed.
	env -u WINDOWID -u AEYE_HOST SSH_CONNECTION='1.2.3.4 22 5.6.7.8 22' \
		KITTY_LISTEN_ON=unix:/remote DISPLAY=localhost:10 SSH_AUTH_SOCK=/remote/agent \
		script -qfc "$TMUX_BIN -L probe attach -t ${1:-main}" /dev/null <"$TMUX_TMPDIR/hold" >/dev/null 2>&1 &
}
# local_newsession <name> -- a local pty client that creates its own session.
local_newsession() {
	env -u SSH_CONNECTION KITTY_LISTEN_ON=unix:/localnew DISPLAY=:7 WINDOWID=777 \
		AEYE_HOST=kitty \
		script -qfc "$TMUX_BIN -L probe new-session -s $1" /dev/null <"$TMUX_TMPDIR/hold" >/dev/null 2>&1 &
}

# pane_env <var> [session] -- what a freshly spawned pane sees.
pane_env() {
	local out="$TMUX_TMPDIR/pane-$RANDOM"
	t new-window -d -t "${2:-main}:" "printenv $1 >'$out'; sleep 30"
	wait_for 25 test -s "$out" || {
		echo "pane printed nothing for $1" >&2
		return 1
	}
	cat "$out"
}

@test "ssh attach after a local one leaves the session env at the local values" {
	attach_local
	wait_for 50 client_count 1
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/local
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/local

	attach_ssh
	wait_for 50 client_count 2
	wait_for 50 sess_env_is SSH_AUTH_SOCK SSH_AUTH_SOCK=/remote/agent
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/local
	sleep 1

	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/local
	sess_env_is DISPLAY DISPLAY=:0
	sess_env_is WINDOWID WINDOWID=111
	sess_env_is AEYE_HOST AEYE_HOST=kitty
	[ "$(pane_env DISPLAY)" = ":0" ]
	[ "$(pane_env KITTY_LISTEN_ON)" = "unix:/local" ]
}

@test "ssh client keeps its own forwarded agent socket (last attach wins)" {
	attach_local
	wait_for 50 client_count 1
	attach_ssh
	wait_for 50 client_count 2
	wait_for 50 sess_env_is SSH_AUTH_SOCK SSH_AUTH_SOCK=/remote/agent
}

@test "an ssh-only attach restores nothing: the ssh values stay" {
	attach_ssh
	wait_for 50 client_count 1
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/remote
	sleep 1
	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/remote
	sess_env_is DISPLAY DISPLAY=localhost:10
}

@test "a local new-session is recorded: a later ssh attach to it restores its values" {
	local_newsession loc
	wait_for 50 client_count 1
	wait_for 50 sess_env_is DISPLAY DISPLAY=:7 loc
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/localnew loc
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/localnew loc

	attach_ssh loc
	wait_for 50 client_count 2
	wait_for 50 sess_env_is SSH_AUTH_SOCK SSH_AUTH_SOCK=/remote/agent loc
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/localnew loc
	sleep 1

	sess_env_is DISPLAY DISPLAY=:7 loc
	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/localnew loc
	[ "$(pane_env DISPLAY loc)" = ":7" ]
}

@test "single client: a local client lacking KITTY_LISTEN_ON still marks it removed" {
	attach_local_nokitty
	wait_for 50 client_count 1
	wait_for 50 sess_env_is KITTY_LISTEN_ON -KITTY_LISTEN_ON
	sess_env_is DISPLAY DISPLAY=:0
}

@test "a local client lacking KITTY_LISTEN_ON, then an ssh client with it: session ends with it removed" {
	attach_local_nokitty
	wait_for 50 client_count 1
	wait_for 50 sess_env_is KITTY_LISTEN_ON -KITTY_LISTEN_ON
	wait_for 50 rec_is DISPLAY :0

	attach_ssh
	wait_for 50 client_count 2
	wait_for 50 sess_env_is SSH_AUTH_SOCK SSH_AUTH_SOCK=/remote/agent
	wait_for 50 sess_env_is KITTY_LISTEN_ON -KITTY_LISTEN_ON
	sleep 1

	sess_env_is KITTY_LISTEN_ON -KITTY_LISTEN_ON
	sess_env_is DISPLAY DISPLAY=:0
}

@test "new-session -d with no clients leaves existing records untouched" {
	attach_local
	wait_for 50 client_count 1
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/local

	t new-session -d -s other
	sleep 1

	rec_is KITTY_LISTEN_ON unix:/local
	[ -z "$(rec seen other)" ]
	[ -z "$(rec KITTY_LISTEN_ON other)" ]
}

@test "new-session -d while an ssh client is attached elsewhere does nothing" {
	attach_ssh
	wait_for 50 client_count 1
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/remote

	t new-session -d -s other
	sleep 1

	[ -z "$(rec seen other)" ]
	[ "$(sess_env KITTY_LISTEN_ON other)" != "KITTY_LISTEN_ON=unix:/remote" ]
	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/remote
}

@test "records are per session: another terminal's values never cross into a session" {
	t new-session -d -s b -x 80 -y 24
	attach_local
	wait_for 50 client_count 1
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/local
	attach_local_b b
	wait_for 50 client_count 2
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/localb b
	wait_for 50 sess_env_is WINDOWID WINDOWID=999 b

	attach_ssh
	wait_for 50 client_count 3
	wait_for 50 sess_env_is SSH_AUTH_SOCK SSH_AUTH_SOCK=/remote/agent
	wait_for 50 sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/local
	sleep 1

	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/local
	sess_env_is WINDOWID WINDOWID=111
	sess_env_is DISPLAY DISPLAY=:0
	sess_env_is KITTY_LISTEN_ON KITTY_LISTEN_ON=unix:/localb b
	rec_is KITTY_LISTEN_ON unix:/local
	rec_is KITTY_LISTEN_ON unix:/localb b
}

@test "smart-nav resolves the kitty socket per invoking client" {
	attach_local
	wait_for 50 client_count 1
	wait_for 50 rec_is KITTY_LISTEN_ON unix:/local
	attach_ssh
	wait_for 50 client_count 2
	sleep 1

	# A shim so the script's bare `tmux` reaches the scratch server.
	mkdir -p "$TMUX_TMPDIR/bin"
	printf '#!/bin/sh\nexec "%s" -L probe "$@"\n' "$TMUX_BIN" >"$TMUX_TMPDIR/bin/tmux"
	# shellcheck disable=SC2016  # the shim expands at run time
	printf '#!/bin/sh\necho "$KITTY_LISTEN_ON" >>"%s"\n' "$TMUX_TMPDIR/kitty.log" >"$TMUX_TMPDIR/bin/kitty"
	chmod +x "$TMUX_TMPDIR/bin/tmux" "$TMUX_TMPDIR/bin/kitty"
	# The session value differs from both clients', so only a per-client read passes.
	t set-environment -t main KITTY_LISTEN_ON unix:/session
	local root_dir pane win
	root_dir="$(dirname "$BATS_TEST_DIRNAME")"
	pane="$(t display-message -p -t main: '#{pane_id}')"
	win="$(t display-message -p -t main: '#{window_id}')"

	local frag c l_name='' r_name=''
	frag="$(t list-keys -T root C-h | grep -o 'client=#{?client_name,#{q:client_name},}' | head -1)"
	[ -n "$frag" ]
	for c in $(t list-clients -F '#{client_name}'); do
		case "$(t display-message -c "$c" -p '#{?#{I/e:SSH_CONNECTION},ssh,local}')" in
		local) l_name="$c" ;;
		ssh) r_name="$c" ;;
		esac
	done
	[ -n "$l_name" ] && [ -n "$r_name" ]
	[ "$(t display-message -c "$l_name" -p "$frag")" = "client=$l_name" ]

	PATH="$TMUX_TMPDIR/bin:$PATH" bash "$root_dir/scripts/tmux-smart-nav.sh" L left 0 1 0 "$pane" "$win" "client=$l_name"
	[ "$(cat "$TMUX_TMPDIR/kitty.log")" = "unix:/local" ]

	: >"$TMUX_TMPDIR/kitty.log"
	PATH="$TMUX_TMPDIR/bin:$PATH" bash "$root_dir/scripts/tmux-smart-nav.sh" L left 0 1 0 "$pane" "$win" "client=$r_name"
	[ "$(cat "$TMUX_TMPDIR/kitty.log")" = "unix:/remote" ]
}
