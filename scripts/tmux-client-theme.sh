#!/usr/bin/env bash
# Backgrounded by the client-light-theme[40]/client-dark-theme[40] hooks
# (config/tmux.conf.tmpl), which stamp @og_client_theme_want synchronously
# before this job starts. The hook fires on every report, not only on a
# change (measured, #663), so the no-op guard and the lock both live here
# rather than in the hook itself. A theme-toggle run slower than the stale
# window can let a second job steal the lock; that is fine, because every
# job re-reads the newest want before applying.
set -uo pipefail

# Guarded so the RAW script still runs under bats, where @lib_log@ is not
# substituted (same pattern as tmux-shell-prompt.sh).
lib_log="@lib_log@"
if [[ $lib_log == @* ]]; then
	lib_log="$(dirname "$0")/lib-log.sh"
fi
# shellcheck source=/dev/null
source "$lib_log"

# Per-user, like the state file it guards. acquire_lock never blocks, so retry
# with a short sleep, bounded by the same staleness window a crashed holder is
# stolen after — past that, this report is dropped, and the next report or
# toggle re-reads the newest want anyway.
lock="${OG_CLIENT_THEME_LOCK:-${TMPDIR:-/tmp}/og-client-theme-$UID.lock}"
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

# apply_local WANT WANTED_FLAVOR STATE_FILE
# The theme-toggle-absent path (headless host): write the state file in its
# schema, clear the palette, set the flavor, then replay the one existing
# reload path. Recovery matches when the config is missing, the source fails,
# or the palette never actually loaded — a theme report must never leave the
# bar colorless.
apply_local() {
	local want=$1 wanted_flavor=$2 state=$3

	mkdir -p "$(dirname "$state")" 2>/dev/null
	local tmp
	if tmp=$(mktemp "$state.XXXXXX" 2>/dev/null); then
		if printf '{"theme":"%s","timestamp":"%s","failed":[],"version":1}\n' \
			"$want" "$(date +%Y-%m-%dT%H:%M:%S%z)" >"$tmp" && mv -f "$tmp" "$state"; then
			:
		else
			rm -f "$tmp"
		fi
	fi

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
	follow="" want="" flavor=""
	IFS='|' read -r follow want flavor < <(tmux display-message -p \
		'#{@og_follow_client_theme}|#{@og_client_theme_want}|#{@catppuccin_flavor}')

	[[ $follow == off ]] && break
	case $want in
	light | dark) ;;
	*) break ;;
	esac

	# Fork-free parse, same regex as lib-claude.sh's setup_claude_colors.
	state="${XDG_STATE_HOME:-$HOME/.local/state}/theme-state.json"
	file_theme="dark"
	if [[ -f $state ]]; then
		content=""
		IFS= read -r -d '' content <"$state" 2>/dev/null || true
		[[ $content =~ \"theme\"[[:space:]]*:[[:space:]]*\"([^\"]*)\" ]] && file_theme="${BASH_REMATCH[1]}"
	fi

	if [[ $want == light ]]; then
		wanted_flavor=latte
	else
		wanted_flavor=mocha
	fi

	# No-op guard: file and flavor already agree with the newest want.
	[[ $file_theme == "$want" && $flavor == "$wanted_flavor" ]] && break

	# Bound: an apply that didn't converge is logged once and never retried
	# in this loop. The next report or toggle retries it.
	if [[ $want == "$applied" ]]; then
		log_event client-theme event nonconverged want "$want"
		break
	fi

	if command -v theme-toggle >/dev/null 2>&1; then
		# Not exec: the lock stays held until the loop settles.
		theme-toggle apply "$want" >/dev/null 2>&1
	else
		apply_local "$want" "$wanted_flavor" "$state"
	fi
	applied="$want"
done
