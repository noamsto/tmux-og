#!/usr/bin/env bats

load helper

CSU="scripts/claude-status-update.sh"

setup() {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	# Hermetic: ignore the tmux session the developer runs bats from
	unset TMUX TMUX_PANE
}

# Current state string written for a pane file.
pane_state() {
	grep -m1 '^state=' "$CLAUDE_STATUS_DIR/panes/$1" | cut -d= -f2
}

pane_ts() {
	grep -m1 '^timestamp=' "$CLAUDE_STATUS_DIR/panes/$1" | cut -d= -f2
}

@test "issue add: creates issues file with id" {
	bash "$CSU" issue add ENG-123 --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "ENG-123" ]
}

@test "issue add: appends second id comma-separated" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" issue add GH-42 --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "ENG-123,GH-42" ]
}

@test "issue add: dedupes existing id" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" issue add ENG-123 --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "ENG-123" ]
}

@test "issue done: removes id, keeps others" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" issue add GH-42 --pane %7
	bash "$CSU" issue "done" ENG-123 --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "GH-42" ]
}

@test "issue done: removing last id removes the file" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" issue "done" ENG-123 --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/issues/7" ]
}

@test "issue done: missing file is a no-op" {
	run bash "$CSU" issue "done" ENG-123 --pane %7
	[ "$status" -eq 0 ]
}

@test "issue clear: removes the file" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" issue clear --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/issues/7" ]
}

@test "issue add: rejects id with invalid characters" {
	run bash "$CSU" issue add 'ENG#123' --pane %7
	[ "$status" -eq 1 ]
}

@test "issue add: rejects empty id" {
	run bash "$CSU" issue add --pane %7
	[ "$status" -eq 1 ]
}

@test "issue: rejects unknown action" {
	run bash "$CSU" issue frobnicate --pane %7
	[ "$status" -eq 1 ]
}

@test "issue add: no pane id exits 0 silently" {
	run bash "$CSU" issue add ENG-123
	[ "$status" -eq 0 ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues" ] || [ -z "$(ls -A "$CLAUDE_STATUS_DIR/issues")" ]
}

@test "issue done: exact match only — prefix id does not remove longer id" {
	bash "$CSU" issue add ENG-1 --pane %7
	bash "$CSU" issue add ENG-12 --pane %7
	bash "$CSU" issue "done" ENG-1 --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "ENG-12" ]
}

@test "state write does not touch the issues file" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" processing --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/issues/7")" = "ENG-123" ]
}

# "Last active" = time since real work. A passive idle write (the idle_prompt
# notification, a session resume) must NOT reset the clock, or a long-idle
# window keeps reporting ~1m. A real event (done) always stamps fresh.
@test "idle preserves the prior timestamp (idle_prompt must not reset last-active)" {
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	printf 'state=done\ntimestamp=1000000000\nsession=work\n' >"$CLAUDE_STATUS_DIR/panes/7"
	bash "$CSU" idle --pane %7
	[ "$(pane_state 7)" = "idle" ]
	[ "$(pane_ts 7)" = "1000000000" ]
}

@test "done stamps a fresh timestamp (work just finished)" {
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	printf 'state=idle\ntimestamp=1000000000\nsession=work\n' >"$CLAUDE_STATUS_DIR/panes/7"
	bash "$CSU" "done" --pane %7
	[ "$(pane_ts 7)" != "1000000000" ]
}

@test "first idle with no prior file stamps a fresh timestamp" {
	bash "$CSU" idle --pane %7
	[ -n "$(pane_ts 7)" ]
	[ "$(pane_ts 7)" != "1000000000" ]
}

# State guard: only `error` (a stopped turn) is protected from routine
# processing/done writes. waiting/denied must yield to the resume write that
# follows an approved prompt / continued-after-denial, or the clock glyph sticks.
@test "guard: waiting yields to the processing resume after a prompt is approved" {
	bash "$CSU" waiting --pane %7
	bash "$CSU" processing --pane %7
	[ "$(pane_state 7)" = "processing" ]
}

@test "guard: denied yields to the processing resume when work continues" {
	bash "$CSU" denied --pane %7
	bash "$CSU" processing --pane %7
	[ "$(pane_state 7)" = "processing" ]
}

@test "guard: error survives a routine processing write" {
	bash "$CSU" error --pane %7
	bash "$CSU" processing --pane %7
	[ "$(pane_state 7)" = "error" ]
}

@test "guard: error survives a done write (failure stays visible through Stop)" {
	bash "$CSU" error --pane %7
	bash "$CSU" "done" --pane %7
	[ "$(pane_state 7)" = "error" ]
}

@test "guard: --force clears a protected error (a new prompt resets)" {
	bash "$CSU" error --pane %7
	bash "$CSU" processing --force --pane %7
	[ "$(pane_state 7)" = "processing" ]
}

@test "clear state removes pane, issues, and tasks files" {
	bash "$CSU" issue add ENG-123 --pane %7
	bash "$CSU" task set "fix the reflow" --pane %7
	bash "$CSU" processing --pane %7
	bash "$CSU" clear --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/panes/7" ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues/7" ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/7" ]
}

@test "task set: writes the phrase to the tasks file" {
	bash "$CSU" task set "fix the reflow" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "fix the reflow" ]
}

@test "task set: collapses newlines and squeezes whitespace" {
	printf -v multi 'fix   the\nreflow\twindows'
	bash "$CSU" task set "$multi" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "fix the reflow windows" ]
}

@test "task set: truncates to 60 chars" {
	long=$(printf 'x%.0s' {1..100})
	bash "$CSU" task set "$long" --pane %7
	got=$(cat "$CLAUDE_STATUS_DIR/tasks/7")
	[ "${#got}" -eq 60 ]
}

@test "task set: preserves non-ASCII (UTF-8 RTL + emoji)" {
	bash "$CSU" task set "fix שלום 🚀 bug" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "fix שלום 🚀 bug" ]
}

@test "task set: a leading -- is content, not a flag" {
	bash "$CSU" task set "--watch the splash" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "--watch the splash" ]
}

@test "task set: a literal --pane prompt is stored, not parsed as a flag" {
	run bash "$CSU" task set "--pane" --pane %7
	[ "$status" -eq 0 ]
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "--pane" ]
}

@test "task set: overwrites the previous phrase" {
	bash "$CSU" task set "first thing" --pane %7
	bash "$CSU" task set "second thing" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/tasks/7")" = "second thing" ]
}

@test "task set: whitespace-only writes nothing" {
	bash "$CSU" task set "   " --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/7" ]
}

@test "task clear: removes the file" {
	bash "$CSU" task set "fix the reflow" --pane %7
	bash "$CSU" task clear --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/7" ]
}

@test "task: rejects unknown action" {
	run bash "$CSU" task frobnicate --pane %7
	[ "$status" -eq 1 ]
}

@test "task set: no pane id exits 0 silently" {
	run bash "$CSU" task set "fix the reflow"
	[ "$status" -eq 0 ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks" ] || [ -z "$(ls -A "$CLAUDE_STATUS_DIR/tasks")" ]
}

@test "name set: writes the title to the names file" {
	bash "$CSU" name set "Fix the reflow" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/names/7")" = "Fix the reflow" ]
}

@test "name set: strips a leading U+258E quote-bar from a pasted seed" {
	bash "$CSU" name set $'▎ "PR review #2128' --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/names/7")" = 'PR review #2128' ]
}

@test "name set: strips a leading markdown quote marker" {
	bash "$CSU" name set "> do the thing" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/names/7")" = "do the thing" ]
}

@test "name set: preserves interior # and capitalization" {
	bash "$CSU" name set "PR review #2128" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/names/7")" = "PR review #2128" ]
}

@test "name set: maps | to a space" {
	bash "$CSU" name set "foo|bar" --pane %7
	[ "$(cat "$CLAUDE_STATUS_DIR/names/7")" = "foo bar" ]
}

@test "name set: truncates to 40 chars" {
	long=$(printf 'x%.0s' {1..60})
	bash "$CSU" name set "$long" --pane %7
	got=$(cat "$CLAUDE_STATUS_DIR/names/7")
	[ "${#got}" -eq 40 ]
}

@test "name set: decoration-only input writes nothing" {
	bash "$CSU" name set $'▎ " ' --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/names/7" ]
}

@test "name clear: removes the file" {
	bash "$CSU" name set "Fix the reflow" --pane %7
	bash "$CSU" name clear --pane %7
	[ ! -e "$CLAUDE_STATUS_DIR/names/7" ]
}

@test "format_issue_list: no ids yields empty" {
	setup_lib_claude
	format_issue_list 3
	[ -z "$REPLY" ]
}

@test "format_issue_list: under cap joins with spaces" {
	setup_lib_claude
	format_issue_list 3 ENG-1 GH-2
	[ "$REPLY" = "ENG-1 GH-2" ]
}

@test "format_issue_list: exactly at cap has no suffix" {
	setup_lib_claude
	format_issue_list 3 ENG-1 ENG-2 ENG-3
	[ "$REPLY" = "ENG-1 ENG-2 ENG-3" ]
}

@test "format_issue_list: over cap truncates with +N" {
	setup_lib_claude
	format_issue_list 3 ENG-1 ENG-2 ENG-3 ENG-4 ENG-5
	[ "$REPLY" = "ENG-1 ENG-2 ENG-3 +2" ]
}

write_pane_fixture() {
	# write_pane_fixture PANE SESSION [ISSUES]
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/issues"
	printf 'state=processing\ntimestamp=%s\nsession=%s\n' "$(date +%s)" "$2" \
		>"$CLAUDE_STATUS_DIR/panes/$1"
	if [ -n "${3:-}" ]; then
		printf '%s\n' "$3" >"$CLAUDE_STATUS_DIR/issues/$1"
	fi
}

@test "claude-status session: appends issue ids after icon" {
	write_pane_fixture 7 work "ENG-123,GH-42"
	make_claude_status
	run bash "$CLAUDE_STATUS_SCRIPT" --session work --format icon-color
	[ "$status" -eq 0 ]
	[[ $output == *"ENG-123 GH-42"* ]]
}

@test "claude-status session: dedupes ids across panes and caps at 3" {
	write_pane_fixture 7 work "ENG-1,ENG-2"
	write_pane_fixture 8 work "ENG-2,ENG-3,ENG-4,ENG-5"
	make_claude_status
	run bash "$CLAUDE_STATUS_SCRIPT" --session work --format icon-color
	[[ $output == *"ENG-1 ENG-2 ENG-3 +2"* ]]
}

@test "claude-status session: no issues leaves output unchanged" {
	write_pane_fixture 7 work
	make_claude_status
	run bash "$CLAUDE_STATUS_SCRIPT" --session work --format icon-color
	[ "$status" -eq 0 ]
	[[ $output != *"ENG"* ]]
}

@test "claude-status session: other session's issues not shown" {
	write_pane_fixture 7 work "ENG-1"
	write_pane_fixture 8 other "GH-9"
	make_claude_status
	run bash "$CLAUDE_STATUS_SCRIPT" --session work --format icon-color
	[[ $output == *"ENG-1"* ]]
	[[ $output != *"GH-9"* ]]
}

@test "claude-status pane: icon-color shows no issue ids" {
	write_pane_fixture 7 work "ENG-123"
	make_claude_status
	run bash "$CLAUDE_STATUS_SCRIPT" --pane %7 --format icon-color
	[ "$status" -eq 0 ]
	[[ $output != *"ENG-123"* ]]
}

# Stubs tmux so cleanup sees PANE_LIST ("pane_id|current_command"
# lines, '%' optional) as the live panes; all other tmux subcommands no-op.
stub_tmux() {
	mkdir -p "$BATS_TEST_TMPDIR/bin"
	# /bin/sh, not /usr/bin/env: the Nix flake-check sandbox has no /usr/bin.
	cat >"$BATS_TEST_TMPDIR/bin/tmux" <<'EOF'
#!/bin/sh
if [ "$1" = list-panes ]; then printf '%s' "$PANE_LIST"; fi
exit 0
EOF
	chmod +x "$BATS_TEST_TMPDIR/bin/tmux"
	PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

@test "cleanup: keeps stamp for a live pane not running claude" {
	write_pane_fixture 7 work "ENG-123"
	stub_tmux
	PANE_LIST=$'%7|fish\n' bash "$CSU" cleanup
	[ -f "$CLAUDE_STATUS_DIR/issues/7" ]
	[ -f "$CLAUDE_STATUS_DIR/panes/7" ]
}

@test "cleanup: removes stamp for a pane that no longer exists" {
	write_pane_fixture 9 work "GH-42"
	stub_tmux
	PANE_LIST=$'%7|claude\n' bash "$CSU" cleanup
	[ ! -f "$CLAUDE_STATUS_DIR/issues/9" ]
	[ ! -f "$CLAUDE_STATUS_DIR/panes/9" ]
}

@test "cleanup: removes orphan task file for a dead pane" {
	mkdir -p "$CLAUDE_STATUS_DIR/tasks"
	printf 'fix the reflow\n' >"$CLAUDE_STATUS_DIR/tasks/9"
	stub_tmux
	PANE_LIST=$'%7|claude\n' bash "$CSU" cleanup
	[ ! -f "$CLAUDE_STATUS_DIR/tasks/9" ]
}

@test "cleanup: keeps task file for a live pane" {
	mkdir -p "$CLAUDE_STATUS_DIR/tasks"
	printf 'fix the reflow\n' >"$CLAUDE_STATUS_DIR/tasks/7"
	stub_tmux
	PANE_LIST=$'%7|fish\n' bash "$CSU" cleanup
	[ -f "$CLAUDE_STATUS_DIR/tasks/7" ]
}

# An empty list-panes (server hiccup, or CC running outside tmux so the query
# hits the wrong/no server) must never read as "every pane is gone" — that would
# wipe live panes' status files in one sweep and blank their status icons.
@test "cleanup: empty pane list keeps all live files" {
	write_pane_fixture 7 work "ENG-123"
	mkdir -p "$CLAUDE_STATUS_DIR/tasks"
	printf 'fix the reflow\n' >"$CLAUDE_STATUS_DIR/tasks/7"
	stub_tmux
	PANE_LIST='' bash "$CSU" cleanup
	[ -f "$CLAUDE_STATUS_DIR/panes/7" ]
	[ -f "$CLAUDE_STATUS_DIR/issues/7" ]
	[ -f "$CLAUDE_STATUS_DIR/tasks/7" ]
}

@test "cleanup: removes screen file for a pane that no longer exists" {
	mkdir -p "$CLAUDE_STATUS_DIR/screen"
	printf 'pane=7\nscreens=[]\n' >"$CLAUDE_STATUS_DIR/screen/9"
	stub_tmux
	PANE_LIST=$'%7|claude\n' bash "$CSU" cleanup
	[ ! -f "$CLAUDE_STATUS_DIR/screen/9" ]
}

@test "cleanup: keeps screen file for a live pane" {
	mkdir -p "$CLAUDE_STATUS_DIR/screen"
	printf 'pane=7\nscreens=[]\n' >"$CLAUDE_STATUS_DIR/screen/7"
	stub_tmux
	PANE_LIST=$'%7|fish\n' bash "$CSU" cleanup
	[ -f "$CLAUDE_STATUS_DIR/screen/7" ]
}

@test "clear state removes screen file" {
	mkdir -p "$CLAUDE_STATUS_DIR/screen"
	printf 'pane=7\nscreens=[]\n' >"$CLAUDE_STATUS_DIR/screen/7"
	bash "$CSU" clear --pane %7
	[ ! -f "$CLAUDE_STATUS_DIR/screen/7" ]
}
