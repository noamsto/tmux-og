#!/usr/bin/env bats
# cursor-hooks-install upserts status hooks without clobbering unrelated entries.

setup() {
	export CURSOR_HOOKS_FILE="$BATS_TEST_TMPDIR/hooks.json"
	WRAPPER="$BATS_TEST_TMPDIR/bin/cursor-status-hook"
	mkdir -p "$(dirname "$WRAPPER")"
	: >"$WRAPPER"
	chmod +x "$WRAPPER"
	INSTALL="$BATS_TEST_DIRNAME/../scripts/cursor-hooks-install.sh"
	MODULE="$BATS_TEST_DIRNAME/../modules/home-manager.nix"
}

@test "creates hooks.json from empty and maps events" {
	run bash "$INSTALL" "$WRAPPER"
	[ "$status" -eq 0 ]
	[ -f "$CURSOR_HOOKS_FILE" ]

	run jq -r '.hooks.sessionStart[].command' "$CURSOR_HOOKS_FILE"
	[ "$status" -eq 0 ]
	[[ $output == *"$WRAPPER cleanup"* ]]
	[[ $output == *"$WRAPPER idle"* ]]

	run jq -r '.hooks.beforeSubmitPrompt[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER processing --force"* ]]

	run jq -r '.hooks.preToolUse[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER processing"* ]]

	run jq -r '.hooks.postToolUseFailure[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER processing"* ]]
	[[ $output != *"$WRAPPER error"* ]]

	run jq -r '.hooks.preCompact[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER compacting"* ]]

	run jq -r '.hooks.stop[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER done"* ]]

	run jq -r '.hooks.subagentStart[0].command' "$CURSOR_HOOKS_FILE"
	[[ $output == *"$WRAPPER processing"* ]]
}

@test "preserves aeye-shaped entries and upsert is idempotent" {
	cat >"$CURSOR_HOOKS_FILE" <<'EOF'
{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {"command": "/nix/store/aeye/adapters/cursor/scripts/session-reset.sh", "timeout": 15}
    ],
    "postToolUse": [
      {"command": "/nix/store/aeye/adapters/cursor/scripts/images.sh", "matcher": "Read|Write|Shell", "timeout": 15}
    ]
  }
}
EOF

	run bash "$INSTALL" "$WRAPPER"
	[ "$status" -eq 0 ]

	# aeye kept
	run jq -r '.hooks.sessionStart[] | select(.command | contains("aeye")) | .command' "$CURSOR_HOOKS_FILE"
	[[ $output == *session-reset.sh* ]]
	run jq -r '.hooks.postToolUse[] | select(.command | contains("aeye")) | .command' "$CURSOR_HOOKS_FILE"
	[[ $output == *images.sh* ]]

	# status present
	count1=$(jq '[.hooks.sessionStart[] | select(.command | contains("/bin/cursor-status-hook"))] | length' "$CURSOR_HOOKS_FILE")
	[ "$count1" -eq 2 ]

	# second run: still exactly 2 status sessionStart entries, aeye still there
	run bash "$INSTALL" "$WRAPPER"
	[ "$status" -eq 0 ]
	count2=$(jq '[.hooks.sessionStart[] | select(.command | contains("/bin/cursor-status-hook"))] | length' "$CURSOR_HOOKS_FILE")
	[ "$count2" -eq 2 ]
	run jq -r '[.hooks.sessionStart[] | select(.command | contains("aeye"))] | length' "$CURSOR_HOOKS_FILE"
	[ "$output" = "1" ]
}

@test "refuses to clobber malformed hooks.json" {
	echo 'not-json' >"$CURSOR_HOOKS_FILE"
	run bash "$INSTALL" "$WRAPPER"
	[ "$status" -ne 0 ]
	[[ $output == *malformed* ]] || [[ $stderr == *malformed* ]]
	[ "$(cat "$CURSOR_HOOKS_FILE")" = "not-json" ]
}

@test "cursorStatus is opt-in and requires agentIntegration" {
	run sed -n '/cursorStatus = {/,/agentIntegration = {/p' "$MODULE"
	[ "$status" -eq 0 ]
	[[ $output == *'default = false;'* ]]

	run grep -F 'programs.tmux-og.cursorStatus.enable requires agentIntegration.enable' "$MODULE"
	[ "$status" -eq 0 ]

	run grep -F 'provisionCursorStatusHooks' "$MODULE"
	[ "$status" -eq 0 ]
	run grep -F 'cursor-hooks-install' "$MODULE"
	[ "$status" -eq 0 ]
	run grep -F 'profileDirectory}/bin/cursor-status-hook' "$MODULE"
	[ "$status" -eq 0 ]
}
