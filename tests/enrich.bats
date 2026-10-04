#!/usr/bin/env bats

load helper

setup() {
	setup_lib_enrich
}

@test "branch_to_linear_key: lowercase team-num" {
	branch_to_linear_key "noa-123-foo"
	[ "$REPLY" = "NOA-123" ]
}

@test "branch_to_linear_key: already uppercase" {
	branch_to_linear_key "NOA-123-foo"
	[ "$REPLY" = "NOA-123" ]
}

@test "branch_to_linear_key: with slash prefix" {
	branch_to_linear_key "feature/noa-123-foo"
	[ "$REPLY" = "NOA-123" ]
}

@test "branch_to_linear_key: bare key no suffix" {
	branch_to_linear_key "noa-123"
	[ "$REPLY" = "NOA-123" ]
}

@test "branch_to_linear_key: pure-numeric prefix is not a linear key" {
	branch_to_linear_key "123-foo"
	[ -z "$REPLY" ]
}

@test "branch_to_linear_key: plain branch name yields empty" {
	branch_to_linear_key "main"
	[ -z "$REPLY" ]
}

@test "branch_to_gh_issue_number: leading number" {
	branch_to_gh_issue_number "247-fix-bug"
	[ "$REPLY" = "247" ]
}

@test "branch_to_gh_issue_number: gh- prefix" {
	branch_to_gh_issue_number "gh-247-fix"
	[ "$REPLY" = "247" ]
}

@test "branch_to_gh_issue_number: issue- prefix" {
	branch_to_gh_issue_number "issue-247"
	[ "$REPLY" = "247" ]
}

@test "branch_to_gh_issue_number: slash then number" {
	branch_to_gh_issue_number "feature/247-foo"
	[ "$REPLY" = "247" ]
}

@test "branch_to_gh_issue_number: linear-style branch is not a gh issue" {
	branch_to_gh_issue_number "noa-123-foo"
	[ -z "$REPLY" ]
}

@test "branch_to_gh_issue_number: plain branch yields empty" {
	branch_to_gh_issue_number "main"
	[ -z "$REPLY" ]
}

@test "sanitize_title: strips CR/LF" {
	sanitize_title "$(printf 'Add foo\r\nbar baz')"
	[ "$REPLY" = "Add foobar baz" ]
}

@test "sanitize_title: hard-truncates long titles to 256 chars" {
	local long
	printf -v long '%0300d' 0
	sanitize_title "$long"
	[ "${#REPLY}" -eq 256 ]
}

@test "sanitize_title: keeps a 120-char title whole" {
	local title
	printf -v title '%0120d' 0
	sanitize_title "$title"
	[ "$REPLY" = "$title" ]
}

@test "sanitize_title: strips ESC control char" {
	sanitize_title "$(printf 'title\033[31mcolored')"
	[ "$REPLY" = "title[31mcolored" ]
}

@test "sanitize_title: strips shell-unsafe quote and hash" {
	sanitize_title "Don't fix #123"
	[ "$REPLY" = "Dont fix 123" ]
}

@test "sanitize_title: strips pipe" {
	sanitize_title 'a|b'
	[[ $REPLY != *'|'* ]]
	[ "$REPLY" = "a b" ]
}

@test "truncate_ellipsis: short string is unchanged" {
	truncate_ellipsis "short" 25
	[ "$REPLY" = "short" ]
}

@test "truncate_ellipsis: long string gets ellipsis at limit" {
	truncate_ellipsis "this title is definitely longer than twenty-five" 25
	[ "${#REPLY}" -eq 25 ]
	[ "${REPLY: -1}" = "…" ]
}

@test "branch_sha1: stable 40-char hex for a branch" {
	branch_sha1 "feat/2-pr-window-enrichment"
	[ "${#REPLY}" -eq 40 ]
	[[ $REPLY =~ ^[0-9a-f]{40}$ ]]
}

@test "branch_sha1: same branch yields same key" {
	branch_sha1 "main"
	local first="$REPLY"
	branch_sha1 "main"
	[ "$REPLY" = "$first" ]
}

@test "collapse_check_rollup: all success/neutral → success" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-success.json)"
	[ "$REPLY" = "success" ]
}

@test "collapse_check_rollup: any failure → failure" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-failure.json)"
	[ "$REPLY" = "failure" ]
}

@test "collapse_check_rollup: any pending → pending" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-pending.json)"
	[ "$REPLY" = "pending" ]
}

@test "collapse_check_rollup: empty array → none" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-empty.json)"
	[ "$REPLY" = "none" ]
}

@test "collapse_check_rollup: pending + neutral → pending" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-mixed.json)"
	[ "$REPLY" = "pending" ]
}

@test "collapse_check_rollup: StatusContext failure → failure" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-statuscontext-failure.json)"
	[ "$REPLY" = "failure" ]
}

@test "collapse_check_rollup: StatusContext success → success" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-statuscontext-success.json)"
	[ "$REPLY" = "success" ]
}

@test "collapse_check_rollup: cancelled conclusion → failure" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-cancelled.json)"
	[ "$REPLY" = "failure" ]
}

@test "provider_priority_list: default order from substituted placeholder" {
	provider_priority_list
	[ "$REPLY" = "linear github" ]
}

@test "parse_explicit_issue_id: GH-<number> is github, uppercase prefix" {
	parse_explicit_issue_id "GH-42"
	[ "$REPLY_PROVIDER" = "github" ]
	[ "$REPLY_LOCAL" = "42" ]
}

@test "parse_explicit_issue_id: gh-<number> lowercase prefix is still github" {
	parse_explicit_issue_id "gh-42"
	[ "$REPLY_PROVIDER" = "github" ]
	[ "$REPLY_LOCAL" = "42" ]
}

@test "parse_explicit_issue_id: bare team key is linear, upper-cased" {
	parse_explicit_issue_id "eng-1957"
	[ "$REPLY_PROVIDER" = "linear" ]
	[ "$REPLY_LOCAL" = "ENG-1957" ]
}

@test "parse_explicit_issue_id: already-uppercase linear key is unchanged" {
	parse_explicit_issue_id "ENG-1957"
	[ "$REPLY_PROVIDER" = "linear" ]
	[ "$REPLY_LOCAL" = "ENG-1957" ]
}

@test "parse_explicit_issue_id: GH- with no digits is malformed, REPLY_LOCAL empty" {
	parse_explicit_issue_id "GH---web"
	[ "$REPLY_PROVIDER" = "github" ]
	[ -z "$REPLY_LOCAL" ]
}

@test "parse_explicit_issue_id: a leading-dash value is not forwarded as a linear key" {
	parse_explicit_issue_id "-URL"
	[ "$REPLY_PROVIDER" = "linear" ]
	[ -z "$REPLY_LOCAL" ]
}

@test "parse_explicit_issue_id: a non-numeric github suffix is malformed, REPLY_LOCAL empty" {
	parse_explicit_issue_id "GH-1a2b"
	[ "$REPLY_PROVIDER" = "github" ]
	[ -z "$REPLY_LOCAL" ]
}

@test "build_window_label: enriched short = provider id" {
	build_window_label short linear ENG-1957 "refactor services" "" "" "" feat/eng-1957 /x
	[ "$REPLY" = "L ENG-1957" ]
	[ "$REPLY_ID" = "L ENG-1957" ]
	[ "$REPLY_REST" = "" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: enriched long = provider id title" {
	build_window_label long linear ENG-1957 "refactor services" "" "" "" feat/eng-1957 /x
	[ "$REPLY" = "L ENG-1957 refactor services" ]
	[ "$REPLY_ID" = "L ENG-1957" ]
	[ "$REPLY_REST" = " refactor services" ]
}

@test "build_window_label: failing PR is a separate REPLY_PR segment in both modes" {
	build_window_label short github 247 "fix bug" 247 OPEN failure gh-247 /x
	[ "$REPLY" = "G 247" ]
	[ "$REPLY_PR" = " F #247" ]
	build_window_label long github 247 "fix bug" 247 OPEN failure gh-247 /x
	[ "$REPLY" = "G 247 fix bug" ]
	[ "$REPLY_PR" = " F #247" ]
}

@test "build_window_label: open PR with passing checks uses success glyph" {
	build_window_label short linear ENG-1 "" 9 open success br /x
	[ "$REPLY" = "L ENG-1" ]
	[ "$REPLY_PR" = " S #9" ]
}

@test "build_window_label: merged PR uses merged glyph" {
	build_window_label short linear ENG-1 "t" 9 merged success br /x
	[ "$REPLY" = "L ENG-1" ]
	[ "$REPLY_PR" = " M #9" ]
}

@test "build_window_label: merged glyph wins over a leftover pending rollup" {
	build_window_label short linear ENG-1 "t" 9 merged pending br /x
	[ "$REPLY_PR" = " M #9" ]
}

@test "build_window_label: merged glyph wins over a leftover failing rollup" {
	build_window_label short linear ENG-1 "t" 9 merged failure br /x
	[ "$REPLY_PR" = " M #9" ]
}

@test "build_window_label: closed PR uses closed glyph" {
	build_window_label short linear ENG-1 "t" 9 closed success br /x
	[ "$REPLY_PR" = " X #9" ]
}

@test "build_window_label: closed glyph wins over a leftover failing rollup" {
	build_window_label short linear ENG-1 "t" 9 closed failure br /x
	[ "$REPLY_PR" = " X #9" ]
}

@test "build_window_label: conflicting PR uses conflict glyph" {
	build_window_label short linear ENG-1 "t" 9 open success br /x conflicting
	[ "$REPLY_PR" = " C #9" ]
}

@test "build_window_label: conflict glyph wins over failing checks" {
	build_window_label short linear ENG-1 "t" 9 open failure br /x conflicting
	[ "$REPLY_PR" = " C #9" ]
}

@test "build_window_label: mergeable PR keeps check-state glyph" {
	build_window_label short linear ENG-1 "t" 9 open pending br /x mergeable
	[ "$REPLY_PR" = " P #9" ]
}

@test "build_window_label: draft PR prepends the draft glyph, keeping check state" {
	build_window_label short linear ENG-1 "t" 9 open success br /x mergeable "" "" 1
	[ "$REPLY_PR" = " D S #9" ]
	build_window_label short linear ENG-1 "t" 9 open pending br /x mergeable "" "" 1
	[ "$REPLY_PR" = " D P #9" ]
}

@test "build_window_label: draft marker sits alongside the conflict glyph" {
	build_window_label short linear ENG-1 "t" 9 open failure br /x conflicting "" "" 1
	[ "$REPLY_PR" = " D C #9" ]
}

@test "build_window_label: terminal states carry no draft marker" {
	build_window_label short linear ENG-1 "t" 9 merged success br /x mergeable "" "" 1
	[ "$REPLY_PR" = " M #9" ]
	build_window_label short linear ENG-1 "t" 9 closed success br /x mergeable "" "" 1
	[ "$REPLY_PR" = " X #9" ]
}

@test "build_window_label: non-draft PR is unchanged" {
	build_window_label short linear ENG-1 "t" 9 open success br /x mergeable "" "" ""
	[ "$REPLY_PR" = " S #9" ]
}

@test "build_window_label: pr_number=none is treated as no PR" {
	build_window_label short linear ENG-1 "t" none "" "" br /x
	[ "$REPLY" = "L ENG-1" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: long with empty title falls back to short form" {
	build_window_label long linear ENG-1 "" "" "" "" br /x
	[ "$REPLY" = "L ENG-1" ]
}

@test "build_window_label: stamped id with empty title uses branch remainder (long)" {
	build_window_label long linear ENG-6011 "" "" "" "" eng-6011-fixservices-dedup-key /x
	[ "$REPLY" = "L ENG-6011 fixservices-dedup-key" ]
	[ "$REPLY_ID" = "L ENG-6011" ]
	[ "$REPLY_REST" = " fixservices-dedup-key" ]
}

@test "build_window_label: plain short = branch basename" {
	build_window_label short "" "" "" "" "" "" feature/fix-login /x
	[ "$REPLY" = "fix-login" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "fix-login" ]
}

@test "build_window_label: plain long = full branch" {
	build_window_label long "" "" "" "" "" "" feature/fix-login /x
	[ "$REPLY" = "feature/fix-login" ]
}

@test "build_window_label: no branch falls back to dir basename" {
	build_window_label long "" "" "" "" "" "" "" /home/noams/proj
	[ "$REPLY" = "proj" ]
}

@test "build_window_label: default branch with no issue falls back to dir basename (long)" {
	build_window_label long "" "" "" "" "" "" main /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "tmux-og" ]
}

@test "build_window_label: master branch with no issue falls back to dir basename (short)" {
	build_window_label short "" "" "" "" "" "" master /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_REST" = "tmux-og" ]
}

@test "build_window_label: default branch with stamped issue keeps issue label, not basename" {
	build_window_label long linear ENG-1 "fix thing" "" "" "" main /home/noams/tmux-og
	[ "$REPLY" = "L ENG-1 fix thing" ]
	[ "$REPLY_ID" = "L ENG-1" ]
}

@test "build_window_label: default branch with PR keeps basename, PR separate" {
	build_window_label long "" "" "" 42 open success main /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_PR" = " S #42" ]
}

@test "build_window_label: default branch with task shows the task, not basename" {
	build_window_label long "" "" "" "" "" "" main /home/noams/tmux-og "" "fix the reflow"
	[ "$REPLY" = "fix the reflow" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "fix the reflow" ]
}

@test "build_window_label: branch-less window with task shows the task (short)" {
	build_window_label short "" "" "" "" "" "" "" /home/noams/proj "" "debug splash flicker"
	[ "$REPLY" = "debug splash flicker" ]
}

@test "build_window_label: empty task on default branch still falls back to basename" {
	build_window_label long "" "" "" "" "" "" main /home/noams/tmux-og "" ""
	[ "$REPLY" = "tmux-og" ]
}

@test "build_window_label: feature branch wins over task" {
	build_window_label short "" "" "" "" "" "" feature/fix-login /x "" "some task"
	[ "$REPLY" = "fix-login" ]
}

@test "build_window_label: issue id wins over task" {
	build_window_label long linear ENG-1 "fix thing" "" "" "" main /x "" "some task"
	[ "$REPLY" = "L ENG-1 fix thing" ]
}

@test "build_window_label: ai_name wins over raw task on default branch" {
	build_window_label long "" "" "" "" "" "" main /home/noams/tmux-og "" "fix the reflow logic again" "reflow-fix"
	[ "$REPLY" = "reflow-fix" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "reflow-fix" ]
}

@test "build_window_label: ai_name used when task is empty (branch-less)" {
	build_window_label short "" "" "" "" "" "" "" /home/noams/proj "" "" "debug-splash"
	[ "$REPLY" = "debug-splash" ]
}

@test "build_window_label: feature branch wins over ai_name" {
	build_window_label short "" "" "" "" "" "" feature/fix-login /x "" "some task" "ai-title"
	[ "$REPLY" = "fix-login" ]
}

@test "build_window_label: issue id wins over ai_name" {
	build_window_label long linear ENG-1 "fix thing" "" "" "" main /x "" "some task" "ai-title"
	[ "$REPLY" = "L ENG-1 fix thing" ]
}

@test "build_window_label: empty ai_name falls through to task" {
	build_window_label long "" "" "" "" "" "" main /home/noams/tmux-og "" "do the thing" ""
	[ "$REPLY" = "do the thing" ]
}

@test "build_window_label: plain branch with merged PR keeps name plain, PR separate (long)" {
	build_window_label long "" "" "" 1921 merged success chore/nango-coding-agent-skill /x
	[ "$REPLY" = "chore/nango-coding-agent-skill" ]
	[ "$REPLY_PR" = " M #1921" ]
}

@test "build_window_label: plain branch with pending PR keeps name plain, PR separate (short)" {
	build_window_label short "" "" "" 1958 open pending feature/fix-login /x
	[ "$REPLY" = "fix-login" ]
	[ "$REPLY_PR" = " P #1958" ]
}

@test "build_window_label: plain branch with no PR is unchanged" {
	build_window_label long "" "" "" none "" "" feature/fix-login /x
	[ "$REPLY" = "feature/fix-login" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: derives linear key from branch (short)" {
	build_window_label short "" "" "" "" "" "" eng-6017-featservices-gmail /x
	[ "$REPLY" = "L ENG-6017" ]
}

@test "build_window_label: derived linear long uses branch remainder as title" {
	build_window_label long "" "" "" "" "" "" eng-6017-featservices-gmail /x
	[ "$REPLY" = "L ENG-6017 featservices-gmail" ]
	[ "$REPLY_ID" = "L ENG-6017" ]
	[ "$REPLY_REST" = " featservices-gmail" ]
}

@test "build_window_label: derived issue + open PR keeps id name, PR separate" {
	build_window_label short "" "" "" 1958 open pending eng-6017-foo /x
	[ "$REPLY" = "L ENG-6017" ]
	[ "$REPLY_PR" = " P #1958" ]
}

@test "build_window_label: derives github number from numeric branch" {
	build_window_label short "" "" "" "" "" "" 247-fix-bug /x
	[ "$REPLY" = "G 247" ]
}

@test "build_window_label: stamped issue id takes precedence over branch" {
	build_window_label short linear ABC-1 "" "" "" "" eng-6017-foo /x
	[ "$REPLY" = "L ABC-1" ]
}

@test "build_window_label: non-issue branch stays bare" {
	build_window_label long "" "" "" "" "" "" chore/nango-coding-agent-skill /x
	[ "$REPLY" = "chore/nango-coding-agent-skill" ]
	[ "$REPLY_ID" = "" ]
}

@test "pr_cache_decision: missing cache always fetches" {
	pr_cache_decision 0 0 "" 0 60 15
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: force fetches even with a fresh cache" {
	pr_cache_decision 1 1 '[{"number":42}]' 1 60 15
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: fresh real-PR cache is served" {
	pr_cache_decision 0 1 '[{"number":42}]' 30 60 15
	[ "$REPLY" = "serve" ]
}

@test "pr_cache_decision: stale real-PR cache fetches on normal TTL" {
	pr_cache_decision 0 1 '[{"number":42}]' 61 60 15
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: empty-array cache uses the shorter TTL_NONE" {
	# Served under TTL_NONE, but a real-PR cache of the same age would expire.
	pr_cache_decision 0 1 '[]' 20 60 15
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: fresh empty-array cache is served within TTL_NONE" {
	pr_cache_decision 0 1 '[]' 5 60 15
	[ "$REPLY" = "serve" ]
}

@test "pr_cache_decision: merged PR is served under TTL_TERMINAL" {
	# Same age expires a real open-PR cache (61 > 60), but merged is terminal.
	pr_cache_decision 0 1 '[{"number":42,"state":"MERGED","title":"x"}]' 61 60 15 3600
	[ "$REPLY" = "serve" ]
}

@test "pr_cache_decision: closed PR is served under TTL_TERMINAL" {
	pr_cache_decision 0 1 '[{"number":42,"state":"CLOSED","title":"x"}]' 61 60 15 3600
	[ "$REPLY" = "serve" ]
}

@test "pr_cache_decision: terminal cache still expires past TTL_TERMINAL" {
	pr_cache_decision 0 1 '[{"number":42,"state":"MERGED","title":"x"}]' 3601 60 15 3600
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: open PR ignores TTL_TERMINAL" {
	pr_cache_decision 0 1 '[{"number":42,"state":"OPEN","title":"x"}]' 61 60 15 3600
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: TTL_TERMINAL defaults to TTL when omitted" {
	pr_cache_decision 0 1 '[{"number":42,"state":"MERGED","title":"x"}]' 61 60 15
	[ "$REPLY" = "fetch" ]
}

@test "pr_cache_decision: escaped state literal in a title is not terminal" {
	# A title containing the text state":"MERGED arrives JSON-escaped (\"), so
	# the raw-quote substring must not match.
	pr_cache_decision 0 1 '[{"number":42,"state":"OPEN","title":"say \"state\":\"MERGED\" loudly"}]' 61 60 15 3600
	[ "$REPLY" = "fetch" ]
}

@test "collapse_check_rollup: pending sets REPLY_PROGRESS to finished/total" {
	collapse_check_rollup '[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""},{"__typename":"StatusContext","state":"PENDING"}]'
	[ "$REPLY" = "pending" ]
	[ "$REPLY_PROGRESS" = "1/3" ]
}

@test "collapse_check_rollup: settled states carry no progress" {
	collapse_check_rollup "$(cat tests/fixtures/rollup-success.json)"
	[ -z "$REPLY_PROGRESS" ]
	collapse_check_rollup "$(cat tests/fixtures/rollup-failure.json)"
	[ -z "$REPLY_PROGRESS" ]
}

@test "collapse_check_rollup: malformed JSON → none, no progress" {
	collapse_check_rollup 'not json'
	[ "$REPLY" = "none" ]
	[ -z "$REPLY_PROGRESS" ]
}

@test "pr_pie_glyph: slice tracks the share finished" {
	pr_pie_glyph 0/8
	[ "$REPLY" = "${ENRICH_PIE_GLYPHS[0]}" ]
	pr_pie_glyph 3/8
	[ "$REPLY" = "${ENRICH_PIE_GLYPHS[2]}" ]
	pr_pie_glyph 7/8
	[ "$REPLY" = "${ENRICH_PIE_GLYPHS[6]}" ]
	pr_pie_glyph 1/3
	[ "$REPLY" = "${ENRICH_PIE_GLYPHS[2]}" ]
}

@test "pr_pie_glyph: unusable progress is empty" {
	local p
	for p in "" 3 0/0 9/8 a/b; do
		pr_pie_glyph "$p"
		[ -z "$REPLY" ]
	done
}

@test "build_window_label: pending PR with progress uses the pie slice" {
	build_window_label short linear ENG-1 "t" 9 open pending br /x mergeable "" "" "" 3/8
	[ "$REPLY_PR" = " ${ENRICH_PIE_GLYPHS[2]} #9" ]
}

@test "build_window_label: pending PR without progress keeps the pending glyph" {
	build_window_label short linear ENG-1 "t" 9 open pending br /x mergeable "" "" "" ""
	[ "$REPLY_PR" = " P #9" ]
}

@test "build_window_label: draft marker sits ahead of the pie" {
	build_window_label short linear ENG-1 "t" 9 open pending br /x mergeable "" "" 1 5/8
	[ "$REPLY_PR" = " D ${ENRICH_PIE_GLYPHS[4]} #9" ]
}

@test "split_pr_badge: glyph half keeps its trailing space, number half starts at #" {
	split_pr_badge " D S #247"
	[ "$REPLY_GLYPH" = " D S " ]
	[ "$REPLY_NUM" = "#247" ]
	split_pr_badge ""
	[ -z "$REPLY_GLYPH" ]
	[ -z "$REPLY_NUM" ]
}
