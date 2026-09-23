# Sources lib-enrich.sh for bats with Nix placeholders stubbed to defaults.
# Run from repo root: bats tests/enrich.bats
setup_lib_enrich() {
	local tmp
	tmp="$(mktemp)"
	sed \
		-e 's/@providers@/linear github/g' \
		-e 's/@enrich_icon_linear@/L/g' \
		-e 's/@enrich_icon_github@/G/g' \
		-e 's/@enrich_icon_pending@/P/g' \
		-e 's/@enrich_icon_success@/S/g' \
		-e 's/@enrich_icon_failure@/F/g' \
		-e 's/@enrich_icon_merged@/M/g' \
		-e 's/@enrich_icon_closed@/X/g' \
		-e 's/@enrich_icon_conflict@/C/g' \
		-e 's/@enrich_icon_draft@/D/g' \
		scripts/lib-enrich.sh >"$tmp"
	# shellcheck source=/dev/null
	source "$tmp"
	rm -f "$tmp"
}

setup_lib_icons() {
	local tmp
	tmp="$(mktemp)"
	# Stub the @ICON_MAP@ / @FALLBACK_ICON@ Nix placeholders so the file sources.
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$tmp"
	# shellcheck source=/dev/null
	source "$tmp"
	rm -f "$tmp"
}

setup_lib_claude() {
	# lib-claude.sh's only placeholder (@assume_dead_after@) is a quoted
	# assignment RHS that the file itself normalizes to 0 — see the
	# "unsubstituted placeholder parses as 0" case in agent-liveness.bats — so
	# raw sourcing is safe. A NEW placeholder here needs a sed stub, as
	# setup_lib_icons does.
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh
}

setup_lib_log() {
	# Sourced raw: @stat@ stays unsubstituted, which lib-log resolves to PATH's
	# (GNU coreutils) `stat`. No other placeholders.
	# shellcheck source=/dev/null
	source scripts/lib-log.sh
}

setup_lib_reflow() {
	# lib-reflow.sh has no Nix placeholders; source directly.
	# shellcheck source=/dev/null
	source scripts/lib-reflow.sh
}

# Builds a runnable claude-status with the @lib_claude@ placeholder resolved.
# Sets CLAUDE_STATUS_SCRIPT to the path.
make_claude_status() {
	CLAUDE_STATUS_SCRIPT="$BATS_TEST_TMPDIR/claude-status.sh"
	sed "s|@lib_claude@|$PWD/scripts/lib-claude.sh|" scripts/claude-status.sh >"$CLAUDE_STATUS_SCRIPT"
}

# Sources claude-status.sh's function definitions (count_for_session,
# count_for_window, tally_state, ...) into the current shell, stopping before
# the "# --- Main ---" CLI-parsing section so it's safe to call under bats
# without the script consuming bats' own args or exiting early.
setup_claude_status_functions() {
	setup_lib_claude
	local tmp
	tmp="$(mktemp)"
	sed -n '1,/^# --- Main ---/p' scripts/claude-status.sh | sed '$d; /^source @lib_claude@$/d' >"$tmp"
	# shellcheck source=/dev/null
	source "$tmp"
	rm -f "$tmp"
}

# Sources lib-notify.sh. Export OG_NOTIFY_DIR BEFORE calling: the dir
# constants derive at source time. notify_prune needs acquire_lock/file_mtime,
# so call setup_lib_log first when a test exercises it.
setup_lib_notify() {
	# lib-notify.sh has no Nix placeholders; source directly.
	# shellcheck source=/dev/null
	source scripts/lib-notify.sh
}

# Builds a runnable og-notify with @lib_log@/@lib_notify@ resolved — the raw
# script cannot run, its `source @lib_log@` would fail. Sets NOTIFY_ROUTER.
make_notify_router() {
	NOTIFY_ROUTER="$BATS_TEST_TMPDIR/og-notify.sh"
	sed -e "s|@lib_notify@|$PWD/scripts/lib-notify.sh|" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|" \
		scripts/og-notify.sh >"$NOTIFY_ROUTER"
}

# Builds a runnable og-notify-center with @lib_notify@ resolved.
# Sets NOTIFY_CENTER.
make_notify_center() {
	NOTIFY_CENTER="$BATS_TEST_TMPDIR/og-notify-center.sh"
	sed "s|@lib_notify@|$PWD/scripts/lib-notify.sh|" \
		scripts/og-notify-center.sh >"$NOTIFY_CENTER"
}

# Builds a runnable tmux-pr-enrich. Every placeholder the script contains must be
# stubbed: @lib_enrich@ and @lib_log@ are sourced, refresh placeholders are used
# in arithmetic (an unsubstituted value is a syntax error), and @reflow@ is
# EXECUTED from write_pr_options on exactly the change path the notify tests
# exercise (left unstubbed, the test execs the literal string). @notify@ is
# deliberately left raw — OG_NOTIFY_BIN overrides it at run time, and its
# raw form is what "notifications disabled" looks like. Sets PR_ENRICH_SCRIPT.
make_pr_enrich() {
	PR_ENRICH_SCRIPT="$BATS_TEST_TMPDIR/tmux-pr-enrich.sh"
	sed -e "s|@lib_enrich@|$PWD/scripts/lib-enrich.sh|" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|" \
		-e 's|@pr_refresh_seconds@|30|' \
		-e 's|@pr_check_refresh_seconds@|300|' \
		-e 's|@reflow@|true|' \
		scripts/tmux-pr-enrich.sh >"$PR_ENRICH_SCRIPT"
}

# Builds a runnable tmux-agent-usage with every placeholder resolved: @lib_log@
# is sourced, @refresh_seconds@ is used in arithmetic (unsubstituted is a syntax
# error), @AGENT_COMMANDS@ is the gate's match list, and the four provider
# placeholders are EXECUTED by a pass — left raw, a test execs the literal
# string. Providers become stubs that append their name to $USAGE_LOG.
# Sets AGENT_USAGE_SCRIPT.
make_agent_usage() {
	local stub
	for stub in claude codex cursor pi; do
		cat >"$BATS_TEST_TMPDIR/usage-$stub" <<-EOF
			#!/bin/sh
			printf '%s\n' "$stub" >>"\$USAGE_LOG"
		EOF
		chmod +x "$BATS_TEST_TMPDIR/usage-$stub"
	done
	AGENT_USAGE_SCRIPT="$BATS_TEST_TMPDIR/tmux-agent-usage.sh"
	sed -e "s|@lib_log@|$PWD/scripts/lib-log.sh|" \
		-e 's|@refresh_seconds@|120|' \
		-e 's|@AGENT_COMMANDS@|claude codex cursor-agent pi|' \
		-e "s|@usage_claude@|$BATS_TEST_TMPDIR/usage-claude|" \
		-e "s|@usage_codex@|$BATS_TEST_TMPDIR/usage-codex|" \
		-e "s|@usage_cursor@|$BATS_TEST_TMPDIR/usage-cursor|" \
		-e "s|@usage_pi@|$BATS_TEST_TMPDIR/usage-pi|" \
		scripts/tmux-agent-usage.sh >"$AGENT_USAGE_SCRIPT"
}
