#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# shellcheck disable=SC2016 # pi auth.json keys hold a literal "$VAR" for the provider to resolve
# The cursor and pi usage providers, end to end against fixture API responses.
# What's worth pinning is the normalization into the cache shape
# tmux-statusline reads — `spend` written with no cap present, and pi's pct
# taken from limit/limit_remaining whatever the reset period — plus pi's
# auth.json key resolution, where a `!command` key must never run.
#
# Fakes: curl answers from $FIXTURES/<last URL segment>.json. The bearer token
# required depends on the URL: a `/credits` request must carry $EXPECT_MGMT_TOKEN
# (the management key), everything else (cursor's endpoints, pi's `/key`) must
# carry $EXPECT_TOKEN — so a cache file existing proves which key the provider
# resolved, without the stub ever recording the token.

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	FIXTURES="$BATS_TEST_TMPDIR/fixtures"
	mkdir -p "$FAKEBIN" "$FIXTURES"
	export FIXTURES
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/cache"
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
	export HOME="$BATS_TEST_TMPDIR"
	unset OPENROUTER_API_KEY PI_AUTH CURSOR_AUTH OG_OPENROUTER_MGMT_KEY_FILE OPENROUTER_MANAGEMENT_KEY XDG_CONFIG_HOME

	cat >"$FAKEBIN/curl" <<-'EOF'
		#!/bin/sh
		auth='' url=''
		while [ $# -gt 0 ]; do
			if [ "$1" = -H ]; then
				shift
				case "$1" in "Authorization: Bearer "*) auth=${1#Authorization: Bearer } ;; esac
			fi
			url=$1
			shift
		done
		case "$url" in
			*credits) want=$EXPECT_MGMT_TOKEN ;;
			*) want=$EXPECT_TOKEN ;;
		esac
		[ -n "$want" ] && [ "$auth" = "$want" ] || exit 22
		f="$FIXTURES/${url##*/}.json"
		[ -f "$f" ] || exit 22
		cat "$f"
	EOF
	chmod +x "$FAKEBIN/curl"
	export PATH="$FAKEBIN:$PATH"
}

# --- cursor ---

cursor_setup() {
	mkdir -p "$HOME/.config/cursor"
	echo '{"accessToken":"cursor-test-not-a-token"}' >"$HOME/.config/cursor/auth.json"
	export EXPECT_TOKEN=cursor-test-not-a-token
	echo '{"teamId":1,"userId":2}' >"$FIXTURES/GetMe.json"
	echo '{}' >"$FIXTURES/GetCurrentPeriodUsage.json"
	echo '{"totalCostCents":1234}' >"$FIXTURES/GetAggregatedUsageEvents.json"
}

@test "cursor: spend is the cycle's totalCostCents in USD, capped by the hard limit" {
	cursor_setup
	echo '{"hardLimit":5000}' >"$FIXTURES/GetHardLimit.json"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	cache="$OG_AGENT_USAGE_DIR/cursor.json"
	[ "$(jq -c .spend "$cache")" = '{"label":"mo","usd":12.34,"period":"cycle","limit_usd":50}' ]
	[ "$(jq -c .monthly.pct "$cache")" = 24 ]
}

@test "cursor: spend is written with no hard limit, and no limit_usd" {
	cursor_setup
	echo '{}' >"$FIXTURES/GetHardLimit.json"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	cache="$OG_AGENT_USAGE_DIR/cursor.json"
	[ "$(jq -c .spend "$cache")" = '{"label":"mo","usd":12.34,"period":"cycle"}' ]
	[ "$(jq -c .monthly "$cache")" = null ]
}

# --- pi ---

pi_auth() { # KEY-OBJECT-JSON
	mkdir -p "$HOME/.pi/agent"
	printf '%s\n' "$1" >"$HOME/.pi/agent/auth.json"
}

pi_key_fixture() { # DATA-JSON
	printf '{"data":%s}\n' "$1" >"$FIXTURES/key.json"
}

pi_mgmt_key_file() { # TOKEN
	printf '%s\n' "$1" >"$BATS_TEST_TMPDIR/mgmt-key"
	export OG_OPENROUTER_MGMT_KEY_FILE="$BATS_TEST_TMPDIR/mgmt-key"
}

run_pi() {
	run bash scripts/tmux-agent-usage-pi.sh
	[ "$status" -eq 0 ]
}

pi_cache() { echo "$OG_AGENT_USAGE_DIR/pi.json"; }

default_pi() {
	pi_auth '{"openrouter":{"key":"sk-or-test-not-a-key"}}'
	export EXPECT_TOKEN=sk-or-test-not-a-key
}

@test "pi: a monthly cap reports pct, month spend, and the cap as limit_usd" {
	default_pi
	pi_key_fixture '{"limit":20,"limit_remaining":15,"limit_reset":"monthly","usage_monthly":5}'
	run_pi
	[ "$(jq -c .monthly "$(pi_cache)")" = '{"label":"mo","pct":25}' ]
	[ "$(jq -c .spend.usd "$(pi_cache)")" = 5 ]
	[ "$(jq -c .spend.limit_usd "$(pi_cache)")" = 20 ]
	[ "$(jq -c .spend.remaining_usd "$(pi_cache)")" = 15 ]
	[ "$(jq -c .spend.remaining_label "$(pi_cache)")" = '"mo"' ]
}

@test "pi: a capped key with limit_remaining reports remaining_usd and remaining_label" {
	default_pi
	pi_key_fixture '{"limit":20,"limit_remaining":18.4,"limit_reset":"monthly","usage_monthly":1.6}'
	run_pi
	[ "$(jq -c .spend.remaining_usd "$(pi_cache)")" = 18.4 ]
	[ "$(jq -c .spend.remaining_label "$(pi_cache)")" = '"mo"' ]
}

@test "pi: an uncapped key has no remaining_usd or remaining_label" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":3.5}'
	run_pi
	[ "$(jq -c '.spend|has("remaining_usd")' "$(pi_cache)")" = false ]
	[ "$(jq -c '.spend|has("remaining_label")' "$(pi_cache)")" = false ]
}

@test "pi: limit_remaining null still gives limit_usd but no remaining_usd" {
	default_pi
	pi_key_fixture '{"limit":20,"limit_remaining":null,"limit_reset":"monthly","usage_monthly":5}'
	run_pi
	[ "$(jq -c .spend.limit_usd "$(pi_cache)")" = 20 ]
	[ "$(jq -c '.spend|has("remaining_usd")' "$(pi_cache)")" = false ]
}

@test "pi: a cap with no limit_remaining still reports limit_usd, without monthly" {
	default_pi
	pi_key_fixture '{"limit":20,"limit_remaining":null,"limit_reset":"monthly","usage_monthly":5}'
	run_pi
	[ "$(jq -c .monthly "$(pi_cache)")" = null ]
	[ "$(jq -c .spend.limit_usd "$(pi_cache)")" = 20 ]
}

@test "pi: a weekly cap's pct comes from limit_remaining, not usage_monthly" {
	default_pi
	# usage_monthly/limit would be 60%; the weekly window has spent 10%.
	pi_key_fixture '{"limit":20,"limit_remaining":18,"limit_reset":"weekly","usage_monthly":12}'
	run_pi
	[ "$(jq -c .monthly "$(pi_cache)")" = '{"label":"wk","pct":10}' ]
	[ "$(jq -c .spend.usd "$(pi_cache)")" = 12 ]
}

@test "pi: an uncapped key has no monthly or limit_usd but still reports spend" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":3.5}'
	run_pi
	[ "$(jq -c .monthly "$(pi_cache)")" = null ]
	[ "$(jq -c .spend "$(pi_cache)")" = '{"label":"mo","usd":3.5,"period":"month"}' ]
}

@test "pi: no cap, mgmt key configured — account balance is used as the left clause" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":1.2}'
	pi_mgmt_key_file mgmt-test-key
	export EXPECT_MGMT_TOKEN=mgmt-test-key
	echo '{"data":{"total_credits":20,"total_usage":1.6}}' >"$FIXTURES/credits.json"
	run_pi
	[ "$(jq -c .balance "$(pi_cache)")" = '{"usd_remaining":18.4}' ]
	[ "$(jq -c '.spend|has("remaining_usd")' "$(pi_cache)")" = false ]
}

@test "pi: no cap, no mgmt key configured — no balance, no left clause" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":1.2}'
	echo '{"data":{"total_credits":20,"total_usage":1.6}}' >"$FIXTURES/credits.json"
	run_pi
	[ "$(jq 'has("balance")' "$(pi_cache)")" = false ]
}

@test "pi: no cap, mgmt key configured but refused (403) — no balance, spend still written" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":1.2}'
	pi_mgmt_key_file mgmt-test-key
	export EXPECT_MGMT_TOKEN=mgmt-test-key
	run_pi
	[ "$(jq 'has("balance")' "$(pi_cache)")" = false ]
	[ "$(jq -c .spend.usd "$(pi_cache)")" = 1.2 ]
}

@test "pi: capped key with a management key configured still uses limit_remaining, never fetches the account balance" {
	default_pi
	pi_key_fixture '{"limit":20,"limit_remaining":18.4,"limit_reset":"monthly","usage_monthly":1.6}'
	pi_mgmt_key_file mgmt-test-key
	export EXPECT_MGMT_TOKEN=mgmt-test-key
	echo '{"data":{"total_credits":99,"total_usage":1}}' >"$FIXTURES/credits.json"
	run_pi
	[ "$(jq -c .spend.remaining_usd "$(pi_cache)")" = 18.4 ]
	[ "$(jq 'has("balance")' "$(pi_cache)")" = false ]
}

@test "pi: the management-key env fallback is used when no key file is set" {
	default_pi
	pi_key_fixture '{"limit":null,"limit_remaining":null,"limit_reset":null,"usage_monthly":1.2}'
	export OPENROUTER_MANAGEMENT_KEY=mgmt-test-key
	export EXPECT_MGMT_TOKEN=mgmt-test-key
	echo '{"data":{"total_credits":20,"total_usage":1.6}}' >"$FIXTURES/credits.json"
	run_pi
	[ "$(jq -c .balance "$(pi_cache)")" = '{"usd_remaining":18.4}' ]
}

# Key resolution: EXPECT_TOKEN is the key the provider should have chosen, so
# pi.json appearing proves the choice.

@test "pi key: a literal key is used as-is" {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"key":"sk-or-test-literal-not-a-key"}}'
	export EXPECT_TOKEN=sk-or-test-literal-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

@test 'pi key: $$ escapes to a literal key starting with $' {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"key":"$$sk-or-test-literal-dollar-not-a-key"}}'
	export EXPECT_TOKEN='$sk-or-test-literal-dollar-not-a-key'
	run_pi
	[ -e "$(pi_cache)" ]
}

@test 'pi key: $! escapes to a literal key starting with !' {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"key":"$!sk-or-test-literal-bang-not-a-key"}}'
	export EXPECT_TOKEN='!sk-or-test-literal-bang-not-a-key'
	run_pi
	[ -e "$(pi_cache)" ]
}

@test 'pi key: $VAR and ${VAR} interpolate the environment' {
	pi_key_fixture '{"usage_monthly":1}'
	export FAKE_OR_KEY=sk-or-test-env-not-a-key EXPECT_TOKEN=sk-or-test-env-not-a-key
	pi_auth '{"openrouter":{"key":"$FAKE_OR_KEY"}}'
	run_pi
	[ -e "$(pi_cache)" ]
	rm "$(pi_cache)"
	pi_auth '{"openrouter":{"key":"${FAKE_OR_KEY}"}}'
	run_pi
	[ -e "$(pi_cache)" ]
}

@test "pi key: a !command key is never run and falls through to OPENROUTER_API_KEY" {
	pi_key_fixture '{"usage_monthly":1}'
	marker="$BATS_TEST_TMPDIR/ran"
	pi_auth "{\"openrouter\":{\"key\":\"!touch $marker\"}}"
	export OPENROUTER_API_KEY=sk-or-test-fallback-not-a-key EXPECT_TOKEN=sk-or-test-fallback-not-a-key
	run_pi
	[ ! -e "$marker" ]
	[ -e "$(pi_cache)" ]
}

@test 'pi key: a composite ${A}_${B} key falls through to OPENROUTER_API_KEY' {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"key":"${A}_${B}"}}'
	export A=x B=y OPENROUTER_API_KEY=sk-or-test-fallback-not-a-key EXPECT_TOKEN=sk-or-test-fallback-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

@test "pi key: an auth.json with no openrouter key falls through to OPENROUTER_API_KEY" {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"anthropic":{"key":"sk-ant-test-not-a-key"}}'
	export OPENROUTER_API_KEY=sk-or-test-fallback-not-a-key EXPECT_TOKEN=sk-or-test-fallback-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

# --- OG_AGENT_USAGE_DIR resolution ---
# The dispatcher (tmux-agent-usage.sh) resolves and owner-checks the cache
# dir and exports it; a provider run standalone with it unset has nowhere
# trusted to write, and must not fall back to a machine-wide /tmp path.

@test "provider: OG_AGENT_USAGE_DIR unset writes nothing, never touches the real default" {
	unset OG_AGENT_USAGE_DIR
	export XDG_RUNTIME_DIR="$BATS_TEST_TMPDIR/xdg"
	export TMPDIR="$BATS_TEST_TMPDIR/tmp"
	mkdir -p "$XDG_RUNTIME_DIR" "$TMPDIR"
	cursor_setup
	echo '{"hardLimit":5000}' >"$FIXTURES/GetHardLimit.json"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ -z "$(find "$XDG_RUNTIME_DIR" "$TMPDIR" -mindepth 1 2>/dev/null)" ]
}
