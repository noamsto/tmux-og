#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells
# #1136: a task runner (just/make) runs its recipe in its own process group, so
# pane_current_command names the runner for the whole recipe. The resolution
# rule is exercised on a stubbed process table; the tmux-update-icons wiring
# lives in runner-child-wiring.bats (a scratch server).

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
