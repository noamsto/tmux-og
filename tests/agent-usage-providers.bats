#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# shellcheck disable=SC2016 # pi auth.json keys hold a literal "$VAR" for the provider to resolve
bats_require_minimum_version 1.5.0 # run --separate-stderr
# The cursor and pi usage providers, end to end against fixture API responses.
# What's worth pinning is the normalization into the cache shape
# tmux-statusline reads and cursor's `--print` object, pi's pct taken from
# limit/limit_remaining whatever the reset period, and pi's auth.json key
# resolution (literal/escaped/env, OAuth .access, and that a `!command` key
# must never run).
#
# Fakes: curl answers from $FIXTURES/<last URL segment>.json. pi's `-H
# Authorization: Bearer` requests must carry $EXPECT_TOKEN (a `/credits` one
# $EXPECT_MGMT_TOKEN, the management key). cursor's `-K -` request reads its
# curl config from stdin and its Cookie header must equal
# WorkosCursorSessionToken=$EXPECT_ACCOUNT::$EXPECT_TOKEN. Either way a served
# fixture proves which credential the provider resolved, and the stub records
# only argv (to curl-argv.log), never a credential.

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	FIXTURES="$BATS_TEST_TMPDIR/fixtures"
	mkdir -p "$FAKEBIN" "$FIXTURES"
	export FIXTURES
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/cache"
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
	export HOME="$BATS_TEST_TMPDIR"
	unset OPENROUTER_API_KEY PI_AUTH OG_OPENROUTER_MGMT_KEY_FILE OPENROUTER_MANAGEMENT_KEY XDG_CONFIG_HOME

	cat >"$FAKEBIN/curl" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$BATS_TEST_TMPDIR/curl-argv.log"
		auth='' url='' stdin_conf=''
		while [ $# -gt 0 ]; do
			if [ "$1" = -H ]; then
				shift
				case "$1" in "Authorization: Bearer "*) auth=${1#Authorization: Bearer } ;; esac
			fi
			if [ "$1" = -K ]; then
				shift
				[ "$1" = - ] && stdin_conf=1
			fi
			url=$1
			shift
		done
		if [ -n "$stdin_conf" ]; then
			cookie=''
			while IFS= read -r line; do
				case "$line" in
					'header = "Cookie: '*'"') cookie=${line#'header = "Cookie: '}; cookie=${cookie%'"'} ;;
				esac
			done
			[ -n "$EXPECT_TOKEN" ] && [ "$cookie" = "WorkosCursorSessionToken=$EXPECT_ACCOUNT::$EXPECT_TOKEN" ] || exit 22
		else
			case "$url" in
				*credits) want=$EXPECT_MGMT_TOKEN ;;
				*) want=$EXPECT_TOKEN ;;
			esac
			[ -n "$want" ] && [ "$auth" = "$want" ] || exit 22
		fi
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
	echo '{"authInfo":{"authId":"auth0|user_synth01"}}' >"$HOME/.config/cursor/cli-config.json"
	export EXPECT_TOKEN=cursor-test-not-a-token EXPECT_ACCOUNT=user_synth01
	printf '#!/bin/sh\necho Linux\n' >"$FAKEBIN/uname"
	chmod +x "$FAKEBIN/uname"
	CACHE="$OG_AGENT_USAGE_DIR/cursor.json"
}

team_fixture() {
	cat >"$FIXTURES/usage-summary.json" <<-'EOF'
		{"billingCycleStart":"2026-10-01T00:00:00.000Z","billingCycleEnd":"2026-11-01T00:00:00.000Z",
		"membershipType":"enterprise","limitType":"team","isUnlimited":false,
		"individualUsage":{"overall":{"enabled":true,"used":40603,"limit":105000,"remaining":64397}},
		"teamUsage":{"onDemand":{"enabled":true,"used":86155,"limit":600000,"remaining":513845}}}
	EOF
}

individual_fixture() {
	cat >"$FIXTURES/usage-summary.json" <<-'EOF'
		{"billingCycleStart":"2026-10-01T00:00:00.000Z","billingCycleEnd":"2026-11-01T00:00:00.000Z",
		"membershipType":"pro","limitType":"user","isUnlimited":false,
		"individualUsage":{"plan":{"autoPercentUsed":20,"apiPercentUsed":61.5},
		"onDemand":{"enabled":false,"used":0,"limit":null}}}
	EOF
}

unlimited_fixture() {
	cat >"$FIXTURES/usage-summary.json" <<-'EOF'
		{"billingCycleStart":"2026-10-01T00:00:00.000Z","billingCycleEnd":"2026-11-01T00:00:00.000Z",
		"membershipType":"enterprise","limitType":"team","isUnlimited":true,
		"individualUsage":{"overall":{"enabled":true,"used":5000,"limit":null}}}
	EOF
}

nolimit_fixture() {
	cat >"$FIXTURES/usage-summary.json" <<-'EOF'
		{"billingCycleStart":"2026-10-01T00:00:00.000Z","billingCycleEnd":"2026-11-01T00:00:00.000Z",
		"membershipType":"enterprise","limitType":"team","isUnlimited":false,
		"individualUsage":{"overall":{"enabled":true,"used":5000,"limit":null}}}
	EOF
}

# --print output with the raw used_pct rounded to 2 decimals, key-sorted.
print_norm() { jq -cS '.plan.used_pct |= (if . == null then . else (. * 100 | round) / 100 end)' <<<"$output"; }

@test "cursor: team shape writes plan pct, cycle reset and plan dollars" {
	cursor_setup
	team_fixture
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly "$CACHE")" = '{"label":"mo","pct":38.7,"reset_at":1793491200}' ]
	[ "$(jq -c .spend "$CACHE")" = '{"label":"mo","usd":406.03,"period":"cycle","limit_usd":1050}' ]
	[ "$(jq -c .windows "$CACHE")" = '[]' ]
}

@test "cursor: individual shape takes max(auto, api) as pct and has no spend" {
	cursor_setup
	individual_fixture
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly.pct "$CACHE")" = 61.5 ]
	[ "$(jq -c .spend "$CACHE")" = null ]
}

@test "cursor: unlimited plan has no monthly and spend without a limit" {
	cursor_setup
	unlimited_fixture
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly "$CACHE")" = null ]
	[ "$(jq -c .spend "$CACHE")" = '{"label":"mo","usd":50,"period":"cycle"}' ]
	nolimit_fixture
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly "$CACHE")" = null ]
	[ "$(jq -c .spend "$CACHE")" = '{"label":"mo","usd":50,"period":"cycle"}' ]
}

@test "cursor: a failed or unrecognised fetch leaves the previous cache byte-identical" {
	cursor_setup
	echo '{"windows":[],"monthly":{"label":"mo","pct":9},"spend":null}' >"$CACHE"
	cp "$CACHE" "$BATS_TEST_TMPDIR/seed"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	cmp "$CACHE" "$BATS_TEST_TMPDIR/seed"
	echo '{}' >"$FIXTURES/usage-summary.json"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	cmp "$CACHE" "$BATS_TEST_TMPDIR/seed"
	team_fixture
	rm "$HOME/.config/cursor/cli-config.json"
	echo '{"accessToken":"not-a-jwt"}' >"$HOME/.config/cursor/auth.json"
	export EXPECT_TOKEN=not-a-jwt
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	cmp "$CACHE" "$BATS_TEST_TMPDIR/seed"
}

@test "cursor --print: each shape prints the normalized object and writes no cache" {
	cursor_setup
	unset OG_AGENT_USAGE_DIR
	team_fixture
	run bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 0 ]
	[ "$(print_norm)" = "$(jq -cS . <<<'{"plan_type":"enterprise","unlimited":false,"cycle":{"starts_at":1790812800,"resets_at":1793491200},"plan":{"used_pct":38.67,"used_usd":406.03,"limit_usd":1050},"pools":{"auto_pct":null,"api_pct":null},"on_demand":[{"scope":"team","enabled":true,"used_usd":861.55,"limit_usd":6000}]}')" ]
	individual_fixture
	run bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 0 ]
	[ "$(print_norm)" = "$(jq -cS . <<<'{"plan_type":"pro","unlimited":false,"cycle":{"starts_at":1790812800,"resets_at":1793491200},"plan":{"used_pct":61.5,"used_usd":null,"limit_usd":null},"pools":{"auto_pct":20,"api_pct":61.5},"on_demand":[{"scope":"individual","enabled":false,"used_usd":0,"limit_usd":null}]}')" ]
	unlimited_fixture
	run bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 0 ]
	[ "$(print_norm)" = "$(jq -cS . <<<'{"plan_type":"enterprise","unlimited":true,"cycle":{"starts_at":1790812800,"resets_at":1793491200},"plan":{"used_pct":null,"used_usd":50,"limit_usd":null},"pools":{"auto_pct":null,"api_pct":null},"on_demand":[]}')" ]
	nolimit_fixture
	run bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 0 ]
	[ "$(jq -c '.plan' <<<"$output")" = '{"used_pct":null,"used_usd":50,"limit_usd":null}' ]
	[ -z "$(find "$OG_AGENT_USAGE_DIR" -mindepth 1 2>/dev/null)" ]
}

@test "cursor --print: distinct exit codes and empty stdout on failure" {
	cursor_setup
	team_fixture
	rm "$HOME/.config/cursor/auth.json"
	run --separate-stderr bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 2 ]
	[ -z "$output" ]
	echo '{"accessToken":"not-a-jwt"}' >"$HOME/.config/cursor/auth.json"
	rm "$HOME/.config/cursor/cli-config.json"
	run --separate-stderr bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 3 ]
	[ -z "$output" ]
	cursor_setup
	rm "$FIXTURES/usage-summary.json"
	run --separate-stderr bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 4 ]
	[ -z "$output" ]
	echo '{"foo":1}' >"$FIXTURES/usage-summary.json"
	run --separate-stderr bash scripts/tmux-agent-usage-cursor.sh --print
	[ "$status" -eq 5 ]
	[ -z "$output" ]
}

@test "cursor: account falls back to the token's JWT sub when cli-config is absent" {
	cursor_setup
	team_fixture
	rm "$HOME/.config/cursor/cli-config.json"
	payload=$(printf '{"sub":"auth0|user_jwt9"}' | base64 | tr -d '\n=' | tr '+/' '-_')
	tok="eyJhbGciOiJub25lIn0.$payload.sig"
	echo "{\"accessToken\":\"$tok\"}" >"$HOME/.config/cursor/auth.json"
	export EXPECT_TOKEN=$tok EXPECT_ACCOUNT=user_jwt9
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly.pct "$CACHE")" = 38.7 ]
}

@test "cursor: XDG_CONFIG_HOME relocates the config, and .access_token is accepted" {
	cursor_setup
	team_fixture
	export XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/xdg"
	mkdir -p "$XDG_CONFIG_HOME/cursor"
	echo '{"accessToken":"","token":"cursor-test-not-a-token"}' >"$XDG_CONFIG_HOME/cursor/auth.json"
	cp "$HOME/.config/cursor/cli-config.json" "$XDG_CONFIG_HOME/cursor/"
	rm -r "$HOME/.config/cursor"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly.pct "$CACHE")" = 38.7 ]
	echo '{"access_token":"cursor-test-not-a-token"}' >"$XDG_CONFIG_HOME/cursor/auth.json"
	rm "$CACHE"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly.pct "$CACHE")" = 38.7 ]
}

@test "cursor: macOS reads the token from the keychain and the account from ~/.cursor" {
	cursor_setup
	team_fixture
	rm -r "$HOME/.config/cursor"
	mkdir -p "$HOME/.cursor"
	echo '{"authInfo":{"userId":"auth0|user_synth01"}}' >"$HOME/.cursor/cli-config.json"
	printf '#!/bin/sh\necho Darwin\n' >"$FAKEBIN/uname"
	printf '#!/bin/sh\necho cursor-test-not-a-token\n' >"$FAKEBIN/security"
	chmod +x "$FAKEBIN/security"
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ "$(jq -c .monthly.pct "$CACHE")" = 38.7 ]
}

@test "cursor: the token never reaches stdout, stderr, the cache or curl's argv" {
	cursor_setup
	team_fixture
	bash scripts/tmux-agent-usage-cursor.sh >"$BATS_TEST_TMPDIR/out" 2>"$BATS_TEST_TMPDIR/err"
	bash scripts/tmux-agent-usage-cursor.sh --print >>"$BATS_TEST_TMPDIR/out" 2>>"$BATS_TEST_TMPDIR/err"
	[ -s "$CACHE" ]
	[ -s "$BATS_TEST_TMPDIR/curl-argv.log" ]
	run grep -rF "$EXPECT_TOKEN" "$BATS_TEST_TMPDIR/out" "$BATS_TEST_TMPDIR/err" "$CACHE" "$BATS_TEST_TMPDIR/curl-argv.log"
	[ "$status" -eq 1 ]
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

@test "pi key: an OAuth credential's minted key comes from .access" {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-test-oauth-not-a-key","refresh":"","expires":9007199254740991}}'
	export EXPECT_TOKEN=sk-or-test-oauth-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

@test "pi key: a literal .key wins over an OAuth .access" {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"key":"sk-or-test-literal-not-a-key","type":"oauth","access":"sk-or-test-oauth-not-a-key"}}'
	export EXPECT_TOKEN=sk-or-test-literal-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

@test "pi key: an OAuth .access wins over OPENROUTER_API_KEY" {
	pi_key_fixture '{"usage_monthly":1}'
	pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-test-oauth-not-a-key"}}'
	export OPENROUTER_API_KEY=sk-or-test-fallback-not-a-key EXPECT_TOKEN=sk-or-test-oauth-not-a-key
	run_pi
	[ -e "$(pi_cache)" ]
}

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

# The dispatcher exports the owner-checked dir; providers have no fallback.

@test "provider: OG_AGENT_USAGE_DIR unset writes nothing, never touches the real default" {
	unset OG_AGENT_USAGE_DIR
	export XDG_RUNTIME_DIR="$BATS_TEST_TMPDIR/xdg"
	export TMPDIR="$BATS_TEST_TMPDIR/tmp"
	mkdir -p "$XDG_RUNTIME_DIR" "$TMPDIR"
	cursor_setup
	team_fixture
	run bash scripts/tmux-agent-usage-cursor.sh
	[ "$status" -eq 0 ]
	[ -z "$(find "$XDG_RUNTIME_DIR" "$TMPDIR" -mindepth 1 2>/dev/null)" ]
}
