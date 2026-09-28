#!/usr/bin/env bash
# Claude usage-limit provider: query the OAuth usage endpoint (the one /usage
# renders) with the CLI's own token and atomically rewrite
# $CACHE_DIR/claude.json for tmux-statusline. A failed fetch (offline, expired
# token, non-JSON) leaves the previous cache untouched.
set -uo pipefail

# The dispatcher (tmux-agent-usage.sh) resolves and owner-checks the cache
# dir, then exports it; run with none set, there's nowhere trusted to write.
CACHE_DIR="${OG_AGENT_USAGE_DIR:-}"
[[ -n $CACHE_DIR ]] || exit 0
CREDS="${CLAUDE_CREDENTIALS:-$HOME/.claude/.credentials.json}"

token=$(jq -r '.claudeAiOauth.accessToken // empty' "$CREDS" 2>/dev/null)
[[ -n $token ]] || exit 0

resp=$(curl -fsS --max-time 10 \
	-H "Authorization: Bearer $token" \
	-H "anthropic-beta: oauth-2025-04-20" \
	https://api.anthropic.com/api/oauth/usage 2>/dev/null) || exit 0

out=$(jq -c '
	def epoch(r): if r == null then null
		else (r | sub("\\.[0-9]+"; "") | strptime("%Y-%m-%dT%H:%M:%S%z") | mktime) end;
	def w(l; u; r): if u == null then empty
		else ({label: l, pct: (u | floor)}
			+ (epoch(r) as $e | if $e == null then {} else {reset_at: $e} end)) end;
	{
		windows: [
			w("5h"; .five_hour.utilization; .five_hour.resets_at),
			w("7d"; .seven_day.utilization; .seven_day.resets_at)
		],
		monthly: (if .extra_usage.is_enabled == true and .extra_usage.utilization != null
			then {label: "mo", pct: (.extra_usage.utilization | floor)}
			else null end)
	}' <<<"$resp" 2>/dev/null) || exit 0

tmp="$CACHE_DIR/.claude.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/claude.json"
