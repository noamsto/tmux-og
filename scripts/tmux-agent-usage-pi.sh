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
# The cap itself (limit) is spend.limit_usd whenever set, even when
# limit_remaining is missing and `monthly` can't be computed. limit/
# limit_remaining are "credits", same unit as usage_monthly; OpenRouter's own
# docs (openrouter.ai/docs/faq) say credits are USD-denominated 1:1, so no
# conversion is needed.
#
# A second call to /api/v1/credits fetches remaining account balance
# (total_credits - total_usage). That endpoint requires a management key;
# pi's auth.json normally holds an ordinary inference key, which OpenRouter
# refuses ("Only management keys can perform this operation", a 403) — so
# refusal is the expected common case, not an error. That fetch never exits
# the script; a refused, empty, or malformed response just omits `balance`
# from the output, leaving `spend` (and everything else) unaffected.
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

credits=$(curl -fsS --max-time 10 \
	-H "Authorization: Bearer $token" \
	https://openrouter.ai/api/v1/credits 2>/dev/null) || credits=''

out=$(jq -c --arg credits "$credits" '
	.data as $d |
	($credits | try fromjson catch null) as $c |
	{
		windows: [],
		monthly: (if ($d.limit // 0) > 0 and $d.limit_remaining != null
			then {
				label: ({daily: "day", weekly: "wk", monthly: "mo"}[$d.limit_reset // ""] // "cap"),
				pct: (100 * ($d.limit - $d.limit_remaining) / $d.limit | floor)
			}
			else null end),
		spend: ({label: "mo", usd: ($d.usage_monthly // 0), period: "month"}
			+ (if ($d.limit // 0) > 0 then {limit_usd: $d.limit} else {} end))
	}
	+ (if ($c | type) == "object" and ($c.data | type) == "object"
			and ($c.data.total_credits | type) == "number"
			and ($c.data.total_usage | type) == "number"
		then {balance: {usd_remaining: ($c.data.total_credits - $c.data.total_usage)}}
		else {} end)' <<<"$resp" 2>/dev/null) || exit 0

mkdir -p "$CACHE_DIR" 2>/dev/null
tmp="$CACHE_DIR/.pi.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/pi.json"
