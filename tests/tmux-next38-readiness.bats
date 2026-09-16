#!/usr/bin/env bats
# Headless readiness smoke for the shipped next-3.8 tmux wrapper.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	SOCKET="og-next38-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# The poller and sweep monitor hooks fire inside this test server, and the
	# sweep reaches two functions that delete files under these dirs — whose
	# defaults are the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	t new-session -d -s s -c "$PWD"
	wait_for_nonempty_option @thm_bg
}

teardown() {
	t kill-server 2>/dev/null || true
}

t() {
	timeout --foreground 30s "$TMUX_BIN" -L "$SOCKET" "$@"
	local status=$?
	if [[ $status -eq 124 ]]; then
		printf 'tmux invocation timed out after 30s:' >&2
		printf ' %q' "$TMUX_BIN" -L "$SOCKET" "$@" >&2
		printf '\n' >&2
	fi
	return "$status"
}

wait_for_nonempty_option() {
	local option=$1
	local got
	local i
	for i in {1..50}; do
		got="$(t show-options -gqv "$option" 2>/dev/null || true)"
		if [[ -n $got ]]; then
			return 0
		fi
		sleep 0.1
	done
	printf 'timed out waiting for non-empty %s\n' "$option" >&2
	return 1
}

wait_for_option() {
	local option=$1
	local want=$2
	local got
	local i
	for i in {1..50}; do
		got="$(t show-options -t s -qv "$option" 2>/dev/null || true)"
		[[ $got == "$want" ]] && return 0
		# @reflow_key "N::H" means the width probe was empty — fail loud (#235).
		if [[ $option == @reflow_key && $got =~ ^[0-9]+:: ]]; then
			printf 'empty width in %s=%s (want %s); width probe was empty\n' \
				"$option" "$got" "$want" >&2
			return 1
		fi
		sleep 0.1
	done
	printf 'timed out waiting for %s=%s, last=%s\n' "$option" "$want" "$got" >&2
	return 1
}

store_conf() {
	grep -o -- '-f /nix/store/[a-z0-9]*-tmux[.]conf' "$TMUX_BIN" | head -1 | cut -d' ' -f2
}

store_path() {
	local pattern=$1
	grep -o "$pattern" "$(store_conf)" | head -1
}

embedded_path() {
	local file=$1
	local pattern=$2
	grep -o "$pattern" "$file" | head -1
}

make_tmux_shim() {
	SHIM_DIR="$BATS_TEST_TMPDIR/shim"
	mkdir -p "$SHIM_DIR"
	cat >"$SHIM_DIR/tmux" <<EOF
#!$(command -v bash)
exec "$TMUX_BIN" -L "$SOCKET" "\$@"
EOF
	chmod +x "$SHIM_DIR/tmux"
}

strip_styles() {
	sed 's/#[[][^]]*[]]//g'
}

wait_for_client() {
	local i
	for i in {1..30}; do
		[[ "$(t list-clients -t s 2>/dev/null | wc -l)" -gt 0 ]] && return 0
		sleep 0.1
	done
	return 1
}

@test "wrapper runs the pinned upstream tmux and catppuccin renders theme variables" {
	run t -V
	[ "$status" -eq 0 ]
	[[ $output == *"next-3.9"* ]]

	thm_bg="$(t show -gv @thm_bg)"
	thm_mauve="$(t show -gv @thm_mauve)"
	[[ $thm_bg =~ ^#[0-9a-fA-F]{6}$ ]]
	[[ $thm_mauve =~ ^#[0-9a-fA-F]{6}$ ]]
}

@test "status-format 0-4 parse and the W loop expands after reflow" {
	t set-option -t s:1 -w @branch "feat/next38-one"
	for idx in 2 3 4 5 6 7 8 9 10; do
		t new-window -t s -c "$PWD"
		t set-option -t "s:$idx" -w @branch "feat/next38-window-$idx-with-extra-label-width"
	done

	make_tmux_shim
	reflow="$(store_path '/nix/store/[[:alnum:]]*-tmux-reflow-windows/bin/tmux-reflow-windows')"
	coproc REFLOW_CTL { "$TMUX_BIN" -L "$SOCKET" -C attach-session -t s; }
	wait_for_client
	PATH="$SHIM_DIR:$PATH" "$reflow" s 36 --force
	# Keep the control client attached until the key is observed — detach first
	# used to race backgrounded empty-width reflows over the good stamp (#235).
	# Height trails the width in the key; a control client contributes no size, so
	# it resolves to 0 and the row cap stays at its 3-row baseline.
	wait_for_option @reflow_key "10:36:0"
	printf 'detach-client\n' >&"${REFLOW_CTL[1]}" || true
	kill "$REFLOW_CTL_PID" 2>/dev/null || true

	[ "$(t show-options -t s -qv status)" -ge 3 ]
	[ "$(t show-options -t s -qv @window_split)" != "999" ]
	[ "$(t show-options -t s -qv @window_per)" -lt 10 ]
	# Row 4 exists but stays unreached at the 3-row cap.
	[ "$(t show-options -t s -qv @window_split3)" = "999" ]

	for i in 0 1 2 3; do
		fmt="$(t show -gv "status-format[$i]")"
		t display-message -p -F "$fmt" >/dev/null
	done
	# Reflow's own session-level rows, including the row-4 format it now writes.
	for i in 1 2 3 4; do
		fmt="$(t show-options -t s -qv "status-format[$i]")"
		t display-message -p -F "$fmt" >/dev/null
	done

	line1="$(t display-message -p -F "$(t show -gv status-format[1])" | strip_styles)"
	line2="$(t display-message -p -F "$(t show -gv status-format[2])" | strip_styles)"
	[[ $line1 == *"1:"* ]]
	[[ $line1 == *"next38"* || $line2 == *"next38"* ]]
}

@test "popup bindings and picker tmux data path are present" {
	keys="$(t list-keys -T prefix)"
	[[ $keys == *"tmux-session-picker"* ]]
	[[ $keys == *"tmux-window-picker"* ]]
	[[ $keys == *"display-popup"* && $keys == *"tmux-enrich-card"* ]]

	session_picker="$(store_path '/nix/store/[[:alnum:]]*-tmux-session-picker/bin/tmux-session-picker')"
	picker="$(embedded_path "$session_picker" '/nix/store/[[:alnum:]]*-tmux-og-go-tools-[^[:space:]]*/bin/tmux-picker-generate')"
	[[ -x $picker ]]

	make_tmux_shim

	run env PATH="$SHIM_DIR:$PATH" tmux list-sessions -F '#{session_name}'
	[ "$status" -eq 0 ]
	[[ $output == *"s"* ]]

	run env PATH="$SHIM_DIR:$PATH" tmux list-windows -a -F '#{session_name}:#{window_index}'
	[ "$status" -eq 0 ]
	[[ $output == *"s"* ]]
}

# A popup for a control client used to open and take the whole remote server
# down with it (#346); upstream af3e4d2 makes display-popup a silent no-op for
# such a client instead.
@test "display-popup is refused for an attached control client" {
	marker="$BATS_TEST_TMPDIR/popup-ran"
	sentinel="$BATS_TEST_TMPDIR/sentinel-ran"
	coproc CTL { "$TMUX_BIN" -L "$SOCKET" -C attach-session -t s; }

	wait_for_client

	printf 'display-popup -E "printf popup-ok > %q"\n' "$marker" >&"${CTL[1]}"
	# The refusal is silent, so a missing marker alone would also pass on a
	# command that never arrived. This one does reach a control client.
	printf 'run-shell "printf sentinel-ok > %q"\n' "$sentinel" >&"${CTL[1]}"
	for _ in {1..30}; do
		[[ -f $sentinel ]] && break
		sleep 0.1
	done
	printf 'detach-client\n' >&"${CTL[1]}" || true
	kill "$CTL_PID" 2>/dev/null || true

	[ "$(cat "$sentinel")" = "sentinel-ok" ]
	[ ! -f "$marker" ]
}

# === Remote bridge structural-input gate (M2.3) ===
#
# The bridge daemon's own integration tests run on vanilla `tmux -L` servers with
# no tmux-og keybindings, so they cannot see the gate at all. These cases run
# against the WRAPPED tmux — the real generated config — which is the only place
# in CI where the gate and the rebound defaults exist.

# The gate is a pure format, so its truth table is checkable without pressing a
# key: it must be true only where the daemon stamped BOTH carriers. #{&&:...}
# yields "1"/"0", and if-shell -F treats "0" (like "") as false.
@test "bridge gate is true only for a tagged window with a stamped pane" {
	gate='#{&&:#{@bridge_win},#{@bridge_pane}}'

	# A normal window: neither carrier set.
	[ "$(t display-message -p -t s:1 -F "$gate")" = 0 ]

	t new-window -d -t s: -n bridgey
	t set-option -w -t s:bridgey @bridge_win 1
	# Window tagged but the pane not stamped — a pane the daemon does not own must
	# still fall through to the local action.
	[ "$(t display-message -p -t s:bridgey -F "$gate")" = 0 ]

	pane="$(t list-panes -t s:bridgey -F '#{pane_id}' | head -1)"
	t set-option -p -t "$pane" @bridge_pane '%42'
	[ "$(t display-message -p -t s:bridgey -F "$gate")" = 1 ]

	# Still false in the untagged window, i.e. the pane option did not leak.
	[ "$(t display-message -p -t s:1 -F "$gate")" = 0 ]
}

# M2.3 made the config own three keys tmux used to own (`,`, `{`, `}`). Their
# non-bridge behavior must stay byte-identical to next-3.8's default, so assert
# the else-branch command and the -N note against the pinned upstream text. This
# turns a hand transcription into a claim that fails loudly on the next tmux bump.
@test "keys the bridge gate newly owns keep their upstream default and note" {
	run t list-keys -T prefix ,
	[ "$status" -eq 0 ]
	# else-branch is tmux's default rename prompt, seeded from #W. Compared in the
	# form list-keys itself prints (tmux normalises the `--` away), which is also
	# the form a future tmux bump would change.
	[[ $output == *'{ command-prompt -I "#W" { rename-window "%%" } }'* ]]

	run t list-keys -T prefix '{'
	[ "$status" -eq 0 ]
	[[ $output == *'swap-pane -U'* ]]

	run t list-keys -T prefix '}'
	[ "$status" -eq 0 ]
	[[ $output == *'swap-pane -D'* ]]

	# The -N notes feed which-key, and rebinding a default drops the note unless
	# it is re-supplied.
	run t list-keys -N -T prefix
	[ "$status" -eq 0 ]
	[[ $output == *'Rename current window'* ]]
	[[ $output == *'Swap the active pane with the pane above'* ]]
	[[ $output == *'Swap the active pane with the pane below'* ]]
}

# Pins the mirror branch's '{ ... }' block form. Its trailing %1 is a template
# substitution done with NO escaping at all (NQ) — safe only because a
# '{ ... }' block is parsed once (ARGS_COMMANDS) and never re-lexed. A later
# rewrite of the same bind into a quoted-string command argument would re-lex
# the printed text and turn %1 into raw injection, and the conf-shell-quoting
# scanner cannot see that change (it scans source text, not tmux's own
# parse). list-keys can: it prints ARGS_COMMANDS as '{ ... }' and ARGS_STRING
# as a quoted string, so a block-to-string rewrite is visible here.
@test "prefix + , mirror branch keeps the { ... } block form with #{qs:1} and a trailing %1" {
	run t list-keys -T prefix ,
	[ "$status" -eq 0 ]
	# list-keys does not escape '#' in printed args (see the '-I "#W"'
	# expectation above), so plain substring matching works.
	[[ $output == *'{ command-prompt -I "#{@window_bridge_name}" { run-shell "'*'rename #{q:@bridge_pane} #{qs:1}" "%1" } }'* ]]
}

# The tmux-level half of the inverse the whole '#'-handling fix depends on:
# X(E(r)) == S(r), where X is tmux's own rename-window format expansion and
# E/S are sanitizeWindowName/stripWindowName. Expected strings are hardcoded
# from the plan's fixture table; windows_test.go's TestWindowNameFixtures pins
# the same E(r)/S(r) strings against the real Go functions, so the pair
# together — not either alone — proves Go and tmux agree.
#
# Rows 6, 8 and 10 of the fixture table have an empty E(r) (nothing to rename
# to) and are excluded here. No row has an unterminated '#[' in E(r) either:
# the sanitizer drops it while building E, so it can never survive into a
# rename target.
@test "tmux rename-window of E(r) yields S(r) for every fixture row with a non-empty E" {
	t new-window -d -t s: -n scratch367
	wid="$(t list-windows -t s -F '#{window_id} #{window_name}' | grep scratch367 | awk '{print $1}')"

	# shellcheck disable=SC2088 # literal remote window names, not paths to expand
	local -a e_vals=(
		'pr##367'
		'a####b'
		'plain-name'
		'x'
		'a##b'
		'ab'
		'abc'
		"it's"
		'~/src'
		'[nix-amd-ai 🧠 󰪣 󰘭 ##46]'
	)
	# shellcheck disable=SC2088 # literal remote window names, not paths to expand
	local -a s_vals=(
		'pr#367'
		'a##b'
		'plain-name'
		'x'
		'a#b'
		'ab'
		'abc'
		"it's"
		'~/src'
		'[nix-amd-ai 🧠 󰪣 󰘭 #46]'
	)

	local i got
	for i in "${!e_vals[@]}"; do
		t rename-window -t "$wid" -- "${e_vals[$i]}"
		got="$(t display-message -p -t "$wid" '#{window_name}')"
		[ "$got" = "${s_vals[$i]}" ]
	done
}

# Every gated key must still carry its original local behavior in the else
# branch: a regression here is a regression for every non-bridge window.
@test "gated keys keep their local behavior in the else branch" {
	run t list-keys -T prefix '|'
	[[ $output == *'split-window -h'* ]]
	[[ $output == *'pane_current_path'* ]]

	run t list-keys -T prefix _
	[[ $output == *'split-window -v'* ]]

	# c keeps the scratchpad guard.
	run t list-keys -T prefix c
	[[ $output == *'scratch-'* ]]
	[[ $output == *'new-window -c'* ]]

	# x keeps the claude-status kill guard.
	run t list-keys -T prefix x
	[[ $output == *'tmux-kill-pane-guard'* ]]
	[[ $output == *'confirm-before'* ]]

	# & keeps its confirmation.
	run t list-keys -T prefix '&'
	[[ $output == *'kill-window'* ]]
	[[ $output == *'confirm-before'* ]]

	# The four resize binds keep -r (repeatable) and their local resize-pane.
	local key
	for key in M-Up M-Down M-Left M-Right; do
		run t list-keys -T prefix "$key"
		[ "$status" -eq 0 ]
		[[ $output == *' -r '* ]]
		[[ $output == *'resize-pane -'* ]]
	done
}

# The focus hook must be registered, and `prefix + r` must stay idempotent: the
# bare `set-hook -gu after-select-pane` clear has to exist, or a reload would
# stack hooks pointing at dead store paths.
@test "after-select-pane focus hook is set and reload-idempotent" {
	run t show-hooks -g
	[ "$status" -eq 0 ]
	[[ $output == *'after-select-pane[20]'* ]]
	[[ $output == *'og-remote-bridge-ctl'* ]]

	# prefix + r re-sources the generated config, so re-sourcing must not stack a
	# second hook. Source the wrapper's own config rather than a user symlink,
	# which does not exist in the sandbox.
	before="$(t show-hooks -g | grep -c 'after-select-pane')"
	t source-file "$(store_conf)"
	after="$(t show-hooks -g | grep -c 'after-select-pane')"
	[ "$before" -eq "$after" ]

	# The clear that makes that true has to be in the config, not incidental.
	grep -q 'set-hook -gu after-select-pane' "$(store_conf)"
}

# update-environment appends must not stack on reload — the bare `set -gu`
# reverts to tmux defaults, then the seven `-ga` lines reapply our additions.
@test "update-environment appends are reload-idempotent" {
	grep -q 'set -gu update-environment' "$(store_conf)"

	before="$(t show-options -g update-environment | wc -l)"
	t source-file "$(store_conf)"
	t source-file "$(store_conf)"
	after="$(t show-options -g update-environment | wc -l)"
	[ "$before" -eq "$after" ]
}

# Guards the general defect class behind #341 (a `set-hook -g` that tmux
# silently discards, e.g. `pane-exited` on the pinned tmux), not just that one
# hook name: every hook the generated config registers must actually be
# stored, or reload would look fine while the hook quietly never fires.
@test "every hook the config registers with set-hook -g is actually stored by tmux" {
	run t show-hooks -g
	[ "$status" -eq 0 ]
	stored="$output"
	# A window-scoped hook (window-resized) lands in the global *window* table
	# even when set with a bare -g — tmux routes by the option's own table, not
	# by the flag. Both tables have to be searched, or such a hook goes
	# unexamined here while looking covered.
	run t show-hooks -gw
	[ "$status" -eq 0 ]
	stored="$stored"$'\n'"$output"

	local conf hooks name
	conf="$(store_conf)"
	# Anchored to a leading letter: `set-hook -g -B '@…'` and `set-hook -g -u -B
	# '@…'` otherwise feed the unanchored class its own flag tokens (-B, -u) as
	# if they were hook names, which show-hooks -g then predictably never lists.
	hooks="$(grep -v -E '^\s*#' "$conf" | grep -oE 'set-hook -g ([A-Za-z][A-Za-z-]*(\[[0-9]+\])?)' | awk '{print $3}' | sort -u)"
	[ -n "$hooks" ]

	while IFS= read -r name; do
		[[ -n $name ]] || continue
		if [[ $stored != *"$name"* ]]; then
			printf 'hook %s registered in config but not stored by tmux (silent set-hook no-op)\n' "$name" >&2
			return 1
		fi
	done <<<"$hooks"

	# The anchor above means a monitor hook (`set-hook -g -B '@name::…'`) is
	# never extracted above -- its name sits behind the -B flag token, not
	# after a bare `set-hook -g`. `show-hooks -g` wouldn't help either: it
	# prints a monitor's command alone, with no indication it is a monitor at
	# all. `show-hooks -g -B` is the one listing form that reports the
	# subscription itself, so monitor hooks are checked against it separately.
	# Each name is checked only if #603's tick floor has actually landed it in
	# this config, so this assertion is correct whether or not it has yet.
	run t show-hooks -g -B
	[ "$status" -eq 0 ]
	local stored_monitors="$output" monitor_name
	for monitor_name in @og-pr-tick @og-backfill-tick @og-usage-tick @og-sweep-tick; do
		grep -qF "'${monitor_name}::" "$conf" || continue
		if [[ $stored_monitors != *"$monitor_name"* ]]; then
			printf 'monitor hook %s registered in config but not stored by tmux (show-hooks -g -B)\n' "$monitor_name" >&2
			return 1
		fi
	done
}

@test "single-row separator omits only after the last window (next_window_index-driven)" {
	for _ in 1 2 3; do
		t new-window -t s -c "$PWD"
	done

	# No client ever attaches in this test, so tmux-reflow-windows's width probe
	# stays empty and it skips (see its own early-exit); status-format[1] stays
	# the global, un-reflowed fallback from config/tmux.conf.nix — the pure
	# "last iteration of the W: loop" case, no @window_split filtering involved.
	fmt="$(t show -gv status-format[1])"
	line="$(t display-message -p -F "$fmt" | strip_styles)"

	sep_count="$(grep -o '│' <<<"$line" | wc -l)"
	[ "$sep_count" -eq 3 ]
	[[ $line == *"4:"* ]]
}

@test "multi-row separators respect row boundaries and omit only at each row's last window" {
	t set-option -t s:1 -w @branch "feat/next38-one"
	for idx in 2 3 4 5 6 7 8 9 10; do
		t new-window -t s -c "$PWD"
		t set-option -t "s:$idx" -w @branch "feat/next38-window-$idx-with-extra-label-width"
	done

	make_tmux_shim
	reflow="$(store_path '/nix/store/[[:alnum:]]*-tmux-reflow-windows/bin/tmux-reflow-windows')"
	coproc REFLOW_CTL { "$TMUX_BIN" -L "$SOCKET" -C attach-session -t s; }
	wait_for_client
	PATH="$SHIM_DIR:$PATH" "$reflow" s 36 --force
	wait_for_option @reflow_key "10:36:0"
	printf 'detach-client\n' >&"${REFLOW_CTL[1]}" || true
	kill "$REFLOW_CTL_PID" 2>/dev/null || true

	split1="$(t show-options -t s -qv @window_split)"
	split2="$(t show-options -t s -qv @window_split2)"
	split3="$(t show-options -t s -qv @window_split3)"

	total_windows=10
	prev_split=0
	for idx in 1 2 3; do
		case $idx in
		1) split=$split1 ;;
		2) split=$split2 ;;
		3) split=$split3 ;;
		esac
		[ "$split" = 999 ] && split=$total_windows
		row_windows=$((split > prev_split ? split - prev_split : 0))
		if ((row_windows == 0)); then
			prev_split=$split
			continue
		fi
		line="$(t display-message -p -F "$(t show-options -t s -qv "status-format[$idx]")" | strip_styles)"
		row_sep="$(grep -o '│' <<<"$line" | wc -l)"
		[ "$row_sep" -eq "$((row_windows - 1))" ]
		prev_split=$split
	done
	[ "$prev_split" -eq "$total_windows" ]
}

@test "focus-follows-mouse, copy-mode-line-numbers, and the ? list-keys rebind are wired" {
	[ "$(t show-options -g -qv focus-follows-mouse)" = "off" ]
	[ "$(t show-options -g -qv copy-mode-line-numbers)" = "off" ]

	run t list-keys -T prefix '?'
	[ "$status" -eq 0 ]
	# tmux normalizes flag order in its own list-keys echo (-N -F "..." -O key),
	# so check each flag/value independently rather than the literal source order.
	[[ $output == *'list-keys -N'* ]]
	[[ $output == *'-F "#{key_table}: #{key_prefix}#{key_string}'* ]]
	[[ $output == *'-O key'* ]]
}

# tmux 3.8 added auto-hide as a pane-scrollbars CHOICE value (replacing the
# old modal narrowing behaviour, #649): pin that the built config actually
# ships it.
@test "pane-scrollbars is auto-hide" {
	[ "$(t show-options -g -qv pane-scrollbars)" = "auto-hide" ]
}

@test "session user options read back with a bare -t target, not the = exact-match prefix" {
	# Pins the targeting asymmetry og-remote-open's mirror dedup relies on:
	# show-options rejects the "=" prefix has-session accepts, and -q hides that
	# failure as an empty string that reads as "unset" (#474).
	t new-session -d -s mirror -c "$PWD"
	t new-session -d -s mirror-remote -c "$PWD"
	t set-option -t mirror @bridge_session upstream
	t set-option -t mirror-remote @bridge_session sibling

	t has-session -t "=mirror"

	run t show-options -t "=mirror" -v @bridge_session
	[ "$status" -ne 0 ]
	[ -z "$(t show-options -t "=mirror" -qv @bridge_session)" ]

	# The bare form reads it, and an exact match still beats the prefix sibling.
	[ "$(t show-options -t mirror -qv @bridge_session)" = upstream ]
}

# === Sixel capability via #{I/f:sixel} client interrogation (#649/3a) ===
#
# picker/remotebridge/daemon/viewident.go reads #{I/f:sixel} per-client rather
# than matching client_termfeatures tokens in Go — these cases pin the tmux
# behavior that resolveViewIdentity now leans on entirely, replacing the
# deleted TestRelayFromTermFeatures unit test's whole-token-vs-substring
# property with a claim about tmux itself.
#
# sixel_field_for_one_client attaches exactly one non-control client to
# session "s" via a throwaway obs-host server, targets it with -T (which sets
# that one client's client_termfeatures directly — no terminal-features/TERM
# pattern lookup involved), reads #{I/f:sixel} for the lone non-control
# client, then tears the obs server down before returning.
sixel_field_for_one_client() {
	local tfeat=$1
	local obs="og-next38-${BATS_TEST_NUMBER}-$$-obs-${2:-$tfeat}"
	"$TMUX_BIN" -L "$obs" new-session -d -x 80 -y 24 \
		"env TERM=xterm-256color $TMUX_BIN -L $SOCKET -T $tfeat attach -t s"
	local got=""
	local i
	for i in {1..50}; do
		got="$(t list-clients -F '#{?client_control_mode,,#{I/f:sixel}}' 2>/dev/null | grep -v '^$' || true)"
		[[ -n $got ]] && break
		sleep 0.1
	done
	"$TMUX_BIN" -L "$obs" kill-server 2>/dev/null || true
	for i in {1..30}; do
		[[ "$(t list-clients -t s 2>/dev/null | wc -l)" -eq 0 ]] && break
		sleep 0.1
	done
	printf '%s' "$got"
}

@test "I/f:sixel is 1 for a client carrying a whole sixel client_termfeatures token" {
	[ "$(sixel_field_for_one_client sixel)" = "1" ]
}

@test "I/f:sixel is 0 for substring-only tokens (nosixel, sixelfoo), not a whole match" {
	# tmux's own -T validation drops an unrecognized feature name rather than
	# storing it, so neither client ends up with a "sixel" token at all — the
	# negative case this repo's deleted Go test used to police is now a
	# property of tmux itself, not of this repo's code.
	[ "$(sixel_field_for_one_client nosixel)" = "0" ]
	[ "$(sixel_field_for_one_client sixelfoo)" = "0" ]
}

@test "I/f:sixel is empty (not 0) for a control-mode client" {
	# Matches 3a.0's own scratch-server measurement: a control client's
	# client_termfeatures is always empty (E1), and #{I/f:sixel} for one
	# reads as empty, not the string "0" — resolveViewIdentity relies on
	# control rows being skipped outright rather than on this specific value,
	# but the field's shape is worth pinning since it is what 3a.0 measured.
	coproc CTL { "$TMUX_BIN" -L "$SOCKET" -C attach-session -t s; }
	wait_for_client

	got="$(t list-clients -F '#{?client_control_mode,#{I/f:sixel},}')"

	kill "$CTL_PID" 2>/dev/null || true

	[ "$got" = "" ]
}
