#!/usr/bin/env bash
# Degrade gracefully: tmux-og not installed, or CC running outside a tmux-og
# tmux pane → silently no-op instead of erroring on every hook event.
command -v claude-status-update >/dev/null 2>&1 || exit 0

# `task`: capture the latest user prompt (UserPromptSubmit hook delivers it as
# JSON on stdin) as the pane's freeform task label. Needs jq to parse; no-op
# without it rather than mangling the prompt.
if [[ ${1:-} == task ]]; then
	command -v jq >/dev/null 2>&1 || exit 0
	prompt=$(jq -r '.prompt // empty' 2>/dev/null) || exit 0
	[[ -n $prompt ]] || exit 0
	# Claude Code re-injects non-user text through UserPromptSubmit — background-task
	# completion notices, system reminders, bang/slash-command envelopes — each
	# wrapped in a hyphenated XML-ish tag (<task-notification>, <system-reminder>,
	# <local-command-stdout>…). Captured as the task label / name seed they produce
	# garbage like "task-notification> <task-id>…", so skip the hook for them.
	# The pattern lives in a variable so its <> stay literal regex chars: inline
	# in [[ =~ ]] bash parses bare <> as operators, and escaping them (\< \>)
	# turns them into GNU word-boundary anchors that match a bare leading word
	# and miss the real <tag>.
	synthetic_tag='^[[:space:]]*<[a-z]+-[a-z-]+>'
	[[ $prompt =~ $synthetic_tag ]] && exit 0
	claude-status-update task set "$prompt"

	# Window naming. On a fallback window (no tracked issue, on the default branch)
	# that has no name yet: seed a mechanical title from this prompt, then nudge
	# the pane's Claude — which has full conversation context — to upgrade it to a
	# concise one. The seed is the instant, readable floor; the nudge is a bonus.
	# Crucially the seed populates @window_ai_name, which flips the `-z $ai_name`
	# gate below for every later prompt — so the nudge fires exactly once instead
	# of taxing every turn. Gated by the @ai_naming global (set from
	# programs.tmux-og.aiNaming.enable); enriched/worktree windows name themselves
	# from issue+branch and are skipped here.
	[[ -n ${TMUX_PANE:-} ]] || exit 0
	command -v tmux >/dev/null 2>&1 || exit 0
	IFS='|' read -r ai_naming issue_id branch ai_name < <(
		tmux display-message -t "$TMUX_PANE" -p \
			'#{@ai_naming}|#{@issue_id}|#{@branch}|#{@window_ai_name}' 2>/dev/null
	)
	[[ $ai_naming == 1 ]] || exit 0
	[[ -z $issue_id ]] || exit 0
	[[ -z $branch || $branch == main || $branch == master ]] || exit 0
	[[ -z $ai_name ]] || exit 0
	# Seed from the prompt (claude-status-update sanitizes + caps to 40 cells).
	claude-status-update name set "$prompt"
	cat <<'REMINDER'
<system-reminder>
This tmux window has only a placeholder name (your raw prompt) and is not tied
to an issue or branch. If the current task is clear, replace it with a concise
title (3-6 words) by running once:
  claude-status-update name set "your title here"
Name only the window you are working in. Skip if the task is still vague — the
placeholder stays. If the focus later shifts to clearly different work, run it
again with a new title.
</system-reminder>
REMINDER
	exit 0
fi

# Forward the hook's transcript path so claude-status-update can record it for
# interrupt detection. Esc-interrupts fire no hook, so the only durable trace is
# a marker line in the transcript; read_pane_state tails this path to find it.
# Fork-free slurp + regex (no jq dependency), mirroring the theme-file parse.
input=""
IFS= read -r -d '' input || true
transcript=""
[[ $input =~ \"transcript_path\"[[:space:]]*:[[:space:]]*\"([^\"]*)\" ]] && transcript="${BASH_REMATCH[1]}"

# A `Stop` hook fires both when the turn really ends and when it parks to wait
# for background work it launched. Claude Code lists that work in the payload's
# `background_tasks`; a running `subagent` entry means the Task call is still
# going, so `done` here would drop the spinner while the subagent works — keep
# `processing`. Only the friendly `subagent` type, not `shell`: a background
# shell is a badge (`bg=N`), not agent work. jq is optional (the `task` hook
# already needs it); without it the write stays `done`, as before.
if [[ ${1:-} == "done" && $input == *background_tasks* ]] && command -v jq >/dev/null 2>&1; then
	subagents=$(jq -r '[.background_tasks[]? | select(.type == "subagent")] | length' <<<"$input" 2>/dev/null) || subagents=0
	[[ ${subagents:-0} -gt 0 ]] && set -- processing "${@:2}"
fi

if [[ -n $transcript ]]; then
	exec claude-status-update "$@" --transcript "$transcript"
fi
exec claude-status-update "$@"
