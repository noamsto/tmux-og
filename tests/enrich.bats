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

# Decodes exactly the escapes \r \n \e \t \\ (the set decodeTSV in
# picker/prenrich handles) into REPLY.
_unescape_tsv() {
	local rest=$1 out=
	while [[ $rest == *\\* ]]; do
		out+=${rest%%\\*}
		rest=${rest#*\\}
		case ${rest:0:1} in
		r) out+=$'\r' ;;
		n) out+=$'\n' ;;
		e) out+=$'\033' ;;
		t) out+=$'\t' ;;
		\\) out+=$'\\' ;;
		*) out+="\\${rest:0:1}" ;;
		esac
		rest=${rest:1}
	done
	REPLY=$out$rest
}

@test "sanitize_title: shared vectors" {
	local vectors=${SANITIZE_TITLE_VECTORS:-$BATS_TEST_DIRNAME/../picker/prenrich/testdata/sanitize-title.tsv}
	local utf8=0 name input want got rows=0
	case ${LC_ALL:-${LC_CTYPE:-${LANG:-}}} in
	*UTF-8* | *utf8*) utf8=1 ;;
	esac
	while IFS=$'\t' read -r name input want; do
		[ -n "$name" ] || continue
		if [[ $name == utf8-* && $utf8 -eq 0 ]]; then continue; fi
		_unescape_tsv "$input"
		input=$REPLY
		_unescape_tsv "$want"
		want=$REPLY
		sanitize_title "$input"
		got=$REPLY
		if [ "$got" != "$want" ]; then
			echo "$name: got [$got] want [$want]"
			return 1
		fi
		rows=$((rows + 1))
	done <"$vectors"
	[ "$rows" -gt 0 ]
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
	build_window_label short linear ENG-1957 "refactor services" "" "" feat/eng-1957 /x
	[ "$REPLY" = "L ENG-1957" ]
	[ "$REPLY_ID" = "L ENG-1957" ]
	[ "$REPLY_REST" = "" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: enriched long = provider id title" {
	build_window_label long linear ENG-1957 "refactor services" "" "" feat/eng-1957 /x
	[ "$REPLY" = "L ENG-1957 refactor services" ]
	[ "$REPLY_ID" = "L ENG-1957" ]
	[ "$REPLY_REST" = " refactor services" ]
}

@test "build_window_label: PR segment is the glyph then #number" {
	build_window_label short linear ENG-1 "t" 9 "D S" br /x
	[ "$REPLY_PR" = " D S #9" ]
}

@test "build_window_label: an empty glyph leaves only the #number" {
	build_window_label short linear ENG-1 "t" 9 "" br /x
	[ "$REPLY_PR" = " #9" ]
}

@test "build_window_label: pr_number=none is treated as no PR" {
	build_window_label short linear ENG-1 "t" none "" br /x
	[ "$REPLY" = "L ENG-1" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: long with empty title falls back to short form" {
	build_window_label long linear ENG-1 "" "" "" br /x
	[ "$REPLY" = "L ENG-1" ]
}

@test "build_window_label: stamped id with empty title uses branch remainder (long)" {
	build_window_label long linear ENG-6011 "" "" "" eng-6011-fixservices-dedup-key /x
	[ "$REPLY" = "L ENG-6011 fixservices-dedup-key" ]
	[ "$REPLY_ID" = "L ENG-6011" ]
	[ "$REPLY_REST" = " fixservices-dedup-key" ]
}

@test "build_window_label: plain short = branch basename" {
	build_window_label short "" "" "" "" "" feature/fix-login /x
	[ "$REPLY" = "fix-login" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "fix-login" ]
}

@test "build_window_label: plain long = full branch" {
	build_window_label long "" "" "" "" "" feature/fix-login /x
	[ "$REPLY" = "feature/fix-login" ]
}

@test "build_window_label: no branch falls back to dir basename" {
	build_window_label long "" "" "" "" "" "" /home/noams/proj
	[ "$REPLY" = "proj" ]
}

@test "build_window_label: default branch with no issue falls back to dir basename (long)" {
	build_window_label long "" "" "" "" "" main /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "tmux-og" ]
}

@test "build_window_label: master branch with no issue falls back to dir basename (short)" {
	build_window_label short "" "" "" "" "" master /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_REST" = "tmux-og" ]
}

@test "build_window_label: default branch with stamped issue keeps issue label, not basename" {
	build_window_label long linear ENG-1 "fix thing" "" "" main /home/noams/tmux-og
	[ "$REPLY" = "L ENG-1 fix thing" ]
	[ "$REPLY_ID" = "L ENG-1" ]
}

@test "build_window_label: default branch with PR keeps basename, PR separate" {
	build_window_label long "" "" "" 42 "S" main /home/noams/tmux-og
	[ "$REPLY" = "tmux-og" ]
	[ "$REPLY_PR" = " S #42" ]
}

@test "build_window_label: default branch with task shows the task, not basename" {
	build_window_label long "" "" "" "" "" main /home/noams/tmux-og "fix the reflow"
	[ "$REPLY" = "fix the reflow" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "fix the reflow" ]
}

@test "build_window_label: branch-less window with task shows the task (short)" {
	build_window_label short "" "" "" "" "" "" /home/noams/proj "debug splash flicker"
	[ "$REPLY" = "debug splash flicker" ]
}

@test "build_window_label: empty task on default branch still falls back to basename" {
	build_window_label long "" "" "" "" "" main /home/noams/tmux-og ""
	[ "$REPLY" = "tmux-og" ]
}

@test "build_window_label: feature branch wins over task" {
	build_window_label short "" "" "" "" "" feature/fix-login /x "some task"
	[ "$REPLY" = "fix-login" ]
}

@test "build_window_label: issue id wins over task" {
	build_window_label long linear ENG-1 "fix thing" "" "" main /x "some task"
	[ "$REPLY" = "L ENG-1 fix thing" ]
}

@test "build_window_label: ai_name wins over raw task on default branch" {
	build_window_label long "" "" "" "" "" main /home/noams/tmux-og "fix the reflow logic again" "reflow-fix"
	[ "$REPLY" = "reflow-fix" ]
	[ "$REPLY_ID" = "" ]
	[ "$REPLY_REST" = "reflow-fix" ]
}

@test "build_window_label: ai_name used when task is empty (branch-less)" {
	build_window_label short "" "" "" "" "" "" /home/noams/proj "" "debug-splash"
	[ "$REPLY" = "debug-splash" ]
}

@test "build_window_label: feature branch wins over ai_name" {
	build_window_label short "" "" "" "" "" feature/fix-login /x "some task" "ai-title"
	[ "$REPLY" = "fix-login" ]
}

@test "build_window_label: issue id wins over ai_name" {
	build_window_label long linear ENG-1 "fix thing" "" "" main /x "some task" "ai-title"
	[ "$REPLY" = "L ENG-1 fix thing" ]
}

@test "build_window_label: empty ai_name falls through to task" {
	build_window_label long "" "" "" "" "" main /home/noams/tmux-og "do the thing" ""
	[ "$REPLY" = "do the thing" ]
}

@test "build_window_label: plain branch with merged PR keeps name plain, PR separate (long)" {
	build_window_label long "" "" "" 1921 "M" chore/coding-agent-skill /x
	[ "$REPLY" = "chore/coding-agent-skill" ]
	[ "$REPLY_PR" = " M #1921" ]
}

@test "build_window_label: plain branch with pending PR keeps name plain, PR separate (short)" {
	build_window_label short "" "" "" 1958 "P" feature/fix-login /x
	[ "$REPLY" = "fix-login" ]
	[ "$REPLY_PR" = " P #1958" ]
}

@test "build_window_label: plain branch with no PR is unchanged" {
	build_window_label long "" "" "" none "" feature/fix-login /x
	[ "$REPLY" = "feature/fix-login" ]
	[ "$REPLY_PR" = "" ]
}

@test "build_window_label: derives linear key from branch (short)" {
	build_window_label short "" "" "" "" "" eng-6017-featservices-gmail /x
	[ "$REPLY" = "L ENG-6017" ]
}

@test "build_window_label: derived linear long uses branch remainder as title" {
	build_window_label long "" "" "" "" "" eng-6017-featservices-gmail /x
	[ "$REPLY" = "L ENG-6017 featservices-gmail" ]
	[ "$REPLY_ID" = "L ENG-6017" ]
	[ "$REPLY_REST" = " featservices-gmail" ]
}

@test "build_window_label: derived issue + open PR keeps id name, PR separate" {
	build_window_label short "" "" "" 1958 "P" eng-6017-foo /x
	[ "$REPLY" = "L ENG-6017" ]
	[ "$REPLY_PR" = " P #1958" ]
}

@test "build_window_label: derives github number from numeric branch" {
	build_window_label short "" "" "" "" "" 247-fix-bug /x
	[ "$REPLY" = "G 247" ]
}

@test "build_window_label: stamped issue id takes precedence over branch" {
	build_window_label short linear ABC-1 "" "" "" eng-6017-foo /x
	[ "$REPLY" = "L ABC-1" ]
}

@test "build_window_label: non-issue branch stays bare" {
	build_window_label long "" "" "" "" "" chore/coding-agent-skill /x
	[ "$REPLY" = "chore/coding-agent-skill" ]
	[ "$REPLY_ID" = "" ]
}

@test "split_pr_badge: glyph half keeps its trailing space, number half starts at #" {
	split_pr_badge " D S #247"
	[ "$REPLY_GLYPH" = " D S " ]
	[ "$REPLY_NUM" = "#247" ]
	split_pr_badge ""
	[ -z "$REPLY_GLYPH" ]
	[ -z "$REPLY_NUM" ]
}
