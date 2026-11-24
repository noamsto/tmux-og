#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031,SC2016 # bats @test blocks run in subshells; export is intentional; '#(' in single quotes is the hostile payload under test
# `og project status` end to end: the shim, og-project-bin, the probe script run by a
# real sh, and the Go merge/render. ssh, tmux, zoxide and tailscale are fakes first
# on PATH; no tmux server is started and nothing here touches a real host.

bats_require_minimum_version 1.5.0 # run --separate-stderr

setup() {
	# The shim falls back to an og-project-bin on PATH, which would test an installed binary.
	[[ -n ${OG_PROJECT_BIN:-} ]] || {
		echo "OG_PROJECT_BIN must name the branch's og-project-bin" >&2
		return 1
	}
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	# Resolved physically so git's absolute common dir equals the path we declare.
	TMPD=$(cd -P "$BATS_TEST_TMPDIR" && pwd)
	export FIX="$TMPD/fix"
	mkdir -p "$FIX"
	export SSH_LOG="$TMPD/ssh.log"
	export TS_MARKER="$TMPD/tailscale.ran"

	export HOME="$TMPD/home"
	export XDG_CONFIG_HOME="$TMPD/config"
	export XDG_DATA_HOME="$TMPD/data"
	export TMUX_TMPDIR="$TMPD/tmux"
	export GIT_CONFIG_NOSYSTEM=1
	export GIT_CONFIG_GLOBAL=/dev/null
	export GIT_TERMINAL_PROMPT=0
	mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$TMUX_TMPDIR"
	unset TMUX TMUX_OG_PROJECTS

	SHIM="$BATS_TEST_DIRNAME/../scripts/og-project.sh"
	SHA_A=$(printf '%040d' 0 | tr 0 a)
	SHA_B=$(printf '%040d' 0 | tr 0 b)
	SHA_C=$(printf '%040d' 0 | tr 0 c)
	SHA_D=$(printf '%040d' 0 | tr 0 d)

	# The host is the word before `--`; a probe command and a heads command are
	# told apart by `ls-remote`, which only HeadsScript holds. A host with no
	# fixture file refuses the connection the way ssh does.
	cat >"$FAKEBIN/ssh" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$SSH_LOG"
		if [ "$1" = -G ]; then
			printf 'hostname %s\n' "$2"
			exit 0
		fi
		host=
		cmd=
		while [ $# -gt 0 ]; do
			case $1 in
			--) cmd=$2; break ;;
			*) host=$1; shift ;;
			esac
		done
		case $cmd in
		*ls-remote*) f="$FIX/$host.heads" ;;
		*) f="$FIX/$host.probe" ;;
		esac
		if [ ! -f "$f" ]; then
			echo "ssh: connect to host $host port 22: Connection refused" >&2
			exit 255
		fi
		cat "$f"
	EOF
	# Silent tmux and zoxide: the local probe finds nothing on its own, so a
	# developer machine's own checkouts never reach the asserted output.
	printf '#!/bin/sh\nexit 0\n' >"$FAKEBIN/tmux"
	printf '#!/bin/sh\nexit 0\n' >"$FAKEBIN/zoxide"
	cat >"$FAKEBIN/tailscale" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$TS_MARKER"
		echo "failed to connect to local tailscaled" >&2
		exit 1
	EOF
	chmod +x "$FAKEBIN"/*
	export PATH="$FAKEBIN:$PATH"
}

# registry JSON on stdin -> $TMUX_OG_PROJECTS
registry() {
	cat >"$TMPD/projects.json"
	export TMUX_OG_PROJECTS="$TMPD/projects.json"
}

# mkrepo <name>: a repo on main with origin https://github.com/og-fixture/<name>.git
mkrepo() {
	local d="$TMPD/repos/$1"
	git init -q -b main "$d"
	git -C "$d" config user.email t@t
	git -C "$d" config user.name t
	git -C "$d" config commit.gpgsign false
	git -C "$d" remote add origin "https://github.com/og-fixture/$1.git"
	git -C "$d" commit -q --allow-empty -m one
}

@test "status prints both hosts, one project key and a duplicate branch" {
	registry <<-'EOF'
		{"bridgeHosts": ["lab", "argon"]}
	EOF
	cat >"$FIX/lab.probe" <<-EOF
		I|id-lab|alice
		R|/srv/widget/.git|main|https://github.com/acme/widget.git
		W|/srv/widget/.git|$SHA_A|/srv/widget|main
		W|/srv/widget/.git|$SHA_A|/srv/widget-feat|feat/x
		D|/srv/widget/.git|0|2|$SHA_B
		T||@3|/srv/widget-feat|work
	EOF
	# origin's true head is not the host's last fetch.
	echo "H|/srv/widget|$SHA_C" >"$FIX/lab.heads"
	# Same repo under another origin spelling; no heads file, so its head stays unchecked.
	cat >"$FIX/argon.probe" <<-EOF
		I|id-argon|bob
		R|/opt/widget/.git|main|git@github.com:acme/widget.git
		W|/opt/widget/.git|$SHA_A|/opt/widget|main
		W|/opt/widget/.git|$SHA_D|/opt/widget-x|feat/x
		D|/opt/widget/.git|||
	EOF

	run bash "$SHIM" status github.com/acme/widget
	[ "$status" -eq 0 ]

	echo "$output" | grep -Eq '^  local  +reachable$'
	echo "$output" | grep -Eq '^  lab  +reachable$'
	echo "$output" | grep -Eq '^  argon  +reachable$'
	# Exactly one project header, hosts under it.
	[ "$(echo "$output" | grep -Ev '^( |$|Hosts$)')" = "github.com/acme/widget" ]
	echo "$output" | grep -Eq '^  lab  +/srv/widget  +2 worktrees  +main ↓2 · origin moved since last fetch$'
	echo "$output" | grep -Eq '^  argon  +/opt/widget  +2 worktrees  +main \? · remote head unchecked$'
	echo "$output" | grep -Fxq '  ⚠ feat/x is checked out on lab and argon (heads differ)'
	echo "$output" | grep -Fxq '  ⚠ main is checked out on lab and argon'
}

@test "status never prints hostile remote strings unsanitised" {
	registry <<-'EOF'
		{"bridgeHosts": ["lab", "evil"]}
	EOF
	hostile=$'\e[31m#(touch pwned)\'q\'"d"a|b\nline2\e]0;title\a'
	cat >"$FIX/lab.probe" <<-EOF
		I|id-lab|alice
		R|/srv/widget/.git|main|https://github.com/acme/widget.git
		W|/srv/widget/.git|$SHA_A|/srv/widget|main
		D|/srv/widget/.git|0|0|$SHA_A
	EOF
	{
		printf 'I|%s|u\n' "$hostile"
		printf 'I|id-evil|mallory\n'
		printf 'R|/srv/widget/.git|main|https://github.com/acme/widget.git\n'
		printf 'W|/srv/widget/.git|%s|/srv/widget|main\n' "$SHA_A"
		printf 'W|/srv/widget/.git|%s|/srv/widget-bad|feat%s\n' "$SHA_A" "$hostile"
		printf 'W|/srv/widget/.git|%s|/srv/widget%s|dev\n' "$SHA_A" "$hostile"
		printf 'R|/srv/evil/.git|main|%s\n' "$hostile"
		printf 'T||@1|/srv/widget|%s\n' "$hostile"
		printf 'D|/srv/widget/.git|1|1|zz%s\n' "$hostile"
		printf 'X|%s\n' "$hostile"
	} >"$FIX/evil.probe"

	run --separate-stderr bash "$SHIM" status github.com/acme/widget
	[ "$status" -eq 0 ]
	all="$output
$stderr"

	# The safe parts of the same answers still print.
	echo "$output" | grep -Eq '^  evil  +reachable \(13 unsafe rows skipped\)$'
	# widget-bad survives its hostile branch, shown without one.
	echo "$output" | grep -Eq '^  evil  +/srv/widget  +2 worktrees'
	echo "$output" | grep -Fxq 'github.com/acme/widget'
	[[ $all != *$'\e'* ]]
	[[ $all != *'#('* ]]
	[[ $all != *"'"* ]]
	[[ $all != *'"'* ]]
	[[ $all != *'|'* ]]
	[ "$(printf '%s' "$all" | LC_ALL=C grep -c '[[:cntrl:]]')" -eq 0 ]
}

@test "status does not exec tailscale when the source is disabled" {
	registry <<-'EOF'
		{"bridgeHosts": ["lab"], "tailscale": false}
	EOF
	cat >"$FIX/lab.probe" <<-EOF
		I|id-lab|alice
		R|/srv/widget/.git|main|https://github.com/acme/widget.git
		W|/srv/widget/.git|$SHA_A|/srv/widget|main
		D|/srv/widget/.git|0|0|$SHA_A
	EOF

	run bash "$SHIM" status github.com/acme/widget
	[ "$status" -eq 0 ]
	echo "$output" | grep -Eq '^  lab  +reachable$'
	[ ! -e "$TS_MARKER" ]
}

@test "status with a failing tailscale yields no source and still succeeds" {
	registry <<-'EOF'
		{"bridgeHosts": ["lab"], "tailscale": true}
	EOF
	cat >"$FIX/lab.probe" <<-EOF
		I|id-lab|alice
		R|/srv/widget/.git|main|https://github.com/acme/widget.git
		W|/srv/widget/.git|$SHA_A|/srv/widget|main
		D|/srv/widget/.git|0|0|$SHA_A
	EOF

	run --separate-stderr bash "$SHIM" status github.com/acme/widget
	[ "$status" -eq 0 ]
	# It was asked, and its failure is not an error.
	grep -Fxq 'status --json' "$TS_MARKER"
	echo "$output" | grep -Eq '^  lab  +reachable$'
	[[ $output != *"Suggested hosts"* ]]
	[[ $output != *"Tailscale reports"* ]]
	[ -z "$stderr" ]
}

@test "status reports an unreachable host" {
	registry <<-'EOF'
		{"bridgeHosts": ["lab", "down"]}
	EOF
	cat >"$FIX/lab.probe" <<-EOF
		I|id-lab|alice
		R|/srv/widget/.git|main|https://github.com/acme/widget.git
		W|/srv/widget/.git|$SHA_A|/srv/widget|main
		D|/srv/widget/.git|0|0|$SHA_A
	EOF

	run bash "$SHIM" status github.com/acme/widget
	[ "$status" -eq 0 ]
	echo "$output" | grep -Eq '^  lab  +reachable$'
	echo "$output" | grep -Eq '^  down  +unreachable — '
	# The dead host has no checkouts to list under the project.
	[ "$(echo "$output" | grep -Ec '^  down  +/')" -eq 0 ]
}

@test "local probe finds checkouts from tmux windows, zoxide and registry paths" {
	mkrepo repo-a
	mkrepo repo-b
	mkrepo repo-c
	export REPO_A="$TMPD/repos/repo-a" REPO_B="$TMPD/repos/repo-b" REPO_C="$TMPD/repos/repo-c"
	# repo-a's origin/main is one commit behind its main.
	git -C "$REPO_A" commit -q --allow-empty -m two
	git -C "$REPO_A" update-ref refs/remotes/origin/main HEAD~1

	# An unmirrored window (empty @bridge_win) whose @git_root is repo-a.
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		printf '|@1|%s|work\n' "$REPO_A"
	EOF
	cat >"$FAKEBIN/zoxide" <<-'EOF'
		#!/bin/sh
		[ "$1 $2" = "query -l" ] && printf '%s\n' "$REPO_B"
		exit 0
	EOF
	registry <<-EOF
		{"projects": {"gamma": {"remote": "github.com/og-fixture/repo-c", "hosts": {"local": {"path": "$REPO_C"}}}}}
	EOF
	# No network: ls-remote against these origins is refused up front, so the
	# remote head stays unchecked on a dev machine and in the nix sandbox alike.
	export GIT_ALLOW_PROTOCOL=file

	run bash "$SHIM" status
	[ "$status" -eq 0 ]
	echo "$output" | grep -Eq '^  local  +reachable$'
	echo "$output" | grep -Fxq 'github.com/og-fixture/repo-a'
	echo "$output" | grep -Fxq 'github.com/og-fixture/repo-b'
	echo "$output" | grep -Fxq 'github.com/og-fixture/repo-c (gamma)'
	[ "$(echo "$output" | grep -Evc '^( |$|Hosts$)')" -eq 3 ]
	echo "$output" | grep -Eq "^  local  +$REPO_A  +1 worktree  +main ↑1"
	[ "$(echo "$output" | grep -c 'main ↑1')" -eq 1 ]
	# Local is probed with a local sh, never ssh.
	[ ! -s "$SSH_LOG" ]
}
