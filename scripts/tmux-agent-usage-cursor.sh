#!/usr/bin/env bash
# Cursor usage provider, from the dashboard's own usage-summary endpoint with
# the CLI's own session token.
#
# `cursor_usage` is the one fetch/normalize path: it prints a policy-free JSON
# object (plan %, plan dollars, raw pool %s, billing cycle, every on-demand
# pool) or returns 2 (no token), 3 (no account), 4 (fetch failed), 5
# (unrecognised response). Two callers share it:
#   --print  print that object and exit with its code; no cache, no gate. A
#            public interface for other tools (docs/agents/enrichment.md).
#   (none)   project it into the cache schema and atomically rewrite
#            $OG_AGENT_USAGE_DIR/cursor.json for tmux-statusline. Any failure
#            exits 0 before the write, leaving the previous cache untouched.
# Amounts from usage-summary are cents; the token reaches curl only through a
# config on stdin and jq only on stdin, and every call that sees auth state
# discards stderr (jq errors can echo string values).
set -uo pipefail
PATH="@cursor_path@:$PATH"

cursor_usage() {
	local conf token account resp
	if [[ $(uname -s) == Darwin ]]; then
		conf="$HOME/.cursor/cli-config.json"
		token=$(timeout 10 security find-generic-password -s cursor-access-token -a cursor-user -w 2>/dev/null) || token=""
	else
		local dir="${XDG_CONFIG_HOME:-$HOME/.config}/cursor"
		conf="$dir/cli-config.json"
		token=$(jq -r '[.accessToken, .access_token, .token | strings | select(. != "")][0] // empty' "$dir/auth.json" 2>/dev/null) || token=""
	fi
	[[ $token =~ ^[A-Za-z0-9._-]+$ ]] || return 2

	account=$(jq -r '[.authInfo.authId, .authInfo.userId | strings | select(. != "")][0] // empty' "$conf" 2>/dev/null) || account=""
	if [[ -z $account ]]; then
		account=$(jq -Rr '
			split(".")[1] // empty
			| gsub("-"; "+") | gsub("_"; "/")
			| . + ("=" * ((4 - length % 4) % 4))
			| @base64d | fromjson | .sub | strings' <<<"$token" 2>/dev/null) || account=""
	fi
	account="${account##*|}"
	[[ $account =~ ^[A-Za-z0-9._-]+$ ]] || return 3

	resp=$(curl -sf --max-time 15 -K - https://cursor.com/api/usage-summary 2>/dev/null <<<"header = \"Accept: application/json\"
header = \"Cookie: WorkosCursorSessionToken=$account::$token\"") || return 4

	# The team shape reports individualUsage.overall used/limit, the individual
	# shape per-pool percentages (max of the two). The billing-cycle bounds are
	# both kept or both dropped. A team plan without an individual limit has
	# dollars but no percentage (used_pct null). A numeric on-demand limit <= 0
	# passes through.
	jq -ce '
		def toepoch:
			if type == "number" then (if . > 1e12 then . / 1000 else . end) | floor
			elif type == "string" then (try (sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601) catch null)
			else null end;
		def bounded: (.limit | type) == "number" and .limit > 0 and (.used | type) == "number";
		def cents: if type == "number" then . / 100 else null end;
		def od($scope): objects | {
			scope: $scope,
			enabled: (.enabled == true),
			used_usd: (.used | cents),
			limit_usd: (.limit | cents)
		};
		if type != "object" then error("not an object") else . end
		| (.isUnlimited == true) as $unlimited
		| ([.individualUsage.overall | objects][0]) as $o
		| ([.individualUsage.overall | objects | select(bounded) | .used / .limit * 100][0]) as $overall
		| ([.individualUsage.plan | objects | .autoPercentUsed | numbers][0]) as $auto
		| ([.individualUsage.plan | objects | .apiPercentUsed | numbers][0]) as $api
		| (if $overall != null then $overall else ([$auto, $api | numbers] | max) end) as $pct
		| if $pct == null and ($unlimited | not) and (($o.used | type) != "number") then error("no usable percentage") else . end
		| (.billingCycleStart | toepoch) as $s
		| (.billingCycleEnd | toepoch) as $e
		| (if $s != null and $e != null and $e > $s then [$s, $e] else [null, null] end) as [$starts, $resets]
		| {
			plan_type: (if (.membershipType | type) == "string" then .membershipType else null end),
			unlimited: $unlimited,
			cycle: {starts_at: $starts, resets_at: $resets},
			plan: {
				used_pct: $pct,
				used_usd: ($o.used | cents),
				limit_usd: (if ($o.limit | type) == "number" and $o.limit > 0 then $o.limit / 100 else null end)
			},
			pools: {auto_pct: $auto, api_pct: $api},
			on_demand: [(.individualUsage.onDemand | od("individual")), (.teamUsage.onDemand | od("team"))]
		}' <<<"$resp" 2>/dev/null || return 5
}

if [[ ${1:-} == --print ]]; then
	out=$(cursor_usage) || exit $?
	printf '%s\n' "$out"
	exit 0
fi

# The dispatcher (tmux-agent-usage.sh) resolves and owner-checks the cache
# dir, then exports it; run with none set, there's nowhere trusted to write.
CACHE_DIR="${OG_AGENT_USAGE_DIR:-}"
[[ -n $CACHE_DIR ]] || exit 0

norm=$(cursor_usage) || exit 0

out=$(jq -c '
	{
		windows: [],
		monthly: (if .unlimited or .plan.used_pct == null then null
			else ({label: "mo", pct: ((.plan.used_pct * 10 | round) / 10)}
				+ (if .cycle.resets_at != null then {reset_at: .cycle.resets_at} else {} end))
			end),
		spend: (if .plan.used_usd != null
			then ({label: "mo", usd: .plan.used_usd, period: "cycle"}
				+ (if .plan.limit_usd != null and (.unlimited | not) then {limit_usd: .plan.limit_usd} else {} end))
			else null end)
	}' <<<"$norm" 2>/dev/null) || exit 0

tmp="$CACHE_DIR/.cursor.json.$$"
printf '%s\n' "$out" >"$tmp" && mv -f "$tmp" "$CACHE_DIR/cursor.json"
