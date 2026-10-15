#!/usr/bin/env bash
# Fan a light/dark toggle out to every mirrored remote.
#
# A mirror pane is a renderer replaying the remote pane's bytes, so its colours
# were chosen on the remote from the remote's own theme state. A local toggle
# re-themes the frame (status line, borders, pickers) and leaves the content in
# the other flavour until the remote applies the same theme.
#
# The toggle already re-sources the config, so the config's tail calls this and
# the change detection lives here: the flavour is compared against the stamp
# from the last fan-out, making `prefix + r` and an activation reload no-ops.
set -uo pipefail

# Pinned at build time for the reason og-remote-open spells out: the tmux
# server's PATH is frozen until a restart, so a bare name can reach a stale ctl.
ctl="@bridge_ctl@"
[[ $ctl == @* ]] && ctl="$(command -v og-remote-bridge-ctl)"

flavor="$(tmux show-options -gv @catppuccin_flavor 2>/dev/null)"
if [[ $flavor == "latte" ]]; then
	theme="light"
else
	theme="dark"
fi

[[ $(tmux show-options -gv @og_theme_applied 2>/dev/null) == "$theme" ]] && exit 0
tmux set-option -g @og_theme_applied "$theme"

# One verb per daemon socket: a host session may carry several placed mirrors,
# each with its own daemon. The verb is daemon-wide, and the pane only rides
# along because every verb takes one, so the first mirrored pane of a socket is
# enough. Split on the last | (a socket path may hold one, a pane id never does).
declare -A sent
while IFS= read -r row; do
	sock="${row%|*}"
	pane="${row##*|}"
	[[ -n $sock && -n $pane && -z ${sent[$sock]:-} ]] || continue
	sent[$sock]=1
	"$ctl" --sock "$sock" theme "$pane" "$theme" >/dev/null 2>&1
done < <(tmux list-panes -a -F '#{@bridge_sock}|#{@bridge_pane}' 2>/dev/null)

# An unreachable daemon leaves one mirror in the old flavour; it does not make
# the reload that called this a failure.
exit 0
