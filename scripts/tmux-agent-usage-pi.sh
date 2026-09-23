#!/usr/bin/env bash
# pi usage provider: query OpenRouter's key-info endpoint with pi's own
# OpenRouter key and atomically rewrite $CACHE_DIR/pi.json for tmux-statusline.
# A failed fetch (offline, revoked key, non-JSON) leaves the previous cache
# untouched.
#
# OpenRouter has no short rate-limit windows. `spend` is usage_monthly (USD,
# current UTC calendar month), always written. `monthly` is a pct only when the
# key carries a cap: computed from limit vs limit_remaining so it stays right
# whatever limit_reset says (daily/weekly/monthly, or null = lifetime cap).
set -uo pipefail

CACHE_DIR="${OG_AGENT_USAGE_DIR:-/tmp/og-agent-usage}"
AUTH="${PI_AUTH:-$HOME/.pi/agent/auth.json}"

# pi's key syntax: `!cmd` runs a shell command (never executed here — a
# background status tick must not run commands from a config file), `$VAR` /
# `${VAR}` interpolates the environment, `$$` / `$!` escape a literal leading
# `$` / `!`, anything else is literal. pi also consults the credential's own
# `.openrouter.env` object before the process environment; that is not read
# here, so such a key falls through to OPENROUTER_API_KEY.
raw=$(jq -r '.openrouter.key // empty' "$AUTH" 2>/dev/null)
case "$raw" in
'!'*) token='' ;;
'$$'*) token="\$${raw#\$\$}" ;;
'$!'*) token="!${raw#\$!}" ;;
'$'*)
	var=${raw#\$}
	var=${var#\{}
	var=${var%\}}
	# Whole-value var name only: a composite like "${A}_${B}" is left
	# unresolved rather than risk ${!var} on a malformed name.
	if [[ $var =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
		token="${!var:-}"
	else
		token=''
	fi
	;;
*) token="$raw" ;;
esac
[[ -n $token ]] || token="${OPENROUTER_API_KEY:-}"
[[ -n $token ]] || exit 0

resp=$(curl -fsS --max-time 10 \
	-H "Authorization: Bearer $token" \
	https://openrouter.ai/api/v1/key 2>/dev/null) || exit 0

out=$(jq -c '
	.data as $d |
	{
		windows: [],
		monthly: (if ($d.limit // 0) > 0 and $d.limit_remaining != null
			then {
				label: ({daily: "day", weekly: "wk", monthly: "mo"}[$d.limit_reset // ""] // "cap"),
				pct: (100 * ($d.limit - $d.limit_remaining) / $d.limit | floor)
			}
			else null end),
		spend: {label: "mo", usd: ($d.usage_monthly // 0), period: "month"}
	}' <<<"$resp" 2>/dev/null) || exit 0

mkdir -p "$CACHE_DIR" 2>/dev/null
tmp="$CACHE_DIR/.pi.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/pi.json"
