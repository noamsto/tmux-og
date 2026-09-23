#!/usr/bin/env bash
# Cursor usage-limit provider: DashboardService with the CLI's own token →
# atomically rewrite $CACHE_DIR/cursor.json for tmux-statusline.
#
# Cursor exposes no short rate-limit windows via this API; the meaningful cap
# is the monthly spend hard limit (GetHardLimit, cents) measured against the
# billing-cycle's aggregated usage-based cost (totalCostCents, also cents).
# percentOfBurstUsed is the only short-window-like signal — shown as a "burst"
# window when nonzero. Fully pooled plans (totalCostCents 0 vs any limit) sit
# at 0% and stay hidden below the monthly threshold. totalCostCents is also
# written as `spend` (USD, billing cycle) whenever the cycle usage is known —
# independent of the `monthly` pct block, which stays limit-gated. A set hard
# limit rides along as spend.limit_usd; with none the key is omitted.
set -uo pipefail

CACHE_DIR="${OG_AGENT_USAGE_DIR:-/tmp/og-agent-usage}"
AUTH="${CURSOR_AUTH:-$HOME/.config/cursor/auth.json}"
API="https://api2.cursor.sh/aiserver.v1.DashboardService"

token=$(jq -r '.accessToken // empty' "$AUTH" 2>/dev/null)
[[ -n $token ]] || exit 0

post() { # METHOD BODY
	curl -fsS --max-time 10 \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/json" \
		-d "$2" "$API/$1" 2>/dev/null
}

me=$(post GetMe '{}') || exit 0
hard=$(post GetHardLimit '{}') || exit 0
cycle=$(post GetCurrentPeriodUsage '{}') || cycle='{}'

# Billing-cycle start in millis; a degenerate cycle (start==end, seen on
# enterprise) falls back to the first of the current UTC month.
start=$(jq -rn --argjson cycle "$cycle" '
	(($cycle.billingCycleStart // "0") | tonumber) as $cs |
	(($cycle.billingCycleEnd // "0") | tonumber) as $ce |
	if $cs > 0 and $cs != $ce then $cs
	else (now | strftime("%Y-%m") + "-01T00:00:00Z" | fromdateiso8601) * 1000 end
	| tostring' 2>/dev/null) || exit 0

agg=$(post GetAggregatedUsageEvents "$(jq -cn --argjson me "$me" --arg start "$start" '
	{teamId: $me.teamId, userId: $me.userId, startDate: $start, endDate: (now * 1000 | floor | tostring)}
	| with_entries(select(.value != null))' 2>/dev/null)") || exit 0

out=$(jq -cn --argjson agg "$agg" --argjson hard "$hard" --argjson cycle "$cycle" '
	($hard.hardLimit // null) as $limit |
	(($cycle.billingCycleEnd // "0") | tonumber) as $ce |
	{
		windows: (if ($agg.percentOfBurstUsed // 0) > 0
			then [{label: "burst", pct: $agg.percentOfBurstUsed}]
			else [] end),
		monthly: (if $limit != null and $limit > 0
			then ({label: "mo", pct: (100 * ($agg.totalCostCents // 0) / $limit | floor)}
				+ (if $ce > (now * 1000) then {reset_at: ($ce / 1000 | floor)} else {} end))
			else null end),
		spend: (if ($agg.totalCostCents // null) != null
			then ({label: "mo", usd: (($agg.totalCostCents | tonumber) / 100), period: "cycle"}
				+ (if $limit != null and $limit > 0 then {limit_usd: ($limit / 100)} else {} end))
			else null end)
	}' 2>/dev/null) || exit 0

mkdir -p "$CACHE_DIR" 2>/dev/null
tmp="$CACHE_DIR/.cursor.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/cursor.json"
