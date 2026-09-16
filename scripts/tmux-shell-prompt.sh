#!/usr/bin/env bash
# Invoked by the pane-shell-prompt hook (OSC 133;A) with #{q:hook_pane}
# #{q:pane_current_command} #{q:session_name}. Clears an exited agent's state,
# but only when the foreground command at the prompt is no longer an agent: a
# still-running agent emitting a nested prompt (subshell, `!`) reports itself as
# the foreground process-group leader and must not read as "agent gone".

set -euo pipefail

# Guarded so the RAW script still runs under bats, where @lib_claude@ is not
# substituted (same pattern and reason as tmux-reap-pane.sh).
# shellcheck source=/dev/null
if [[ -f "@lib_claude@" ]]; then
	source "@lib_claude@"
else
	exit 0
fi

# The agent manifest, compiled at build time — the same @AGENT_COMMANDS@ list
# tmux-update-icons' sweep uses to arm detection, so the two can't diverge.
AGENT_COMMANDS="${AGENT_COMMANDS:-@AGENT_COMMANDS@}"

# normalize_wrapped_cmd CMD — strip makeWrapper's `.foo-wrapped` shape, exactly
# as tmux-update-icons.sh does. return 0 keeps this safe under set -e (the
# `[[ ]] && …` chain otherwise fails when the name isn't wrapped).
normalize_wrapped_cmd() {
	REPLY="$1"
	[[ $REPLY == .*-wrapped ]] && REPLY="${REPLY#.}" && REPLY="${REPLY%-wrapped}"
	return 0
}

pcc=""
normalize_wrapped_cmd "${2:-}"
pcc="$REPLY"

# Fail closed on an unreadable command; a later prompt self-heals once readable.
[[ -n $pcc ]] || exit 0

# A nested prompt inside a live agent keeps the agent as the foreground command.
case " $AGENT_COMMANDS " in
*" $pcc "*) exit 0 ;;
*) : ;;
esac

claude_clear_agent_state "${1:-}" "${3:-}"
