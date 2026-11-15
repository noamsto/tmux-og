#!/usr/bin/env bats
# bats runs teardown() after a setup() that skipped, so a teardown that kills
# the server its environment resolves to reaches the live server when setup
# skipped before isolating (a bare `tmux kill-server` follows TMUX, else
# TMUX_TMPDIR; a kill through an explicit -L/-S socket or TMUX_TMPDIR= is exempt). Every such teardown must open with the OG_TEST_ISOLATED guard,
# and setup must set the marker after its isolation and before its first skip
# (skips must sit directly in setup's body: one hidden in a helper is invisible
# here).

# shellcheck disable=SC2016  # the guard line is matched literally
GUARD='[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0'

body() { # <file> <function>: the function's lines, including a one-liner
	awk -v fn="$2" '
		index($0, fn "() {") == 1 { p = 1; print; if ($0 ~ /\}[[:space:]]*$/ && $0 !~ /\{[[:space:]]*$/) p = 0; next }
		p { print; if ($0 ~ /^}/) p = 0 }
	' "$1"
}

@test "env-resolved teardown kills sit behind the isolation guard" {
	local f bad=()
	for f in tests/*.bats; do
		[[ $f == tests/teardown-isolation.bats ]] && continue
		body "$f" teardown | grep -v '^[[:space:]]*#' | grep -E '(^|[[:space:]"$({])(tmux|TMUX_[B]IN|REAL_TMUX|TMUX_RAW)\}?"? .*kill-(server|session)' | grep -qvE ' -[LS] |TMUX_TMPDIR=' || continue
		[[ "$(body "$f" teardown | grep -v '^[[:space:]]*#' | sed -n '2p')" == *"$GUARD" ]] ||
			bad+=("$f: teardown does not open with the OG_TEST_ISOLATED guard")
	done
	if ((${#bad[@]})); then
		printf '%s\n' "${bad[@]}" >&2
		return 1
	fi
}

@test "setup sets the isolation marker before its first skip" {
	local f bad=() marker skip
	for f in tests/*.bats; do
		[[ $f == tests/teardown-isolation.bats ]] && continue
		grep -qF "$GUARD" "$f" || continue
		marker=$(body "$f" setup | grep -n '^[[:space:]]*OG_TEST_ISOLATED=1$' | head -1 | cut -d: -f1)
		unset_line=$(body "$f" setup | grep -n '^[[:space:]]*unset TMUX' | head -1 | cut -d: -f1)
		tmpdir_line=$(body "$f" setup | grep -n 'TMUX_TMPDIR=' | head -1 | cut -d: -f1)
		skip=$(body "$f" setup | grep -n '\bskip\b' | head -1 | cut -d: -f1)
		[[ -n $marker ]] || {
			bad+=("$f: setup never sets OG_TEST_ISOLATED=1")
			continue
		}
		[[ -n $unset_line && -n $tmpdir_line ]] && ((unset_line < marker && tmpdir_line < marker)) ||
			{
				bad+=("$f: setup sets OG_TEST_ISOLATED=1 before unsetting TMUX / exporting TMUX_TMPDIR")
				continue
			}
		[[ -z $skip ]] || ((marker < skip)) || bad+=("$f: setup skips before setting OG_TEST_ISOLATED=1")
	done
	if ((${#bad[@]})); then
		printf '%s\n' "${bad[@]}" >&2
		return 1
	fi
}
