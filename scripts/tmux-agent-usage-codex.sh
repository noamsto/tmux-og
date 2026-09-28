#!/usr/bin/env bash
# Codex usage-limit provider: query the ChatGPT backend usage endpoint with the
# CLI's own token and atomically rewrite $CACHE_DIR/codex.json for
# tmux-statusline. A failed fetch leaves the previous cache untouched.
set -uo pipefail

# The dispatcher (tmux-agent-usage.sh) resolves and owner-checks the cache
# dir, then exports it; run with none set, there's nowhere trusted to write.
CACHE_DIR="${OG_AGENT_USAGE_DIR:-}"
[[ -n $CACHE_DIR ]] || exit 0
AUTH="${CODEX_AUTH:-$HOME/.codex/auth.json}"

token=$(jq -r '.tokens.access_token // empty' "$AUTH" 2>/dev/null)
acct=$(jq -r '.tokens.account_id // empty' "$AUTH" 2>/dev/null)
[[ -n $token && -n $acct ]] || exit 0

resp=$(curl -fsS --max-time 10 \
	-H "Authorization: Bearer $token" \
	-H "ChatGPT-Account-Id: $acct" \
	https://chatgpt.com/backend-api/wham/usage 2>/dev/null) || exit 0

# Windows arrive as primary/secondary with arbitrary durations (5h or weekly);
# sort short-first so the display reads 5h then 7d regardless of which slot
# the backend filled. Monthly is the workspace spend control: used/limit are
# decimal strings, so the percentage is computed here (the API's own
# used_percent rounds 82.8/100000 down to 0).
out=$(jq -c '
	def lbl(s): if s >= 86400 then "\(s / 86400 | floor)d" else "\(s / 3600 | floor)h" end;
	{
		windows: ([.rate_limit.primary_window, .rate_limit.secondary_window]
			| map(select(. != null))
			| sort_by(.limit_window_seconds)
			| map({label: lbl(.limit_window_seconds), pct: .used_percent}
				+ (if .reset_at == null then {} else {reset_at: .reset_at} end))),
		monthly: (if .spend_control.individual_limit.limit != null
			then ({label: "mo", pct: (100 * (.spend_control.individual_limit.used | tonumber)
				/ (.spend_control.individual_limit.limit | tonumber) | floor)}
				+ (if .spend_control.individual_limit.reset_at == null then {}
					else {reset_at: .spend_control.individual_limit.reset_at} end))
			else null end)
	}' <<<"$resp" 2>/dev/null) || exit 0

tmp="$CACHE_DIR/.codex.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/codex.json"
