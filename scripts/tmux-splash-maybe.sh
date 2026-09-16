#!/usr/bin/env bash
# Show the welcome splash once, only on a fresh, empty session.
# Fired (backgrounded) from client-attached / client-session-changed hooks.
# $1 = target session name (#{hook_session_name}); falls back to the current
# one. $2 = invoking client name (#{hook_client}).
set -euo pipefail

session="${1:-}"
[ -n "$session" ] || session="$(tmux display-message -p '#{session_name}')"

# Once per tmux server — a global flag, so only the first fresh session after
# server start shows it (not every new session, nor on session switch).
if [ "$(tmux show-option -gqv @splash_shown)" = "1" ]; then exit 0; fi

# The remote bridge attaches a -CC control-mode client. It has no tty, so a
# second popup on it dereferences NULL inside tmux and takes the whole server
# down (#346, upstream tmux/tmux#5551) — and a mirror has no business showing
# a splash. Deliberately without setting @splash_shown: a later real attach
# must still get it.
client="${2:-}"
control=""
while read -r mode name; do
	[ "$name" = "$client" ] && control="$mode" || true
done < <(tmux list-clients -t "$session" -F '#{client_control_mode} #{client_name}' 2>/dev/null)
[ -n "$control" ] || exit 1
[ "$control" = 1 ] && exit 0

# Only a brand-new, single-pane session.
[ "$(tmux display-message -t "$session" -p '#{session_windows}')" = "1" ] || exit 0
[ "$(tmux display-message -t "$session" -p '#{window_panes}')" = "1" ] || exit 0

# Only when the pane is sitting at an interactive shell — never cover a
# tmux-remux–restored program/editor.
case "$(tmux display-message -t "$session" -p '#{pane_current_command}')" in
fish | bash | zsh | sh | dash | nu) ;;
*) exit 0 ;;
esac

# `#{I/e:SSH_CONNECTION}` reads SSH_CONNECTION out of the *client's own*
# environment (`ft->c->environ` in tmux, format.c) directly — strictly more
# correct than the old session-environment-table read, which reflected
# whichever client most recently attached to the session, not necessarily the
# one that triggered this particular call. Interrogating `$client` by name
# removes that ambiguity entirely. An unset var and an interrogation error are
# treated the same (both mean "not remote"), same fail-safe shape as before.
is_remote_attach() {
	local v
	v="$(tmux display-message -c "$client" -p '#{I/e:SSH_CONNECTION}' 2>/dev/null)" || return 1
	[ -n "$v" ]
}

# Baked in at build time from programs.tmux-og.splash.remote (skip|static|full).
if is_remote_attach; then
	# shellcheck disable=SC2194 # @splash_remote@ is a Nix build-time placeholder
	case "@splash_remote@" in
	skip)
		# Don't set @splash_shown: a later local attach on this same server
		# should still get the splash it never got over ssh.
		exit 0
		;;
	static)
		tmux set-option -g @splash_shown 1
		tmux display-popup -E -B -w 100% -h 100% -t "$session" -c "$client" @tmux_splash@ --static
		exit 0
		;;
	esac
fi

tmux set-option -g @splash_shown 1
tmux display-popup -E -B -w 100% -h 100% -t "$session" -c "$client" @tmux_splash@
