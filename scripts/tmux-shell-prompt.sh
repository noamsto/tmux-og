#!/usr/bin/env bash
# Invoked by the pane-shell-prompt hook (OSC 133;A) with #{q:hook_pane}
# #{qs:pane_current_command} #{qs:session_name} #{q:window_id}
# #{?#{@window_has_agent},1,0} #{?#{@bridge_win},1,0} — both booleans go through
# `#{?…,1,0}` because an unset user option makes `#{q:…}` expand to nothing, not
# an empty word, so a bare trailing option would vanish and shift the next arg
# into its slot. Clears an exited agent's state, but only when the foreground
# command at the prompt is no longer an agent: a still-running agent emitting a
# nested prompt (subshell, `!`) reports itself as the foreground process-group
# leader and must not read as "agent gone". A @bridge_win mirror window is
# skipped whole before that clear (#741): its pane state is daemon-owned.
#
# Also the event trigger for #671: when this was the window's last live
# agent, resets the window's naming/crew display state (never
# @crew_name/@crew_color themselves — dispatcher-owned, CLAUDE.md hard
# constraint). It takes only the *option* half of that reset and stamps
# @window_naming_dirty: this hook is server-side, so it fires with or without
# an attached client, and CLAUDE_STATUS_DIR is a bare /tmp path shared by every
# tmux server on the machine — the rm's stay on the client-gated per-tick pass
# (#692), which clears the mark last. tmux-update-icons.sh is the ground truth
# for whatever this event path can't reach: its per-tick loop for a client-less
# window or a shell with no OSC 133 support, and since #692 its
# client-independent @og-sweep-tick pass, which is what writes
# @window_has_agent on a host whose only clients are control-mode remote-bridge
# transports — so this path is armed there too, not inert.

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

# Reflow seam, pinned to the store path for the reason tmux-update-icons' own
# @reflow@ is: a bare name resolves against the tmux server's frozen PATH and
# stays stale until a server restart (#336). Still starting with '@' means the
# placeholder was never substituted, and disables the forced reflow.
REFLOW_BIN="@reflow@"

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

# A @bridge_win mirror window is daemon-owned (#741): its panes run the bridge
# renderer, and all pane state — panes/, screen/, interrupt/, @claude_status,
# @agent_screen — is shipped by the bridge daemon under the LOCAL pane id. A
# renderer re-emitting the remote shell's OSC 133 would otherwise reach
# claude_clear_agent_state and delete that state; the daemon's unchanged-row
# suppression means it is then never rewritten until the remote value next
# changes. $6 is #{?#{@bridge_win},1,0}, passed by the hook like @window_has_agent
# so this skip costs no fork on the every-prompt path.
[[ ${6:-} == 1 ]] && exit 0

claude_clear_agent_state "${1:-}" "${3:-}"

# Window-wide naming/crew reset (#671). Gated on the passed-through
# @window_has_agent (hook-fire-time value, no fork) first: pane-shell-prompt
# fires on every prompt redraw of every shell pane, not just on agent exit, so
# an unconditional window-wide scan here would fork on every prompt draw in
# every plain shell pane in the fleet.
window_id="${4:-}"
[[ ${5:-} == 1 && -n $window_id ]] || exit 0

manual=$(tmux display-message -p -t "$window_id" '#{@window_manual_name}')

still_has_agent=""
while IFS='|' read -r p_id p_cmd; do
	[[ -n $p_id ]] || continue
	normalize_wrapped_cmd "$p_cmd"
	case " $AGENT_COMMANDS " in *" $REPLY "*)
		still_has_agent=1
		break
		;;
	esac
done < <(tmux list-panes -t "$window_id" -F '#{pane_id}|#{pane_current_command}')

[[ -n $still_has_agent ]] && exit 0

# Option half only (#692), then the deletion owed. Stamp the mark BEFORE the
# clear so a crash between the two writes still leaves the deletion owed;
# claude_clear_window_naming (the client-gated per-tick pass) clears it last,
# after the files are gone.
tmux set -qw -t "$window_id" @window_naming_dirty 1
claude_clear_window_display "$window_id" "$manual"

if [[ $REFLOW_BIN != @* ]]; then
	"$REFLOW_BIN" "${3:-}" --force >/dev/null 2>&1 &
fi
