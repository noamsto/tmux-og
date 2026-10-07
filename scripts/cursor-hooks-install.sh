#!/usr/bin/env bash
# Upsert tmux-og Cursor status hooks into ~/.cursor/hooks.json.
# Usage: cursor-hooks-install <wrapper-absolute-path>
# Strips prior entries whose command contains /bin/cursor-status-hook, then
# appends the template. Leaves aeye/user entries alone. Fails on malformed JSON.
set -euo pipefail

wrapper="${1:?usage: cursor-hooks-install <wrapper-path>}"
hooks_file="${CURSOR_HOOKS_FILE:-${CURSOR_HOME:-$HOME/.cursor}/hooks.json}"
marker='/bin/cursor-status-hook'

if ! command -v jq >/dev/null 2>&1; then
	echo "cursor-hooks-install: jq is required but not found on PATH" >&2
	exit 1
fi

mkdir -p "$(dirname "$hooks_file")"

if [[ -f $hooks_file ]]; then
	if ! jq empty "$hooks_file" >/dev/null 2>&1; then
		echo "cursor-hooks-install: malformed JSON in $hooks_file — refusing to clobber" >&2
		exit 1
	fi
	existing=$(cat "$hooks_file")
else
	existing='{"version":1,"hooks":{}}'
fi

# shellcheck disable=SC2016
merged=$(jq -n --argjson existing "$existing" --arg wrapper "$wrapper" --arg marker "$marker" '
	def entry($args; $timeout):
		{command: ($wrapper + " " + $args), timeout: $timeout};
	def strip:
		with_entries(.value |= map(select((.command // "") | contains($marker) | not)));
	def ensure_hooks:
		.hooks // {} | strip;
	($existing + {version: ($existing.version // 1)}) as $base |
	($base | ensure_hooks) as $h |
	$base * {hooks: (
		$h
		| .sessionStart = ((.sessionStart // []) + [
			entry("cleanup"; 15),
			entry("idle"; 15)
		])
		| .beforeSubmitPrompt = ((.beforeSubmitPrompt // []) + [
			entry("processing --force"; 15)
		])
		| .preToolUse = ((.preToolUse // []) + [
			entry("processing"; 15)
		])
		| .postToolUse = ((.postToolUse // []) + [
			entry("processing"; 15)
		])
		| .postToolUseFailure = ((.postToolUseFailure // []) + [
			entry("processing"; 15)
		])
		| .preCompact = ((.preCompact // []) + [
			entry("compacting"; 15)
		])
		| .stop = ((.stop // []) + [
			entry("done"; 15)
		])
		| .subagentStart = ((.subagentStart // []) + [
			entry("processing"; 15)
		])
	)}
')

tmp=$(mktemp)
printf '%s\n' "$merged" >"$tmp"
mv "$tmp" "$hooks_file"
