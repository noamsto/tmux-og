#!/usr/bin/env bash
# pi usage provider: query OpenRouter's key-info endpoint with pi's own
# OpenRouter key and atomically rewrite $CACHE_DIR/pi.json for tmux-statusline.
# The key comes from auth.json's `.openrouter.key`, else the OAuth-minted
# `.openrouter.access`, else $OPENROUTER_API_KEY. A failed fetch (offline,
# revoked key, non-JSON) leaves the previous cache untouched.
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
# spend.remaining_usd is limit_remaining verbatim — same "credits" unit as
# usage_monthly/limit, no conversion needed — and is written only alongside a
# resolvable, capped key, mirroring the `monthly` gating above.
#
# When the key has no cap, a fallback call to /api/v1/credits fetches the
# account's remaining balance (total_credits - total_usage) instead, written
# as a top-level `balance` field. That endpoint requires a management key;
# pi's own auth.json key is an ordinary inference key, which OpenRouter
# refuses ("Only management keys can perform this operation", a 403), hence
# the separate key. It's resolved from $OG_OPENROUTER_MGMT_KEY_FILE (default
# $XDG_CONFIG_HOME/tmux-og/openrouter-mgmt-key) or, failing that, the
# $OPENROUTER_MANAGEMENT_KEY env var. A missing file, unset env, or a refused
# or malformed response all degrade silently — no `balance` field, and
# `spend`/`monthly`/`remaining_usd` are unaffected either way.
set -uo pipefail

# The dispatcher (tmux-agent-usage.sh) resolves and owner-checks the cache
# dir, then exports it; run with none set, there's nowhere trusted to write.
CACHE_DIR="${OG_AGENT_USAGE_DIR:-}"
[[ -n $CACHE_DIR ]] || exit 0
AUTH="${PI_AUTH:-$HOME/.pi/agent/auth.json}"

# An OAuth `/login` (pi's own login flow) stores the key it mints, a plain
# bearer token, under `.openrouter.access` with `type: "oauth"` — there is no
# `.key` then. Try it after `.key` and before the env, mirroring pi's own
# credential precedence.
#
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
[[ -n $token ]] || token=$(jq -r '.openrouter.access // empty' "$AUTH" 2>/dev/null)
[[ -n $token ]] || token="${OPENROUTER_API_KEY:-}"
[[ -n $token ]] || exit 0

mgmt_key_file="${OG_OPENROUTER_MGMT_KEY_FILE:-${XDG_CONFIG_HOME:-$HOME/.config}/tmux-og/openrouter-mgmt-key}"
mgmt_key=""
[[ -r $mgmt_key_file ]] && mgmt_key=$(<"$mgmt_key_file")
[[ -n $mgmt_key ]] || mgmt_key="${OPENROUTER_MANAGEMENT_KEY:-}"

resp=$(curl -fsS --max-time 10 \
	-H "Authorization: Bearer $token" \
	https://openrouter.ai/api/v1/key 2>/dev/null) || exit 0

has_cap_remaining=false
jq -e '(.data.limit // 0) > 0 and .data.limit_remaining != null' <<<"$resp" >/dev/null 2>&1 && has_cap_remaining=true

credits=''
if [[ $has_cap_remaining != true && -n $mgmt_key ]]; then
	credits=$(curl -fsS --max-time 10 \
		-H "Authorization: Bearer $mgmt_key" \
		https://openrouter.ai/api/v1/credits 2>/dev/null) || credits=''
fi

out=$(jq -c --arg credits "$credits" '
	.data as $d |
	($d.limit) as $limit |
	($d.limit_remaining) as $limit_remaining |
	({daily: "day", weekly: "wk", monthly: "mo"}[$d.limit_reset // ""] // "cap") as $label |
	($credits | try fromjson catch null) as $c |
	{
		windows: [],
		monthly: (if ($limit // 0) > 0 and $limit_remaining != null
			then {
				label: $label,
				pct: (100 * ($limit - $limit_remaining) / $limit | floor)
			}
			else null end),
		spend: ({label: "mo", usd: ($d.usage_monthly // 0), period: "month"}
			+ (if ($limit // 0) > 0 then {limit_usd: $limit} else {} end)
			+ (if ($limit // 0) > 0 and $limit_remaining != null
				then {remaining_usd: $limit_remaining, remaining_label: $label}
				else {} end))
	}
	+ (if ($c | type) == "object" and ($c.data | type) == "object"
			and ($c.data.total_credits | type) == "number"
			and ($c.data.total_usage | type) == "number"
		then {balance: {usd_remaining: ($c.data.total_credits - $c.data.total_usage)}}
		else {} end)' <<<"$resp" 2>/dev/null) || exit 0

tmp="$CACHE_DIR/.pi.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/pi.json"
