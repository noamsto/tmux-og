#!/usr/bin/env bash
# The launcher's logic lives in picker/remoteopen (built as og-remote-launcher).
# This shim exists so mkRemoteScript can substitute store paths (the
# remote_launcher, bridge_ctl, bridge_daemon, bridge_renderer, reflow and
# loading placeholders), which a Go binary can't carry. Unsubstituted
# placeholders (bats) fall back to PATH, and OG_REMOTE_LAUNCHER is consulted
# only then. exec keeps the pid so signals and the picker's process-group TERM
# reach the launcher. --self is what the daemon's hand-off re-enters (#336).
launcher="@remote_launcher@"
[[ $launcher == @* ]] && launcher="${OG_REMOTE_LAUNCHER:-$(command -v og-remote-launcher || true)}"
if [[ -z $launcher ]]; then
	echo "og-remote-open: og-remote-launcher not found (build picker/ or set OG_REMOTE_LAUNCHER)" >&2
	exit 1
fi
exec "$launcher" --self "${BASH_SOURCE[0]}" --ctl "@bridge_ctl@" --daemon "@bridge_daemon@" --renderer "@bridge_renderer@" --reflow "@reflow@" --loading "@loading@" -- "$@"
