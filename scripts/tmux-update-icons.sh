#!/usr/bin/env bash
# Lightweight icon updater called via #() every status-interval.
# Updates @window_icon_display (unpadded, for window names / top-right)
# and @window_icon_padded (fixed-width, for status bar alignment).
# Includes colored claude status icon in both variables.
# Outputs nothing (side-effect only).

# shellcheck source=/dev/null  # Nix store paths substituted at build time
source @lib_icons@
# shellcheck source=/dev/null
source @lib_claude@

# Derived at build time from the shipped manifests' match_commands (agentCommands
# in config/tmux.conf.nix), so a manifest can't ship without being swept.
# ${AGENT_COMMANDS:-...} lets tests inject a list, same as AGENT_DETECT_BIN below.
AGENT_COMMANDS="${AGENT_COMMANDS:-@AGENT_COMMANDS@}"
# ${AGENT_DETECT_BIN:-...} lets tests inject a real path via env; Nix build
# substitution still wins in the shipped script (no env var set at runtime).
AGENT_DETECT_BIN="${AGENT_DETECT_BIN:-@agent_detect_bin@}"
# Empty when enrich is disabled (Nix build-time substitution, same as
# tmux-reconcile-window's issue-stamp wiring); ${ISSUE_STAMP_BIN:-...} likewise
# lets tests inject a real path via env.
ISSUE_STAMP_BIN="${ISSUE_STAMP_BIN:-@issue_stamp@}"
# Same shape: empty (unsubstituted @*) means the cwd-move re-stamp (#596) below
# is off; ${RECONCILE_BIN:-...} likewise lets tests inject a real path via env.
RECONCILE_BIN="${RECONCILE_BIN:-@reconcile@}"
# Store path to tmux-carousel-restore, substituted at Nix build time when the
# carousel viewer package is wired in (left as "@carousel_restore@" otherwise —
# see the RESUME_CAROUSEL guard below, which never reaches an unsubstituted
# placeholder). ${CAROUSEL_RESTORE_BIN:-...} is the same test-seam shape as
# AGENT_DETECT_BIN above.
CAROUSEL_RESTORE_BIN="${CAROUSEL_RESTORE_BIN:-@carousel_restore@}"
# Store path to tmux-reflow-windows. The per-session forced reflow at the loop
# tail and the #692 occupancy sweep both call it from here, so a config reload
# repoints them without a server restart. ${REFLOW_BIN:-...} is the same
# test-seam shape as AGENT_DETECT_BIN above; an unsubstituted @reflow@ disables
# the forced reflow rather than exec'ing a literal placeholder.
REFLOW_BIN="${REFLOW_BIN:-@reflow@}"

# normalize_wrapped_cmd CMD
# Strips nix makeWrapper's `.foo-wrapped` shape down to `foo` (what
# pane_current_command reports for every agent CLI on a nix host); passes
# unwrapped names through unchanged. Sets REPLY.
normalize_wrapped_cmd() {
	REPLY="$1"
	[[ $REPLY == .*-wrapped ]] && REPLY="${REPLY#.}" && REPLY="${REPLY%-wrapped}"
}

# under PATH BASE
# True when PATH is BASE itself, or nested under it — mirrors the awk under()
# in tmux-worktree-match.sh. A nested .worktrees/ checkout belongs to a
# different branch, and a plain prefix match would also read
# "/x/tmux-og-old" as inside "/x/tmux-og". False when either is empty.
under() {
	local p="$1" base="$2"
	[[ -z $p || -z $base ]] && return 1
	[[ $p == "$base" ]] && return 0
	[[ $p == "$base"/* && $p != "$base"/.worktrees/* ]]
}

# Arms `agent-detect` on agent panes that don't already have a live pipe,
# stamps each agent pane's presence for lib-claude's dead-agent floor, and (for
# the per-tick status-format caller only, see below) reaps claude-status state
# as a ~60s backstop for death paths no pane hook fires on (kill-pane /
# kill-window / kill-session / respawn-pane -k — measured) — a third job riding
# the same list-panes roundtrip. pane-exited/pane-died own the common
# process-exit path. Using #{pane_pipe} as the gate means a dead parser (pipe
# closes -> pane_pipe 0) self-heals on a later tick. Arm/stamp gate
# independently of each other and of the reap.
arm_agent_detect() {
	local arm=1 stamp=0
	[[ $AGENT_DETECT_BIN == @* ]] && arm=0
	[[ -n ${CLAUDE_LIVE_DIR:-} && ${CLAUDE_ASSUME_DEAD_AFTER:-0} =~ ^[0-9]+$ ]] &&
		((CLAUDE_ASSUME_DEAD_AFTER > 0)) && stamp=1
	# The sweep is a full-server list-panes — a second tmux roundtrip per tick,
	# multiplied by attached sessions. Arming (new pane, dead pipe) only needs
	# seconds-level latency, so run every 5th tick (CLAUDE_NOW = this tick's
	# epoch second). The monitor hook that drives the sweep already fires on
	# its own 5s cadence, so a non-empty $1 skips this throttle there — under
	# that driver the modulo is a phase filter on a clock nobody aligns, not a
	# rate limit, and it would silently stop arming on a drifted residue.
	if [[ -z ${1:-} ]] && ((CLAUDE_NOW % 5)); then
		return 0
	fi

	# Bail on a failed list-panes rather than reading an empty stream: an empty
	# result is indistinguishable from "no agent panes", and stamping .sweep
	# after one would assert a pass that never observed anything — the reader
	# would then read every live pane's lagging stamp as a dead agent.
	#
	# #{window_id}/#{@bridge_win}/#{@window_has_agent}/#{@window_manual_name}
	# plus the naming options ride the same roundtrip for the #692 occupancy
	# pass below — no second call, no separate state. #{window_id} is the row's
	# canary (only a real window id matches ^@[0-9]+$, and #{pane_current_command}
	# — the one field ahead of it that is not a closed token — can in principle
	# carry a '|' and shift it into something that fails the match).
	# #{@window_task} and #{@window_ai_name} are both free-form, so their row
	# copies run through tmux's s/[|]/ / substitution (the bracket expression
	# is load-bearing — a bare s/|/ / is an ERE empty alternation) to keep them
	# pipe-free: the canary above sits before them, so a '|' here would shift
	# task/session_name with nothing left to catch it. #{session_name} is last
	# because it may contain '|'.
	local rows
	rows=$(tmux list-panes -a -F '#{pane_id}|#{pane_current_command}|#{pane_pipe}|#{window_id}|#{@bridge_win}|#{@window_has_agent}|#{@window_manual_name}|#{s/[|]/ /:@window_ai_name}|#{s/[|]/ /:@window_task}|#{session_name}' 2>/dev/null) || return 0
	# claude_reap_dead_panes deletes under CLAUDE_STATUS_DIR -- a bare /tmp path
	# shared by every tmux server on the machine, which TMUX_TMPDIR/-L isolation
	# does not touch -- by checking each pane id against THIS CALLER's own
	# list-panes -a, so it cannot tell whose state it is deleting. The per-tick
	# caller ($1 empty) runs only while this server has a real client drawing a
	# status line, and only every ~60s: the hooks own the common path, this is
	# the structural-kill backstop. Arming and live/ presence stamping keep the
	# 5s cadence (CLAUDE_LIVE_SWEEP_FRESH=15 is calibrated to it). The sweep
	# caller ($1 non-empty) runs on a client-independent timer on every
	# wrapped-tmux server, so a second scratch server would continuously wipe
	# the real server's live agent state — it must never reap. Arming below is
	# non-destructive and stays unconditional.
	if [[ -z ${1:-} ]] && ((CLAUDE_NOW % 60 == 0)); then
		claude_reap_dead_panes "$rows"
	fi

	if ((stamp)) && [[ ! -d $CLAUDE_LIVE_DIR ]]; then
		mkdir -p "$CLAUDE_LIVE_DIR"
	fi

	# Per-window occupancy (#692), accumulated in the same pass. With neither
	# arming nor stamping this loop now only does the fork-free command match
	# (the old `((arm || stamp)) || return 0` short-circuit is gone) so the sweep
	# caller can reconcile occupancy from the same rows.
	local -A win_has=() win_cur=() win_manual=() win_bridge=() win_sess=() win_ai=() win_task=()
	local pid cmd piped wid bridge ha manual ai task sname
	while IFS='|' read -r pid cmd piped wid bridge ha manual ai task sname; do
		# A here-string of an empty result still yields one blank line.
		[[ -n $pid ]] || continue
		# A shifted row (see the format comment above) leaves nothing here, so
		# it contributes no occupancy and no stale-naming clear — fail closed.
		if [[ $wid =~ ^@[0-9]+$ ]]; then
			win_cur[$wid]="$ha"
			win_manual[$wid]="$manual"
			win_bridge[$wid]="$bridge"
			win_sess[$wid]="$sname"
			win_ai[$wid]="$ai"
			win_task[$wid]="$task"
		fi
		normalize_wrapped_cmd "$cmd"
		case " $AGENT_COMMANDS " in *" $REPLY "*) ;; *) continue ;; esac
		[[ $wid =~ ^@[0-9]+$ ]] && win_has[$wid]=1
		((stamp)) && printf '%s\n' "$CLAUDE_NOW" >"$CLAUDE_LIVE_DIR/${pid#%}"
		[[ $piped == 0 ]] || continue
		((arm)) && tmux pipe-pane -o -t "$pid" "$AGENT_DETECT_BIN ${pid#%}"
	done <<<"$rows"

	# Strictly after the last per-pane stamp, and written nowhere else: the
	# reader takes a fresh .sweep as proof that every agent pane of that pass was
	# stamped, so a lagging per-pane stamp means "no agent here" with no grace
	# window to wait out after a resume.
	((stamp)) && printf '%s\n' "$CLAUDE_NOW" >"$CLAUDE_LIVE_DIR/.sweep"

	# #692: on the client-independent sweep, make @window_has_agent track live
	# occupancy and reset naming display on any window that has no live agent but
	# still carries naming state. Entering only on a 1->0 option transition would
	# miss the reported host outright: @window_has_agent is never written there,
	# so it is never 1 and the stale @window_ai_name/@window_task would never
	# clear. A manually-named window's naming is not stale
	# and is left alone, matching claude_clear_window_display. The per-tick caller
	# ($1 empty) does the same with claude_clear_window_naming, which also deletes
	# names/tasks/issues; here only the option half is safe (CLAUDE_STATUS_DIR is
	# a bare /tmp path shared by every tmux server on the machine), so the
	# deletion is owed via the @window_naming_dirty mark — stamped BEFORE the
	# clear so a crash between the two writes still leaves the deletion owed.
	# Mirrors are daemon-owned and skipped, matching the per-tick loop.
	if [[ -n ${1:-} ]]; then
		local -A sess_reflow=()
		local wid stale
		for wid in "${!win_cur[@]}"; do
			[[ ${win_bridge[$wid]:-} == 1 ]] && continue
			if [[ -n ${win_has[$wid]:-} ]]; then
				[[ ${win_cur[$wid]:-} == 1 ]] && continue
				tmux set -qw -t "$wid" @window_has_agent 1
			else
				stale=""
				[[ -n ${win_cur[$wid]:-} ]] && stale=1
				if [[ ${win_manual[$wid]:-} != 1 && (-n ${win_ai[$wid]:-} || -n ${win_task[$wid]:-}) ]]; then
					stale=1
				fi
				[[ -n $stale ]] || continue
				tmux set -qw -t "$wid" @window_naming_dirty 1
				claude_clear_window_display "$wid" "${win_manual[$wid]:-}"
			fi
			sess_reflow[${win_sess[$wid]:-}]=1
		done
		# A session name (not a window id) so reflow's own scratch-* skip still
		# applies. Load-bearing: a control-mode client reports a real
		# #{client_width}, so reflow runs on a bridge-only host and is what
		# recomposes @window_label_rest_long from the cleared options.
		for s in "${!sess_reflow[@]}"; do
			[[ -n $s && $REFLOW_BIN != @* ]] || continue
			"$REFLOW_BIN" "$s" --force >/dev/null 2>&1 &
			disown 2>/dev/null || true
		done
	fi
	return 0
}

main() {
	# Driven by the @og-sweep-tick monitor hook, so a control-only host still
	# arms. Dispatched on an environment variable rather than argv because
	# #{qs:session_name} quotes a session name without changing its VALUE: a
	# session literally named "--sweep" would make $1 that exact string and route
	# itself into this branch forever, never rendering its own icons again. A
	# session name cannot forge OG_TICK_SWEEP.
	#
	# Arming only -- no prune here, and arm_agent_detect skips its reap for this
	# caller; see the reap's own comment for why a client-independent timer must
	# not delete from CLAUDE_STATUS_DIR.
	if [[ -n ${OG_TICK_SWEEP:-} ]]; then
		arm_agent_detect force
		return 0
	fi

	SESSION=${1:-$(tmux display-message -p '#{session_name}')}
	# $2 is #{@resume_claude}, expanded by the status format — avoids a show-option
	# fork per tick. "on" enables stamping each Claude pane's @remux_relaunch override
	# so tmux-remux resumes the session (not a bare shell) on restore.
	RESUME_CLAUDE=${2:-}
	# $3 is #{start_time}, expanded by the status format like $2 — avoids a
	# display-message fork per tick; direct invocations (hooks) fall back to one.
	SERVER_START=${3:-$(tmux display-message -p '#{start_time}')}
	# $4 is #{@resume_carousel}, expanded by the status format like $2 — avoids a
	# show-option fork per tick. "on" enables stamping any carousel viewer pane's
	# @remux_relaunch override so tmux-remux resumes it via tmux-carousel-restore
	# (not a bare shell) on restore. ${4:-} so the run-shell hook invocation
	# (config/tmux.conf.nix), which passes only $1, means "off".
	RESUME_CAROUSEL=${4:-}
	# $5 is #{@catppuccin_flavor}, expanded by the status format like $2 —
	# avoids a show-option fork per tick; direct invocations (hooks) fall back
	# to one via setup_claude_colors's own $TMUX-gated fork.
	CATPPUCCIN_FLAVOR=${5:-}
	# $6 is #{pid} (this server's own), expanded by the status format like $2 —
	# avoids a display-message fork per tick; direct invocations (hooks, the
	# config-load call site) fall back to one, same shape as SERVER_START/$3.
	SERVER_PID=${6:-$(tmux display-message -p '#{pid}')}
	MAX_ICONS=@MAX_ICONS@

	setup_claude_colors "$CATPPUCCIN_FLAVOR"

	# Purge pane-keyed status left by a previous tmux server before deriving any
	# label, so a restored pane that reused a dead pane's id doesn't inherit its
	# name/task. No-op after the first tick of each server (marker-gated).
	# SERVER_PID lets the sweep protect a live different server's state via the
	# panes/<id> server= ownership stamp instead of mtime alone.
	claude_prune_stale_state "$SERVER_START" "$SERVER_PID"

	# session_id is $N and cannot contain '|'; session_name can. Key every
	# window map by id:index, and parse names as the remainder after the first
	# '|' so a pipe in the name cannot shift the id. @reflow@ still wants a
	# name; window/session -t uses the id (numeric names are then unambiguous).
	declare -A sess_name sess_id_of
	while IFS= read -r line; do
		[[ -n $line ]] || continue
		sid="${line%%|*}"
		sname="${line#*|}"
		[[ -n $sid ]] || continue
		sess_name[$sid]="$sname"
		sess_id_of[$sname]="$sid"
	done < <(tmux list-sessions -F '#{session_id}|#{session_name}')
	INVOKE_SID="${sess_id_of[$SESSION]:-}"

	# --- Single batched list-panes call: all data in one tmux IPC roundtrip ---
	# list-panes -a, not -s: this script is invoked from status-format[0], which
	# tmux only evaluates for a client drawing a status line. Sessions with no
	# attached client never get their own tick, so one attached pass has to stamp
	# every window (#580). Window arrays are keyed session_id:index so indices
	# that collide across sessions don't merge, and a '|' in a session name
	# cannot shift later fields (the #580 discriminator: @window_icon_padded
	# never set).
	declare -A pane_to_win win_procs win_pane_path win_cur_branch win_active_pane win_cur_task win_cur_name pane_cur_relaunch pane_img_src pane_idx
	declare -A win_cur_display win_cur_padded win_cur_ago win_cur_rename win_cur_crew win_cur_crew_seen win_cur_bridge
	declare -A win_panes win_cur_has_agent win_cur_manual win_cur_naming_dirty
	declare -A all_sess sess_cur_active_icon sess_cur_session_fg sess_active_proc sess_active_win
	# cwd-move re-stamp (#596): win_cwd/win_cwd_pane/win_worktree/win_cwd_seen are
	# captured on the window's first NON-floating pane — a separate authority from
	# win_pane_path's plain first-pane-wins, since a floating scratch pane commonly
	# sits in a different directory. win_poison fails a window closed when a '|' in
	# a path has shifted its fields (see the read loop below).
	declare -A win_cwd win_cwd_pane win_worktree win_cwd_seen win_poison
	# '|' delimiter, not tab: tab is IFS-whitespace, so an empty middle field (a
	# window with no @branch yet) collapses and shifts every later field left,
	# corrupting cur_branch/active flags. session_id ($N) is the session field —
	# never session_name, which may itself contain '|'. '@window_task' is
	# free-form so it stays last — read drops any stray '|' it contains into that
	# final field.
	# @window_ai_name is sanitized free of '|' (claude-status-update), so it is safe
	# as a fixed middle field. @remux_relaunch is "claude --resume <uuid>" — no '|'
	# either, so it also stays a fixed middle field before the free-form task.
	# The icon/ago/rename/session fields are our own writes read back for
	# change-gating: glyphs, #[fg=…] codes, spaces, and hex colors — never '|'.
	# @crew_name (harness-stamped codename) and @crew_seen (our shadow of it) are
	# kebab tokens, so they sit safely before the free-form task; @bridge_win is
	# "1" or empty and @bridge_proc is a command name, so both do too.
	# @claude_img_src is aeye's own pane option (a "<server pid>-<pane>" key, or
	# empty) — no '|', so it too stays a fixed middle field before the task.
	# @window_has_agent, @window_manual_name and @window_naming_dirty (#671/#692)
	# are all closed "1"/"" tokens, same shape as @crew_name, so all three sit
	# safely before the task too. @window_naming_dirty is the client-independent
	# sweep's mark that a window's shared-dir naming files are still owed a
	# deletion (see claude_clear_window_naming).
	# pane_floating_flag and pane_active are both closed sets ("0"/"1"), so —
	# like session_id — they're safe as fixed middle fields no matter what's in
	# neighboring paths; win_poison below fails a window closed when either reads
	# outside that set, since a shifted row can never be told apart from a
	# genuine one. @worktree and @window_cwd_seen are paths and carry the same
	# '|' exposure pane_current_path already does, so both sit ahead of
	# pane_active/window_active — a '|' in either then cannot shift the flags the
	# rest of this script trusts as fixed-format.
	while IFS='|' read -r pane_id sess idx pidx pane_path proc cur_branch pane_floating cur_worktree cur_cwd_seen pane_active window_active cur_ai_name cur_relaunch cur_display cur_padded cur_ago cur_rename opt_active_icon opt_session_fg cur_crew cur_crew_seen cur_bridge bridge_proc cur_img_src cur_has_agent cur_manual cur_naming_dirty cur_task; do
		[[ -n $pane_id ]] || continue
		# A mirror pane runs the bridge renderer; @bridge_proc carries what the
		# remote pane is actually running, which is what the icons should show.
		[[ -n $bridge_proc ]] && proc="$bridge_proc"
		wkey="$sess:$idx"
		# A '|' in a path shifts every field after it in THIS row; pane_active is
		# the fixed-format canary. Fail closed like tmux-worktree-match's NF check:
		# poison the whole window rather than risk a compare that can never
		# settle, which would be a reconcile fork every tick forever.
		[[ $pane_active == 0 || $pane_active == 1 ]] || win_poison[$wkey]=1
		pane_to_win["${pane_id#%}"]="$wkey"
		# Space-joined bare pane ids for claude_clear_window_naming (#671) — same
		# shape as win_procs below, accumulated ahead of that block's `continue` so
		# a remain-on-exit corpse (empty pane_current_command) still lands here.
		win_panes[$wkey]="${win_panes[$wkey]:+${win_panes[$wkey]} }${pane_id#%}"
		pane_cur_relaunch["${pane_id#%}"]="$cur_relaunch"
		pane_img_src["${pane_id#%}"]="$cur_img_src"
		pane_idx["${pane_id#%}"]="$pidx"
		all_sess[$sess]=1
		# Session options (same on every row of a session) must be copied here:
		# the EOF read that ends the loop blanks the read variables themselves.
		sess_cur_active_icon[$sess]="$opt_active_icon"
		sess_cur_session_fg[$sess]="$opt_session_fg"
		# First pane per window wins for path/branch/task — panes in a window share a
		# cwd, and @window_task/@branch are window options (same for every pane).
		if [[ -z ${win_pane_path[$wkey]+x} ]]; then
			win_pane_path[$wkey]="$pane_path"
			win_cur_branch[$wkey]="$cur_branch"
			win_cur_task[$wkey]="$cur_task"
			win_cur_name[$wkey]="$cur_ai_name"
			win_cur_display[$wkey]="$cur_display"
			win_cur_padded[$wkey]="$cur_padded"
			win_cur_ago[$wkey]="$cur_ago"
			win_cur_rename[$wkey]="$cur_rename"
			win_cur_crew[$wkey]="$cur_crew"
			win_cur_crew_seen[$wkey]="$cur_crew_seen"
			win_cur_bridge[$wkey]="$cur_bridge"
			win_cur_has_agent[$wkey]="$cur_has_agent"
			win_cur_manual[$wkey]="$cur_manual"
			win_cur_naming_dirty[$wkey]="$cur_naming_dirty"
		fi
		# cwd authority (#596): the FIRST NON-FLOATING pane, not the first pane full
		# stop and not the active pane — a floating scratch pane commonly sits in a
		# different directory, and keying on the active pane would re-tag the
		# window on every `prefix + o` between two repos, which is
		# user-gesture-driven git/gh/reflow load for a keystroke that changed
		# nothing.
		if [[ -z ${win_cwd[$wkey]+x} && $pane_floating != 1 ]]; then
			win_cwd[$wkey]="$pane_path"
			win_cwd_pane[$wkey]="$pane_id"
			win_worktree[$wkey]="$cur_worktree"
			win_cwd_seen[$wkey]="$cur_cwd_seen"
		fi
		# The task file is keyed by the pane Claude runs in, so resolve the genuinely
		# active pane (list-panes orders by index, not active-first).
		[[ $pane_active == 1 ]] && win_active_pane[$wkey]="${pane_id#%}"
		[[ $window_active == 1 ]] && sess_active_win[$sess]="$idx"
		# Track each session's active pane command (active pane in that session's
		# active window) — @active_pane_icon is session-scoped.
		[[ $pane_active == 1 && $window_active == 1 ]] && sess_active_proc[$sess]="$proc"
		# Collect unique processes per window
		[[ -z $proc ]] && continue
		existing="${win_procs[$wkey]:-}"
		case " $existing " in
		*" $proc "*) ;;
		*) win_procs[$wkey]="${existing:+$existing }$proc" ;;
		esac
	done < <(tmux list-panes -a -F '#{pane_id}|#{session_id}|#{window_index}|#{pane_index}|#{pane_current_path}|#{pane_current_command}|#{@branch}|#{pane_floating_flag}|#{@worktree}|#{@window_cwd_seen}|#{pane_active}|#{window_active}|#{@window_ai_name}|#{@remux_relaunch}|#{@window_icon_display}|#{@window_icon_padded}|#{@window_claude_ago}|#{automatic-rename}|#{@active_pane_icon}|#{@claude_session_fg}|#{@crew_name}|#{@crew_seen}|#{@bridge_win}|#{@bridge_proc}|#{@claude_img_src}|#{@window_has_agent}|#{@window_manual_name}|#{@window_naming_dirty}|#{@window_task}')

	arm_agent_detect

	# Carousel viewer restore: stamp @remux_relaunch on any pane running the aeye
	# carousel (non-empty @claude_img_src) so tmux-remux relaunches it via
	# tmux-carousel-restore (not a bare shell) on a future restore. A viewer pane
	# has no claude-status state file, so it never appears in claude_pane_ids()
	# below — hence a separate pass over the same batched read. Change-gated like
	# the Claude stamp, and -q is likewise omitted so a lost write is loud.
	# The @* test disables the pass on an unsubstituted placeholder (the
	# @assume_dead_after@ rule in lib-claude.sh); resumeCarouselEnable gates the
	# same case in nix, but this script also runs straight from tests and hooks.
	# The stamp carries the host's CURRENT pane index as an argument. Pane ids do
	# not survive a restore and tmux-remux carries no pane options, so this is
	# the only evidence of which sibling was the host that reaches the restored
	# pane; tmux-carousel-restore uses it to break ties between agent panes
	# rather than picking one arbitrarily. @claude_img_src is "<srv>-<pane num>",
	# so the host's id is already in hand and its index comes from pane_idx,
	# built in the batched read above — no extra fork on the 1s path.
	if [[ $RESUME_CAROUSEL == on && $CAROUSEL_RESTORE_BIN != @* ]]; then
		for pane_file in "${!pane_img_src[@]}"; do
			src="${pane_img_src[$pane_file]}"
			[[ -n $src ]] || continue
			desired="$CAROUSEL_RESTORE_BIN"
			host_idx="${pane_idx[${src##*-}]:-}"
			[[ $host_idx =~ ^[0-9]+$ ]] && desired="$CAROUSEL_RESTORE_BIN $host_idx"
			cur="${pane_cur_relaunch[$pane_file]:-}"
			[[ $desired != "$cur" ]] || continue
			tmux set -p -t "%$pane_file" @remux_relaunch "$desired"
		done
	fi

	# --- Claude status: read pane files, bucket by session:index ---
	declare -A win_claude_state win_claude_fade win_claude_unseen win_claude_ts
	# Per-window per-state counts + that state's freshest pane fade/unseen
	# (keys: "<wkey>,<state>"); the winning state is resolved after the loop.
	declare -A win_cnt win_state_fade win_state_unseen win_has_claude
	# Per-session tally drives that session's status-bar tint (@claude_session_fg)
	declare -A sess_w sess_k sess_p sess_d sess_i sess_e sess_dn sess_int sess_min_fade sess_unseen
	while IFS= read -r pane_file; do
		[[ -n $pane_file ]] || continue
		win_idx="${pane_to_win[$pane_file]:-}"
		[[ -n $win_idx ]] || continue
		s="${win_idx%:*}"
		read_pane_state "$CLAUDE_PANES_DIR/$pane_file" || continue
		state="$REPLY"
		fade=$REPLY_FADE
		unseen=$REPLY_UNSEEN

		# Stamp the pane's resume override so tmux-remux relaunches the actual
		# Claude session (not a bare shell) on restore. The transcript basename
		# is the session UUID. Set only on change — @remux_relaunch is read back via
		# the batched list-panes above, so a stable pane forks nothing per tick.
		if [[ $RESUME_CLAUDE == on ]]; then
			uuid="${REPLY_TRANSCRIPT##*/}"
			uuid="${uuid%.jsonl}"
			desired=""
			[[ -n $uuid ]] && desired="claude --resume $uuid"
			cur="${pane_cur_relaunch[$pane_file]:-}"
			# An empty desired means no real transcript (a screen-only agent-detect
			# ghost entry) — refuse to clobber a Codex/Cursor hook's own stamp with
			# nothing. A non-empty desired is positive evidence of a live Claude
			# session, so it may overwrite a foreign stamp (a pane that moved from
			# Codex/Cursor to Claude).
			# No -q, unlike the batched sets below: it made a lost write return 0
			# and print nothing, so the only trace was a downstream
			# "invalid option" (#373).
			if [[ -n $desired ]] && [[ $desired != "$cur" ]]; then
				tmux set -p -t "%$pane_file" @remux_relaunch "$desired"
			fi
		fi
		# Freshest pane timestamp per window drives the "last active" label
		[[ -n $REPLY_TS ]] && ((REPLY_TS > ${win_claude_ts[$win_idx]:-0})) &&
			win_claude_ts[$win_idx]=$REPLY_TS
		# Session aggregate: count states, freshest pane wins the fade
		case "$state" in
		error) ((sess_e[$s]++)) ;;
		waiting) ((sess_w[$s]++)) ;;
		compacting) ((sess_k[$s]++)) ;;
		interrupted) ((sess_int[$s]++)) ;;
		processing) ((sess_p[$s]++)) ;;
		done) ((sess_d[$s]++)) ;;
		idle) ((sess_i[$s]++)) ;;
		denied) ((sess_dn[$s]++)) ;;
		esac
		((fade < ${sess_min_fade[$s]:-100})) && sess_min_fade[$s]=$fade
		[[ $unseen == 1 ]] && sess_unseen[$s]=1
		# Per-window: tally the state and track the freshest fade / any-unseen for
		# it. The winning state (and its pane's fade) is picked after the loop.
		key="$win_idx,$state"
		win_cnt[$key]=$((${win_cnt[$key]:-0} + 1))
		if [[ -z ${win_state_fade[$key]:-} ]] || ((fade < win_state_fade[$key])); then
			win_state_fade[$key]=$fade
		fi
		[[ $unseen == 1 ]] && win_state_unseen[$key]=1
		win_has_claude[$win_idx]=1
	done < <(claude_pane_ids)

	# Resolve each window's icon state from its counts via the shared priority
	# order, then adopt the winning state's freshest pane fade/unseen. Using
	# claude_priority_state (not a hand-rolled merge) keeps the ordering — and
	# every state, incl. denied — identical to the session tint below. Counts are
	# gathered in claude_priority_state's positional order; keys route through the
	# scalar $key because a literal-comma subscript is unsafe (shfmt -s mangles it).
	prio_order=(waiting compacting processing "done" idle error denied interrupted)
	for win_idx in "${!win_has_claude[@]}"; do
		counts=()
		for st in "${prio_order[@]}"; do
			key="$win_idx,$st"
			counts+=("${win_cnt[$key]:-0}")
		done
		claude_priority_state "${counts[@]}"
		key="$win_idx,$REPLY"
		win_claude_state[$win_idx]="$REPLY"
		win_claude_fade[$win_idx]="${win_state_fade[$key]:-0}"
		win_claude_unseen[$win_idx]="${win_state_unseen[$key]:-0}"
	done

	# Session-name color: tint with that session's aggregate claude state, faded
	# by its freshest pane's age. Empty when no claude panes — the format falls
	# back to the theme's session color.
	declare -A sess_fg
	for s in "${!all_sess[@]}"; do
		claude_priority_state "${sess_w[$s]:-0}" "${sess_k[$s]:-0}" "${sess_p[$s]:-0}" "${sess_d[$s]:-0}" "${sess_i[$s]:-0}" "${sess_e[$s]:-0}" "${sess_dn[$s]:-0}" "${sess_int[$s]:-0}"
		claude_faded_hex "$REPLY" "${sess_min_fade[$s]:-100}" "${sess_unseen[$s]:-0}"
		sess_fg[$s]=$REPLY
	done

	# --- Compute process icons + claude per window, measure display widths ---
	declare -a all_idx=()
	declare -A win_icons win_icon_dw win_display
	declare -A sess_need_reflow

	# Collect all tmux set commands to batch via `tmux source -`
	tmux_cmds=""

	for wkey in "${!win_pane_path[@]}"; do
		all_idx+=("$wkey")
		s="${wkey%:*}"
		idx="${wkey##*:}"
		# win_cwd (first non-floating pane), not win_pane_path (first pane full
		# stop): the #137 branch poll below and the cwd-move gate must read the
		# same pane, or they stamp @branch/@git_root from different panes and
		# revert each other on a split window. Falls back to win_pane_path for a
		# window that is ALL float — killing the last tiled pane leaves one — since
		# the poll writes whatever git returns, and an empty path would blank
		# @branch/@git_root and then re-fork git every tick on the -z arm below.
		pane_path="${win_cwd[$wkey]:-${win_pane_path[$wkey]}}"
		target="$wkey"

		# Agent occupancy (#671/#692): @window_has_agent tracks whether any pane
		# in the window currently runs a manifest agent command, and gates the
		# task/name file reads just below — a lingering self-report file must not
		# re-stamp an option the client-independent sweep cleared. That hazard
		# exists only for a window the sweep owns: a manually-named or mirror
		# window's naming is never cleared by the sweep, so its kept file still
		# legitimately owns the option and is read as before. Computed here
		# (ahead of those reads) and reused by the transition block below.
		has_agent=""
		if [[ ${win_cur_bridge[$wkey]:-} != 1 ]]; then
			# shellcheck disable=SC2086  # win_procs is a space-joined string; word-split intentionally
			for p in ${win_procs[$wkey]:-}; do
				normalize_wrapped_cmd "$p"
				case " $AGENT_COMMANDS " in *" $REPLY "*)
					has_agent=1
					break
					;;
				esac
			done
		fi
		naming_read=1
		if [[ -z $has_agent && ${win_cur_manual[$wkey]:-} != 1 && ${win_cur_bridge[$wkey]:-} != 1 ]]; then
			naming_read=""
		fi

		# Task label tracks the active pane's self-reported "what Claude is doing"
		# phrase (UserPromptSubmit hook). It can change in any window, so poll every
		# window each tick — a single small file read. Set directly (not batched via
		# `source -`): the phrase is free-form and would break the command parser.
		task=""
		[[ -n $naming_read && -f "$CLAUDE_TASKS_DIR/${win_active_pane[$wkey]}" ]] &&
			IFS= read -r task <"$CLAUDE_TASKS_DIR/${win_active_pane[$wkey]}"
		if [[ $task != "${win_cur_task[$wkey]:-}" ]]; then
			tmux set -qw -t "$target" @window_task "$task"
			sess_need_reflow[$s]=1
		fi

		# AI name: the active pane's Claude-set window title (claude-status-update
		# name set, prompted by the UserPromptSubmit nudge on fallback windows).
		# build_window_label prefers it over the raw task. Mirror like the task —
		# free-form, set directly, only on change so reflow isn't kicked every tick.
		ai_name=""
		[[ -n $naming_read && -f "$CLAUDE_NAMES_DIR/${win_active_pane[$wkey]}" ]] &&
			IFS= read -r ai_name <"$CLAUDE_NAMES_DIR/${win_active_pane[$wkey]}"
		if [[ $ai_name != "${win_cur_name[$wkey]:-}" ]]; then
			tmux set -qw -t "$target" @window_ai_name "$ai_name"
			sess_need_reflow[$s]=1
		fi

		# Crew badge: the fan-out harness stamps @crew_name directly, and no tmux
		# hook fires on a user-option set — so poll for a change here and kick the
		# forced reflow, like task/branch. The multi-line grid's badge column is
		# reflow-computed (@window_crew_disp + crew_colw), so a name change must
		# recompute; @crew_color is read live by the format and needs no reflow.
		# @crew_seen is our own shadow of the last name we acted on.
		if [[ ${win_cur_crew[$wkey]:-} != "${win_cur_crew_seen[$wkey]:-}" ]]; then
			tmux set -qw -t "$target" @crew_seen "${win_cur_crew[$wkey]:-}"
			sess_need_reflow[$s]=1
		fi

		# Agent occupancy (#671/#692) transition: @window_has_agent tracks whether
		# any pane in the window currently runs a manifest agent command. On a
		# genuine transition, clear naming/crew display state via
		# claude_clear_window_naming (never @crew_name/@crew_color themselves —
		# dispatcher-owned, CLAUDE.md hard constraint). clear_needed adds the
		# @window_naming_dirty mark the client-independent sweep leaves when it
		# cleared the options but could not delete the shared-dir files; it is
		# gated on has_agent being empty so a window that gained a new agent while
		# the mark was outstanding does not re-fire every tick. Mirror windows are
		# excluded outright (daemon-owned).
		clear_needed=""
		if [[ -z $has_agent && -n ${win_cur_naming_dirty[$wkey]:-} ]]; then
			clear_needed=1
		fi
		if [[ -z ${win_poison[$wkey]:-} && ${win_cur_bridge[$wkey]:-} != 1 && ($has_agent != "${win_cur_has_agent[$wkey]:-}" || -n $clear_needed) ]]; then
			if [[ -n $has_agent ]]; then
				tmux set -qw -t "$target" @window_has_agent 1
			else
				# Target by pane id, not "$sess:$idx" — renumber-windows can slide
				# an index onto a different window between this batched read and
				# this write, and unlike the @window_has_agent set above, this call
				# is destructive (deletes names/tasks/issues files), so it must not
				# risk landing on the wrong window.
				# shellcheck disable=SC2086  # win_panes is a space-joined string of bare pane ids; word-split intentionally into positional args
				claude_clear_window_naming "%${win_panes[$wkey]%% *}" "${win_cur_manual[$wkey]:-}" ${win_panes[$wkey]:-}
			fi
			sess_need_reflow[$s]=1
		fi

		# cwd-move re-stamp (#596): a window whose first non-floating pane cd'd into
		# a different worktree keeps the old repo's @worktree/@branch/@git_root
		# forever — no tmux hook fires on a plain `cd`. Ride this batched read and
		# delegate the actual re-tagging to the reconciler, which already knows how
		# to derive @worktree/@branch/@git_root/@issue_* from a target pane and
		# bails on a bridge mirror itself — this gate only decides WHEN to fire it.
		# No sess_need_reflow here: the reconciler forces its own reflow, and one
		# fired from here would race ahead of the stamps it is meant to render.
		if [[ $RECONCILE_BIN != @* && -z ${win_poison[$wkey]:-} && ${win_cur_bridge[$wkey]:-} != 1 && -n ${win_cwd[$wkey]:-} ]]; then
			cwd="${win_cwd[$wkey]}"
			# Free path: a cwd already inside its stamped worktree (same repo, or a
			# `cd` deeper into it) costs nothing and writes nothing — the common
			# case on every tick, and what keeps a 40-window tmux-remux restore from
			# firing 40 reconciles at once.
			if ! under "$cwd" "${win_worktree[$wkey]:-}"; then
				# Bounds what the free path above cannot settle: after a move into a
				# non-git directory the reconciler exits without writing, so
				# @worktree still names the old repo and the check above stays true
				# forever. The memo is what stops that being one fork per tick.
				if [[ $cwd != "${win_cwd_seen[$wkey]:-}" ]]; then
					# Direct argv, not the tmux_cmds/`tmux source -` batch below: a
					# directory name can legally contain a single quote, which a
					# batched single-quoted token has no escape for — same hazard
					# the @branch write below documents. No -q either, same reason:
					# a lost write should surface as a loud "invalid option", not
					# silently return 0 (#373).
					# Both calls target the PANE, never "$target" ($sess:$idx):
					# renumber-window is on, so an index captured during the read
					# above can slide onto a neighbour before either lands.
					tmux set-option -t "${win_cwd_pane[$wkey]}" -w @window_cwd_seen "$cwd"
					"$RECONCILE_BIN" "${win_cwd_pane[$wkey]}" --cwd-move >/dev/null 2>&1 &
					disown 2>/dev/null || true
				fi
			fi
		fi

		# Branch detection forks git per window. A branch only changes in the window
		# where a checkout/cd happens, so poll only the invoking session's active
		# window each tick; other sessions' active windows and every inactive window
		# trust their cached @branch (worktrunk stamps it on switch). Invoking
		# session is matched by id ($N), looked up from $1 / $SESSION's name via
		# the list-sessions map — a '|' in the name must not be compared as a
		# middle format field.
		# A window with no @branch yet (manual new-window, restore, never-attached
		# session) is polled once to seed it, then trusted — this caps the steady
		# git fork rate at ~1/tick plus unseeded windows.
		if [[ ($s == "$INVOKE_SID" && $idx == "${sess_active_win[$INVOKE_SID]:-}") || -z ${win_cur_branch[$wkey]:-} ]]; then
			# timeout so a stuck git (NFS stall, held index.lock) can't wedge the
			# whole icon updater — it degrades to the cached branch for that tick.
			branch=$(timeout 2 git -C "$pane_path" branch --show-current 2>/dev/null) || branch=""
			if [[ $branch != "${win_cur_branch[$wkey]:-}" ]]; then
				# Direct argv (not the tmux_cmds/`tmux source -` batch below): a git
				# branch name can legally contain a single quote, which a batched
				# single-quoted token has no escape for — `tmux source -` would
				# reparse it and let an adversarial branch name inject arbitrary
				# tmux commands. Only forks on a genuine transition (rare), so this
				# doesn't touch the steady-state hot path.
				tmux set-option -t "$target" -w @branch "$branch"
				# Re-derive git root when branch changes (different repo or worktree)
				git_root=$(timeout 2 git -C "$pane_path" rev-parse --show-toplevel 2>/dev/null) || git_root=""
				tmux set-option -t "$target" -w @git_root "$git_root"
				sess_need_reflow[$s]=1
				# Auto re-stamp (#137): a genuine transition (previous branch non-empty,
				# so this isn't the initial seed already covered by post-switch/
				# reconcile-window) means a `git checkout -b` happened in-place — the
				# new branch's issue/PR never got a chance to stamp. Re-fire so
				# @issue_* catches up; serialized through tmux-issue-stamp's own
				# per-window lock, so this never races post-switch or `enrich`.
				if [[ -n $ISSUE_STAMP_BIN && $ISSUE_STAMP_BIN != @* && -n ${win_cur_branch[$wkey]:-} && -n $branch ]]; then
					"$ISSUE_STAMP_BIN" "$target" "$git_root" "$branch" >/dev/null 2>&1 &
					disown 2>/dev/null || true
				fi
			fi
		fi

		# Build process icons from batched data
		build_proc_icons "${win_procs[$wkey]:-}" "$MAX_ICONS"
		proc_icon_str="${REPLY% }"
		icon="$REPLY"
		# shellcheck disable=SC2153 # REPLY_DW set by build_proc_icons (sourced lib)
		icon_dw=$REPLY_DW

		# Append colored claude status icon (shares the icon column)
		c_state="${win_claude_state[$wkey]:-}"
		display="${proc_icon_str}"
		claude_colored_icon "$c_state" "${win_claude_fade[$wkey]:-0}" "${win_claude_unseen[$wkey]:-0}"
		if [[ -n $REPLY ]]; then
			icon+="$REPLY"
			((icon_dw += 2)) # 1-cell nerd font icon + 1 space
			# Add to display with space separator if process icons exist
			[[ -n $display ]] && display+=" "
			display+="${REPLY% }" # strip trailing space for display
		fi

		win_icons[$wkey]="$icon"
		win_icon_dw[$wkey]=$icon_dw
		win_display[$wkey]="$display"

		# "Last active" time: shown only for halted states (the live icon already
		# conveys active ones). A bare unit like "5m" is parser-safe, so batch it.
		# Gated on change (read back for free via the batched list-panes) — it only
		# ticks over once a minute.
		ago=""
		case "$c_state" in
		idle | done | interrupted | error)
			ts="${win_claude_ts[$wkey]:-0}"
			if ((ts > 0 && CLAUDE_NOW > ts)); then
				claude_ago "$((CLAUDE_NOW - ts))"
				ago="$REPLY"
			fi
			;;
		esac
		if [[ $ago != "${win_cur_ago[$wkey]:-}" ]]; then
			tmux_cmds+="set -qw -t '$target' @window_claude_ago '$ago'"$'\n'
		fi
	done

	# Set per-session active pane icon and claude tint (from batched data)
	for s in "${!all_sess[@]}"; do
		active_icon=""
		proc="${sess_active_proc[$s]:-}"
		normalize_wrapped_cmd "$proc"
		proc="$REPLY"
		[[ -n $proc ]] && active_icon="${ICON_MAP[$proc]:-}"
		if [[ $active_icon != "${sess_cur_active_icon[$s]:-}" ]]; then
			tmux_cmds+="set -q -t '$s' @active_pane_icon '$active_icon'"$'\n'
		fi
		if [[ ${sess_fg[$s]:-} != "${sess_cur_session_fg[$s]:-}" ]]; then
			tmux_cmds+="set -q -t '$s' @claude_session_fg '${sess_fg[$s]}'"$'\n'
		fi
	done

	# --- Second pass: set unpadded + padded icon variables ---
	# Fixed column: worst case MAX_ICONS emoji (3 cells each) + 1 nerd font claude (2 cells)
	TARGET_DW=$((MAX_ICONS * 3 + 2))
	for wkey in "${all_idx[@]}"; do
		target="$wkey"

		# Unpadded (for window names — process icons + colored claude)
		if [[ ${win_display[$wkey]} != "${win_cur_display[$wkey]:-}" ]]; then
			tmux_cmds+="set -qw -t '$target' @window_icon_display '${win_display[$wkey]}'"$'\n'
		fi

		# Re-assert automatic-rename: window names are derived (label + icon via
		# automatic-rename-format) and allow-rename is off, so it must stay on.
		# tmux-remux restore creates windows with `new-window -n`, which flips it
		# off and freezes the name on a stale label; this self-heals it. Gated on
		# the effective value (boolean options expand to 0/1 in formats).
		#
		# Except on a mirror window (#167 @bridge_win opt-out), where the daemon
		# owns the name: turning automatic-rename back on undoes its
		# rename-window, and tmux only re-derives a name when the active pane
		# produces output — an idle renderer never does, so the name freezes on
		# whatever the format yielded at that instant (the launcher's cwd).
		#
		# Also except a window carrying @window_manual_name (#671): this same
		# reassert doesn't distinguish a genuine user `prefix + ,` rename from
		# tmux-remux's restore-induced automatic-rename off, so without this a
		# manual rename reverted within ~1s regardless of who set it.
		if [[ ${win_cur_bridge[$wkey]:-} == 1 ]]; then
			if [[ ${win_cur_rename[$wkey]:-} == 1 ]]; then
				tmux_cmds+="set -qw -t '$target' automatic-rename off"$'\n'
			fi
		elif [[ ${win_cur_rename[$wkey]:-} != 1 && ${win_cur_manual[$wkey]:-} != 1 ]]; then
			tmux_cmds+="set -qw -t '$target' automatic-rename on"$'\n'
		fi

		# Padded (for status bar — process icons + claude, fixed width)
		pad_to_width "${win_icons[$wkey]}" "${win_icon_dw[$wkey]}" "$TARGET_DW"
		if [[ $REPLY != "${win_cur_padded[$wkey]:-}" ]]; then
			tmux_cmds+="set -qw -t '$target' @window_icon_padded '$REPLY'"$'\n'
		fi
	done

	# Batch the surviving set commands in one IPC call; a steady-state tick
	# (no spinner, no minute rollover) emits nothing, so skip the fork.
	if [[ -n $tmux_cmds ]]; then
		printf '%s' "$tmux_cmds" | tmux source -
	fi

	# A branch or task change means window labels (built by reflow from
	# @branch/@issue_*/@window_task) are stale — no tmux hook fires on cd or a new
	# prompt, so kick a forced reflow here. Per session whose labels actually
	# changed, not only the invoking session. Reflow exits when #{client_width}
	# is empty (no attached client), so an unattached session's first @branch
	# seed still stamps the window option; the grid pass waits until a client
	# exists. Icon stamps from this script do not depend on that.
	# The call below is the reflow store path (not a bare name) so a config
	# reload repoints it without a tmux server restart. Reflow takes a session
	# name; we map from the id we keyed on.
	for s in "${!sess_need_reflow[@]}"; do
		"$REFLOW_BIN" "${sess_name[$s]}" --force >/dev/null 2>&1 &
		# Instant reflow can finish before disown; a reaped job makes disown
		# return 1, which is then the process exit (main's last command).
		disown 2>/dev/null || true
	done
}

[[ ${BASH_SOURCE[0]} == "$0" ]] && main "$@"
