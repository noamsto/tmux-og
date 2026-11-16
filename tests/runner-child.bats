#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells
# #1136: a task runner (just/make) runs its recipe in its own process group, so
# pane_current_command names the runner for the whole recipe. The resolution
# rule is exercised on a stubbed process table; the tmux-update-icons wiring
# (one ps per tick, none without a runner pane) on a scratch server.

load helper

setup() {
	setup_lib_icons
}

# resolve PID FALLBACK < table
resolve() {
	load_proc_table
	resolve_runner_child "$1" "$2"
}

@test "just -> sh -> nix resolves to nix" {
	resolve 100 just <<-'EOF'
		  PID  PPID COMM
		  100     1 bash
		  101   100 just
		  102   101 sh
		  103   102 nix
	EOF
	[ "$REPLY" = nix ]
}

@test "nested runners: just -> make -> sh -> cargo resolves to cargo, not rustc" {
	resolve 100 just <<-'EOF'
		100 1 just
		101 100 make
		102 101 sh
		103 102 cargo
		104 103 rustc
	EOF
	[ "$REPLY" = cargo ]
}

@test "idle runner keeps its own name" {
	resolve 100 just <<-'EOF'
		100 1 bash
		101 100 just
		102 101 sh
	EOF
	[ "$REPLY" = just ]
}

@test ".foo-wrapped child is unwrapped" {
	resolve 100 make <<-'EOF'
		100 1 make
		101 100 .nix-wrapped
	EOF
	[ "$REPLY" = nix ]
}

@test "macOS full-path comm rows are reduced to the basename" {
	resolve 100 just <<-'EOF'
		100 1 /bin/zsh
		101 100 /nix/store/abc-just/bin/just
		102 101 /bin/sh
		103 102 /nix/store/abc/bin/.nix-wrapped
	EOF
	[ "$REPLY" = nix ]
}

@test "long wrapped child (argv[0], past ps comm's 15 chars) is unwrapped" {
	resolve 100 just <<-'EOF'
		100 1 just
		101 100 .quickshell-wrapped --flag
	EOF
	[ "$REPLY" = quickshell ]
}

@test "login-shell dash prefix and args are stripped" {
	resolve 100 just <<-'EOF'
		100 1 -bash
		101 100 just build
		102 101 sh -c nix build
		103 102 nix build .#x
	EOF
	[ "$REPLY" = nix ]
}

@test "runner is the pane process itself" {
	resolve 100 just <<-'EOF'
		100 1 just
		101 100 go
	EOF
	[ "$REPLY" = go ]
}

@test "a background job beside the runner is not picked up" {
	resolve 100 just <<-'EOF'
		100 1 bash
		101 100 sleep
		102 100 just
		103 102 sh
		104 103 cargo
	EOF
	[ "$REPLY" = cargo ]
}

@test "no runner under the pane keeps the fallback" {
	resolve 100 just <<-'EOF'
		100 1 bash
		101 100 vim
	EOF
	[ "$REPLY" = just ]
}

@test "is_runner knows runners and wrapped runners only" {
	is_runner just
	is_runner .make-wrapped
	run is_runner nix
	[ "$status" -ne 0 ]
	run is_runner ""
	[ "$status" -ne 0 ]
}

# --- tmux-update-icons wiring -------------------------------------------------

setup_server() {
	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux" XDG_DATA_HOME="$TDIR/data"
	export CLAUDE_STATUS_DIR="$TDIR/claude-status" TMPDIR="$TDIR"
	mkdir -p "$TMUX_TMPDIR" "$TDIR/bin"
	# shellcheck disable=SC2174
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,tasks,names}
	unset TMUX
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5)) AGENT_COMMANDS="claude"

	printf '#!/bin/sh\nexit 0\n' >"$TDIR/fake-reflow"
	chmod +x "$TDIR/fake-reflow"
	sed -e 's/@ICON_MAP@/["just"]="J" ["sh"]="S" ["sleep"]="Z"/' -e 's/@FALLBACK_ICON@/F/' scripts/lib-icons.sh >"$TDIR/lib-icons.sh"
	sed \
		-e "s|@lib_icons@|$TDIR/lib-icons.sh|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$TDIR/fake-reflow|g" \
		-e "s|@MAX_ICONS@|3|g" \
		scripts/tmux-update-icons.sh >"$TDIR/update-icons.sh"

	# Logs only the whole-table reads this feature adds; lib-claude's `ps -o`
	# liveness probes pass through unlogged.
	REAL_PS="$(command -v ps)"
	cat >"$TDIR/bin/ps" <<-EOF
		#!/bin/sh
		case "\$1" in -Ao) echo ps >>"$TDIR/ps.log" ;; esac
		exec "$REAL_PS" "\$@"
	EOF
	chmod +x "$TDIR/bin/ps"
	export PATH="$TDIR/bin:$PATH"
	: >"$TDIR/ps.log"

	# pane_current_command is the executable basename: bash copied to `just`.
	cp -L "$(command -v bash)" "$TDIR/bin/just"
	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"
	tmux -f /dev/null new-session -d -s S -c "$TDIR" -x 200 -y 50
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	PANE="$(tmux display-message -p -t S '#{pane_id}')"
}

teardown() {
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	tmux kill-server 2>/dev/null || true
}

wait_cmd() {
	local tries=30
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$PANE" '#{pane_current_command}')" = "$1" ] && return 0
		sleep 0.1
	done
	return 1
}

display_of() { tmux show -wv -t "$WIN" @window_icon_display 2>/dev/null || true; }

@test "no runner pane: update-icons reads no process table" {
	setup_server
	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	[ ! -s "$TDIR/ps.log" ]
}

@test "just pane running sleep shows the fallback icon, then the shell's once it ends" {
	setup_server
	tmux respawn-pane -k -t "$PANE" -- "$TDIR/bin/just" -c 'sleep 30; true'
	wait_cmd just
	local tries=30
	while ((tries-- > 0)); do
		# shellcheck disable=SC2009  # no pgrep on darwin
		ps -A -o ppid= | grep -qx " *$(tmux display-message -p -t "$PANE" '#{pane_pid}')" && break
		sleep 0.1
	done

	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	during="$(display_of)"
	echo "during: [$during]"
	[ "$(wc -l <"$TDIR/ps.log")" -eq 1 ]
	[ "$during" = "Z" ]
	[ "$(tmux show -v -t S @active_pane_icon)" = "Z" ]

	tmux respawn-pane -k -t "$PANE" -- sh -c 'sleep 30'
	wait_cmd sh || wait_cmd sleep
	run bash "$TDIR/update-icons.sh" S
	after="$(display_of)"
	echo "after: [$after]"
	[ "$after" != "J" ]
}

@test "idle just pane keeps the runner's own icon" {
	setup_server
	tmux respawn-pane -k -t "$PANE" -- "$TDIR/bin/just" -c 'read -r x'
	wait_cmd just
	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	[ "$(display_of)" = "J" ]
	[ "$(tmux show -v -t S @active_pane_icon)" = "J" ]
}
