#!/usr/bin/env bats
# Status-bar colour roles (#1203). The built tmux-apply-theme-colors (with
# @picker_generate@ resolved, passed in $APPLY_SCRIPT) stamps @og_c_<name> from
# `picker_generate status-roles <flavor>` ($PICKER_GENERATE); every status
# format reads those options. Runs on a private, config-less tmux server.

# Every catppuccin name a role can resolve to; each gets a distinct hex.
THM_NAMES="bg fg crust mantle mauve green teal blue surface_1 peach red pink yellow sky maroon lavender
subtext_0 subtext_1 overlay_0 overlay_1 overlay_2"

# Mocha: the option each role resolved to before the indirection existed.
declare -gA MOCHA_OPT=(
	[dim]=overlay_1 [pr_closed]=overlay_0 [pr_pending]=peach [pr_success]=green
	[park]=overlay_0 [host_peach]=peach [host_teal]=teal [host_pink]=pink
	[host_yellow]=yellow [host_sky]=sky
)

declare -gA HEX

setup() {
	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$TDIR/data"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	export TMPDIR="$TDIR"
}

start_server() {
	command -v tmux >/dev/null || skip "tmux not on PATH"
	[[ -x ${PICKER_GENERATE:-} ]] || skip "PICKER_GENERATE not set"
	[[ -x ${APPLY_SCRIPT:-} ]] || skip "APPLY_SCRIPT not set"
	OG_TEST_ISOLATED=1
	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	local n i=16
	for n in $THM_NAMES; do
		tmux set -g "@thm_$n" "$(printf '#%02x%02x%02x' "$i" "$((i + 1))" "$((i + 2))")"
		HEX[$n]=$(tmux show -gv "@thm_$n")
		i=$((i + 7))
	done
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	tmux kill-server 2>/dev/null || true
}

@test "mocha: every role stamps the hex of its pre-indirection @thm_* option" {
	start_server
	tmux set -g @catppuccin_flavor mocha
	bash "$APPLY_SCRIPT"
	local name
	for name in "${!MOCHA_OPT[@]}"; do
		[ "$(tmux show -gv "@og_c_$name")" = "${HEX[${MOCHA_OPT[$name]}]}" ]
	done
	# No role beyond the ones pinned above.
	[ "$(tmux show -g | grep -c '^@og_c_')" -eq "${#MOCHA_OPT[@]}" ]
}

@test "latte: every role stamps a non-empty #rrggbb" {
	start_server
	tmux set -g @catppuccin_flavor latte
	bash "$APPLY_SCRIPT"
	local name
	for name in "${!MOCHA_OPT[@]}"; do
		[[ "$(tmux show -gv "@og_c_$name")" =~ ^#[0-9a-fA-F]{6}$ ]]
	done
}

@test "pane-border chrome takes the stamped dim colour" {
	start_server
	tmux set -g @catppuccin_flavor mocha
	bash "$APPLY_SCRIPT"
	[[ "$(tmux show -gwv pane-border-style)" == *"${HEX[overlay_1]}"* ]]
}

@test "every @og_c_<name> the formats, scripts and Go renderers reference is a status role" {
	[[ -x ${PICKER_GENERATE:-} ]] || skip "PICKER_GENERATE not set"
	local roles refs ref
	roles=$("$PICKER_GENERATE" status-roles mocha | awk '{print $1}')
	[ -n "$roles" ]
	refs=$(grep -ohE '@og_c_[a-z_]+' \
		config/tmux.conf.tmpl config/tmux.conf.reference.nix scripts/*.sh \
		generator/render/status.go picker/remotebridge/daemon/park.go | sed 's/^@og_c_//' | sort -u)
	[ -n "$refs" ]
	for ref in $refs; do
		# A trailing "_" is a shell-built name (host_$c): it must prefix a role.
		if [[ $ref == *_ ]]; then
			grep -q "^$ref" <<<"$roles"
		else
			grep -qx "$ref" <<<"$roles"
		fi
	done
}
