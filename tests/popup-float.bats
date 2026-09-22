#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
bats_require_minimum_version 1.5.0 # run !
# Upstream tmux 34cd5da4 deletes popups; `display-popup` survives only as an
# undocumented compat command that opens a modal floating pane in the target
# window (#725, docs/agents/floats.md). This pins, against the real binary,
# that the config loads clean and that the launchers' assumptions hold:
# modal, remain-on-exit off, a second popup is a silent no-op, closing
# restores focus.
#
# Same attached-client harness as tests/float-tool-focus.bats: a key binding
# fires only for an attached client, so the outer server's pane runs a real
# `tmux attach`.

setup() {
	IN="pf725-in-${BATS_TEST_NUMBER}-$$"
	OUT="pf725-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# The poller and sweep hooks fire inside this test server, and the sweep
	# reaches functions that delete files under these dirs — whose defaults are
	# the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	# tmux takes default-shell from $SHELL, and the pane's command runs under it.
	SHELL="$(command -v bash)"
	export SHELL

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat the keypress.
	inner set-option -g @splash_shown 1
	attach_client
	PREFIX="$(inner show-options -gv prefix)"
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# A real attached client for the inner server: its keys come from a second
# server's pane, which is where a keypress can actually reach the key table.
attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	OPANE="$(outer list-panes -F '#{pane_id}' | head -1)"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
}

send() { outer send-keys -t "$OPANE" "$@"; }

press() { # key
	send "$PREFIX"
	sleep 0.2
	send "$1"
	sleep 0.5
}

dump() {
	inner list-panes -a -F '#{pane_id}|#{pane_floating_flag}|#{pane_modal_flag}|#{pane_active}|#{window_id}' >&2
}

# The pane id of the modal float in window WIN, or empty. Polls up to 5s: the
# popup's own process (a Go binary) needs a moment to start and render.
modal_float() { # win
	local win="$1" got deadline
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		got="$(inner list-panes -t "$win" -F '#{pane_id}|#{pane_modal_flag}' 2>/dev/null |
			awk -F'|' '$2 == "1" { print $1 }')"
		[ -n "$got" ] && {
			printf '%s' "$got"
			return 0
		}
		sleep 0.1
	done
	return 1
}

@test "1: config sources clean" {
	CONF="$(grep -m1 -o -- '-f [^[:space:]]*' "$TMUX_BIN" | cut -d' ' -f2)"
	[ -n "$CONF" ]
	run inner source-file "$CONF"
	[ "$status" -eq 0 ]
	# Not a blanket empty-output check: in the nix sandbox, four unrelated
	# nixpkgs plugins (better-mouse-mode, vim-tmux-navigator, tmux-fzf,
	# tmux-fingers) fail their own unpatched `#!/usr/bin/env bash` shebang with
	# "returned 127" whenever /usr/bin/env isn't on the sandbox's PATH — noise
	# on every config load, on every check, regardless of this file. What #725
	# actually broke is distinguishable: `invalid option: popup-border-lines`
	# (the option is gone upstream) and catppuccin's popup-style/-border-style
	# `set -gF` failing its plugin with "returned 1" (not 127) — see
	# docs/superpowers/specs/2026-09-21-popups-to-floats-design.md.
	[[ $output != *'invalid option'* ]]
	if [[ $output =~ returned\ 1([^0-9]|$) ]]; then
		echo "a plugin returned exit code 1 (not the pre-existing 127 sandbox noise): $output" >&2
		false
	fi
}

@test "2: prefix + n opens a modal float in the client's window, Escape closes it" {
	local win base float
	win="$(inner new-window -P -F '#{window_id}')"
	base="$(inner display-message -p -t "$win" '#{pane_id}')"

	press n

	float="$(modal_float "$win")" || {
		echo "prefix + n opened no modal float" >&2
		dump
		false
	}
	[ "$(inner display-message -p -t "$float" '#{pane_floating_flag}')" = 1 ]
	[ "$(inner show-options -pqv -t "$float" remain-on-exit)" = off ]

	send Escape
	sleep 0.5

	local remaining deadline
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		remaining="$(inner list-panes -t "$win" -F '#{pane_modal_flag}' | grep -c '^1' || true)"
		[ "$remaining" = 0 ] && break
		sleep 0.1
	done
	[ "$remaining" = 0 ] || {
		echo "float still open after Escape" >&2
		dump
		false
	}
	[ "$(inner display-message -p -t "$base" '#{pane_active}')" = 1 ]
}

@test "3: a second popup while one is open adds no pane" {
	local win float client count
	win="$(inner display-message -p -t s: '#{window_id}')"

	press n

	float="$(modal_float "$win")" || {
		echo "prefix + n opened no modal float" >&2
		dump
		false
	}

	client="$(inner list-clients -F '#{client_name}')"
	run inner display-popup -c "$client" -E 'sleep 30'
	[ "$status" -eq 0 ]

	count="$(inner list-panes -t "$win" -F '#{pane_floating_flag}' | grep -c '^1' || true)"
	[ "$count" = 1 ]
	[ "$(modal_float "$win")" = "$float" ]
}

@test "4: prefix + S reaches the scratch session" {
	press S

	local found deadline
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		found="$(inner list-clients -F '#{client_session}' | grep -Fx scratch-s || true)"
		[ -n "$found" ] && break
		sleep 0.2
	done
	[ -n "$found" ] || {
		echo "no client attached to scratch-s within 5s" >&2
		dump
		false
	}

	# Routed to the nested client by the float's all-keys capture: the modal
	# float itself is a `tmux attach` to scratch-s, so this detaches THAT
	# client rather than reaching the outer server's own prefix table.
	press d

	local remaining
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		remaining="$(inner list-panes -a -F '#{pane_floating_flag}' | grep -c '^1' || true)"
		[ "$remaining" = 0 ] && break
		sleep 0.2
	done
	[ "$remaining" = 0 ] || {
		echo "float still present after detach" >&2
		dump
		false
	}
}

@test "5: chrome rule at the tmux layer" {
	local win base float client visible eff

	win="$(inner display-message -p -t s: '#{window_id}')"
	base="$(inner display-message -p -t s: '#{pane_id}')"
	client="$(inner list-clients -F '#{client_name}')"

	inner display-popup -c "$client" -E 'sleep 30' &
	local popup_pid=$!

	float="$(modal_float "$win")" || {
		echo "background popup never opened a modal pane" >&2
		dump
		kill "$popup_pid" 2>/dev/null || true
		false
	}

	# NOT_MODAL: the enumerator filter omits the float and keeps the pane
	# under it.
	visible="$(inner list-panes -t "$win" -f '#{!:#{pane_modal_flag}}' -F '#{pane_id}')"
	printf '%s\n' "$visible" | grep -qxF "$base"
	if printf '%s\n' "$visible" | grep -qxF "$float"; then
		echo "NOT_MODAL still listed the float" >&2
		dump
		false
	fi

	# EFF_ACTIVE: the pane under the modal is marked "effectively active",
	# never the modal itself.
	eff="$(inner list-panes -t "$win" -F '#{pane_id}|#{?window_modal_pane,#{pane_last},#{pane_active}}' |
		awk -F'|' '$2 == "1" { print $1 }')"
	[ "$eff" = "$base" ]

	kill "$popup_pid" 2>/dev/null || true
	wait "$popup_pid" 2>/dev/null || true
}
