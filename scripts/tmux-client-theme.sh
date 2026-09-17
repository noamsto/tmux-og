#!/usr/bin/env bash
# Backgrounded by the client-light-theme[40]/client-dark-theme[40] hooks
# (config/tmux.conf.tmpl), which stamp @og_client_theme_want synchronously
# before this job starts. The hook fires on every report, not only on a
# change (measured, #663), so the no-op guard and the lock both live here
# rather than in the hook itself. A theme-toggle run slower than the stale
# window can let a second job steal the lock; that is fine, because every
# job re-reads the newest want before applying.
#
# This handler is tmux-only: it never invokes theme-toggle and never touches
# theme-state.json — that file is theme-toggle's alone (see CLAUDE.md's
# "Theme support" section). It also enforces an ssh-client rule: a report
# from an ssh-attached client is ignored while any non-ssh, non-control-mode
# client is attached to the server (a headless server's own reports ARE
# followed). The rule is enforced authoritatively from a tmux-stamped
# @og_client_theme_client option read fresh every loop iteration, paired
# with @og_client_theme_want; the job's own argv is only a cheap pre-lock
# fast path, never the authority.
set -uo pipefail

# Guarded so the RAW script still runs under bats, where @lib_log@ is not
# substituted (same pattern as tmux-shell-prompt.sh).
lib_log="@lib_log@"
if [[ $lib_log == @* ]]; then
	lib_log="$(dirname "$0")/lib-log.sh"
fi
# shellcheck source=/dev/null
source "$lib_log"

is_ssh_client() {
	local v
	v="$(tmux display-message -c "$1" -p '#{I/e:SSH_CONNECTION}' 2>/dev/null)" || return 1
	[[ -n $v ]]
}
# True if some attached, non-control client OTHER than $1 is not ssh.
# Deliberately excludes only the named reporter, not "all ssh clients" — a
# second ssh client attached alongside the reporter must not count as
# local, and must not suppress detection of a genuine local client either.
any_local_client_attached_besides() {
	local exclude=$1 ctrl name
	while IFS='|' read -r ctrl name; do
		[[ $ctrl == 1 || -z $name || $name == "$exclude" ]] && continue
		is_ssh_client "$name" || return 0
	done < <(tmux list-clients -F '#{client_control_mode}|#{client_name}' 2>/dev/null)
	return 1
}

# Per-user, like the lock it guards. acquire_lock never blocks, so retry
# with a short sleep, bounded by the same staleness window a crashed holder is
# stolen after — past that, this report is dropped, and the next report or
# toggle re-reads the newest want anyway.
lock="${OG_CLIENT_THEME_LOCK:-${TMPDIR:-/tmp}/og-client-theme-$UID.lock}"

client="${1:-}"
if [[ -n $client ]] && is_ssh_client "$client" && any_local_client_attached_besides "$client"; then
	exit 0
fi

locked=0
deadline=$((SECONDS + OG_LOCK_STALE_SECONDS))
while :; do
	acquire_lock "$lock" && {
		locked=1
		break
	}
	((SECONDS < deadline)) || break
	sleep 0.2
done
((locked)) || exit 0

# apply WANTED_FLAVOR
# tmux-only apply: clear @thm_*, set @catppuccin_flavor, replay the reload
# path. NEVER writes theme-state.json — that file is theme-toggle's alone
# (a headless host with no theme-toggle now just never gets a state file;
# the live tmux flavor is authoritative while the server is up). Recovery
# matches when the config is missing, the source fails, or the palette
# never actually loaded — a theme report must never leave the bar colorless.
apply() {
	local wanted_flavor=$1

	# Exactly two tmux forks: one read, one batched clear. catppuccin sets
	# @thm_* with -ogq, so a second flavor never loads over the first once
	# cleared.
	local opt args=() first=1
	while IFS= read -r opt; do
		[[ $opt == @thm_* ]] || continue
		((first)) && first=0 || args+=(\;)
		args+=(set -gu "$opt")
	done < <(tmux show-options -g -F '#{option_name}' 2>/dev/null)
	((${#args[@]})) && tmux "${args[@]}"

	tmux set -g @catppuccin_flavor "$wanted_flavor"

	local conf="$HOME/.config/tmux/tmux.conf" reloaded=0
	if [[ -r $conf ]] && tmux source-file "$conf" 2>/dev/null; then
		reloaded=1
	fi

	if ((! reloaded)) || [[ -z $(tmux show-options -gv @thm_bg 2>/dev/null) ]]; then
		tmux run-shell "@bash@ @catppuccin@"
		tmux run-shell "@apply_theme_colors@"
		tmux run-shell -b "@remote_theme@"
	fi
}

applied=""
while :; do
	follow="" want="" flavor="" reporter=""
	IFS='|' read -r follow want flavor reporter < <(tmux display-message -p \
		'#{@og_follow_client_theme}|#{@og_client_theme_want}|#{@catppuccin_flavor}|#{@og_client_theme_client}')

	[[ $follow == off ]] && break
	case $want in
	light | dark) ;;
	*) break ;;
	esac

	# Authoritative: @og_client_theme_want and @og_client_theme_client are
	# stamped together by the hook body (same command-queue drain), so the
	# reporter paired with THIS want is read fresh every iteration — unlike
	# the fast-path above, which only knows the argv of the job that started
	# this particular background process and can be stale by the time this
	# loop reaches a newer want. A newer report landing between the hook's
	# two `set` commands can pair one read with the wrong reporter; that
	# affects at most one report and self-corrects on the next, same
	# tolerance the existing last-want-wins design already has.
	#
	# A local report immediately followed by a gated ssh report loses the
	# local want (the ssh hook overwrites @og_client_theme_want, so every
	# job gates out here on its next iteration) — acceptable, since the
	# next real report converges.
	#
	# Empty $reporter fails open (report proceeds), matching
	# tmux-splash-maybe.sh's is_remote_attach direction — but "not ssh"
	# here means "apply", the opposite of that script's "not ssh" meaning.
	if [[ -n $reporter ]] && is_ssh_client "$reporter" && any_local_client_attached_besides "$reporter"; then
		break
	fi

	if [[ $want == light ]]; then
		wanted_flavor=latte
	else
		wanted_flavor=mocha
	fi

	# No-op guard: flavor already agrees with the newest want.
	[[ $flavor == "$wanted_flavor" ]] && break

	# Bound: an apply that didn't converge is logged once and never retried
	# in this loop. The next report or toggle retries it.
	if [[ $want == "$applied" ]]; then
		log_event client-theme event nonconverged want "$want"
		break
	fi

	apply "$wanted_flavor"
	applied="$want"
done
