#!/usr/bin/env bash
# Shared Claude status utilities for tmux scripts.
# Sourced (not executed) — provides constants and functions.

# shellcheck disable=SC2034  # used by scripts that source this library
CLAUDE_STATUS_DIR="${CLAUDE_STATUS_DIR:-/tmp/claude-status}"
CLAUDE_PANES_DIR="$CLAUDE_STATUS_DIR/panes"
CLAUDE_SCREEN_DIR="$CLAUDE_STATUS_DIR/screen"
CLAUDE_ISSUES_DIR="$CLAUDE_STATUS_DIR/issues"
CLAUDE_TASKS_DIR="$CLAUDE_STATUS_DIR/tasks"
CLAUDE_NAMES_DIR="$CLAUDE_STATUS_DIR/names"
CLAUDE_INTERRUPT_DIR="$CLAUDE_STATUS_DIR/interrupt"
CLAUDE_WATCHERS_DIR="$CLAUDE_STATUS_DIR/watchers"
CLAUDE_LIVE_DIR="$CLAUDE_STATUS_DIR/live"
CLAUDE_SPINNER_FRAMES=("󰪞" "󰪟" "󰪠" "󰪡" "󰪢" "󰪣" "󰪤" "󰪥")
CLAUDE_ICON_WAITING="󰔟"
CLAUDE_ICON_COMPACTING="󰡍"
CLAUDE_ICON_DONE="󰸞"
CLAUDE_ICON_IDLE="󰒲"
CLAUDE_ICON_ERROR="󰅚"       # nerd: nf-md-close_circle_outline
CLAUDE_ICON_DENIED="󰔟"      # same clock as waiting, different color
CLAUDE_ICON_INTERRUPTED="󰜺" # nerd: nf-md-cancel — user-interrupted (Esc) turn
CLAUDE_ICON_BG="󰅐"          # nerd: nf-md-timer_sand — background shells still running

# Timestamp cache (set once per script invocation). The env override is a test
# seam, same shape as CLAUDE_ASSUME_DEAD_AFTER below: tmux-update-icons throttles
# its presence sweep on CLAUDE_NOW % 5, so without this a suite cannot pin the
# second and the sweep is a 1-in-5 coin flip (#373). Kept fork-free — this runs
# on the per-second status path.
#
# Digits only: a non-numeric value inherited from a shell would freeze the clock
# for every status script, and freeze the % 5 throttle with it — so the sweep
# would either never run again or run every tick, silently.
[[ ${CLAUDE_NOW:-} =~ ^[0-9]+$ ]] || printf -v CLAUDE_NOW '%(%s)T' -1

# Staleness fade — the color stays the state's bright hue until its threshold,
# then eases toward dim grey over CLAUDE_FADE_DURATION seconds (not a hard snap).
CLAUDE_STALE_WAITING=30
CLAUDE_STALE_COMPACTING=60
CLAUDE_STALE_PROCESSING=300
CLAUDE_STALE_DONE=60
CLAUDE_STALE_ERROR=120
CLAUDE_STALE_DENIED=60
CLAUDE_FADE_DURATION=45

# Interrupt detection. No Claude Code hook fires on an Esc-interrupt, so a turn
# abandoned mid-flight just leaves the pane stuck at `processing` forever. The
# only durable trace is a marker line the interrupt writes into the transcript.
# read_pane_state reclassifies such a pane to `interrupted` — but only once it
# has been quiet past CLAUDE_INTERRUPT_CHECK_AGE, so normal tool churn (which
# refreshes the timestamp every PreToolUse/PostToolUse) never reads a transcript.
# A genuinely long-running tool keeps its `tool_use` block at the tail, not the
# marker, so it is not a false positive — it just costs one tail per read while
# it runs.
#
# The verdict is cached per pane at interrupt/<id> and reused while the stamp
# is newer than the transcript. Without it, a pane that stays `interrupted`
# (a derived state — panes/<id> still says `processing`) would fork tail every
# tick of every poller until its next prompt.
CLAUDE_INTERRUPT_CHECK_AGE=8
CLAUDE_INTERRUPT_MARKER="Request interrupted by user"

# Dead-agent floor. An agent that exits back to a shell leaves its last state
# behind: claude_prune_stale_state reaps only a previous tmux server's files
# and claude_reap_dead_panes reaps only a pane list-panes -a no longer
# reports, so a pane running a plain shell keeps reporting an agent state and
# merely fades. The presence sweep in tmux-update-icons stamps live/<id> for
# every pane whose foreground command is an agent, and live/.sweep once that
# pass completes; read_pane_state withdraws a state that has gone stale on a
# pane the sweep stopped stamping.
#
# Positive evidence only — a *missing* live/<id> never vetoes. Absence is
# ambiguous (pane never swept, feature just switched on, sweep never ran), while
# a stamp that has stopped advancing is a live sweep reporting "no agent here".
# An agent that shells out (Claude does, constantly) only skips a stamp or two,
# and the veto also needs the state itself stale past its CLAUDE_STALE_* mark.
#
# 0 disables both halves; so does an unsubstituted placeholder. The env override
# is the test seam, same shape as AGENT_DETECT_BIN in tmux-update-icons.
CLAUDE_ASSUME_DEAD_AFTER="${CLAUDE_ASSUME_DEAD_AFTER:-@assume_dead_after@}"
[[ $CLAUDE_ASSUME_DEAD_AFTER =~ ^[0-9]+$ ]] || CLAUDE_ASSUME_DEAD_AFTER=0
# How long .sweep stays fresh, and the floor under the threshold above: three
# missed sweeps of the 5s cadence. A stale .sweep means nobody is publishing
# presence (client detached, feature off), which deactivates the veto entirely.
CLAUDE_LIVE_SWEEP_FRESH=15

# claude_pid_is_tmux PID
# Succeeds when PID looks like a live tmux server. `kill -0` alone is satisfied
# by any process that reused a dead server's pid, so the process name is
# checked too (`ps -o comm= -p`, portable to macOS; the nix wrapper may show as
# .tmux-wrapped). Fails safe toward "yes": no ps on PATH, or an empty answer,
# cannot tell, so the caller protects.
claude_pid_is_tmux() {
	kill -0 "$1" 2>/dev/null || return 1
	command -v ps &>/dev/null || return 0
	local comm
	comm=$(ps -o comm= -p "$1" 2>/dev/null)
	[[ -z $comm ]] && return 0
	[[ $comm == *tmux* ]]
}

# claude_prune_stale_state SERVER_START [SERVER_PID]
# Drops pane-id-keyed status files left behind by a previous tmux server. tmux
# restarts pane ids at %0 on server (re)start, so a restored pane can reuse a
# dead pane's id and inherit its cached name/task/issue — surfacing an unrelated
# window's label. mtime vs this server's start_time is the base signal:
# anything written before this server booted is stale. A marker file holding
# the current start_time gates the directory scan to once per server, so the
# per-tick status poller that calls this stays cheap.
#
# mtime alone cannot tell "written by a server that has since died" from
# "written moments ago by a different, still-running server" — under the
# shared CLAUDE_STATUS_DIR, a second server's very first boot could otherwise
# delete a live different server's state. When SERVER_PID (this booting
# server's own #{pid}) is passed, panes/, screen/ and watchers/ files are
# scanned once up front for their server= field (stamped by the writers of
# panes/<id>: scripts/claude-status-update.sh and
# picker/remotebridge/daemon/agentstatus.go; of screen/<id>: agent-detect's
# statefile.Writer and that same shipper; of watchers/<id>: agent-detect's
# registerWatcher); an id whose recorded owner PID is numeric, not SERVER_PID,
# and still a live tmux process (claude_pid_is_tmux, evaluated once per
# distinct owner per pass) is protected from deletion across every dir this
# sweeps, regardless of mtime.
# Caveats, deliberately:
#   (a) the liveness check is `kill -0` plus a `ps` comm match for "tmux", so a
#       reused PID belonging to an unrelated process no longer protects a dead
#       generation's ids. Where ps is missing or answers nothing it cannot
#       tell and protects — under-reap, never over-delete, is the posture.
#   (b) screen-only agent panes (pi/codex/cursor, no panes/<id> sibling) are
#       protected through the server= stamps on screen/<id> and watchers/<id>.
#       Files written before those stamps existed carry none and still fall to
#       mtime alone. Separately, claude_reap_dead_panes (the ~60s sweep) deletes
#       screen/<id> and watchers/<id> for ids absent from this server's pane
#       list with no ownership guard, so cross-server flapping is not fully
#       closed — a known follow-up.
#   (c) with SERVER_PID the marker gate is per-server
#       (.server_start.<pid>, content = start_time), so two live servers each
#       sweep once per boot instead of ping-ponging one shared marker. The
#       shared .server_start is still written after every sweep, and is the
#       only gate when SERVER_PID is empty. A sweep also removes
#       .server_start.<pid> markers whose pid is no longer alive.
# GNU stat pinned by Nix, same rationale as lib-log.sh's OG_STAT.
OG_STAT="@stat@"
if [[ $OG_STAT == @* ]]; then
	OG_STAT=stat
fi
claude_prune_stale_state() {
	local server_start=$1 server_pid=${2:-}
	[[ -z $server_start ]] && return 0
	local marker="$CLAUDE_STATUS_DIR/.server_start"
	local gate="$marker"
	[[ -n $server_pid ]] && gate="$marker.$server_pid"
	[[ -r $gate && $(<"$gate") == "$server_start" ]] && return 0

	local -A protected=() owner_live=()
	if [[ -n $server_pid ]]; then
		local odir pf id owner key val
		for odir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_WATCHERS_DIR"; do
			for pf in "$odir"/*; do
				[[ -f $pf ]] || continue
				id="${pf##*/}"
				owner=""
				while IFS='=' read -r key val || [[ -n $key ]]; do
					[[ $key == server ]] && {
						owner="$val"
						break
					}
				done <"$pf"
				[[ $owner =~ ^[0-9]+$ ]] || continue
				[[ $owner == "$server_pid" ]] && continue
				if [[ -z ${owner_live[$owner]+x} ]]; then
					if claude_pid_is_tmux "$owner"; then
						owner_live["$owner"]=1
					else
						owner_live["$owner"]=0
					fi
				fi
				[[ ${owner_live[$owner]} == 1 ]] && protected["$id"]=1
			done
		done
	fi

	local dir f mt
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_TASKS_DIR" "$CLAUDE_NAMES_DIR" "$CLAUDE_INTERRUPT_DIR" "$CLAUDE_WATCHERS_DIR" "$CLAUDE_LIVE_DIR"; do
		[[ -d $dir ]] || continue
		for f in "$dir"/*; do
			[[ -f $f ]] || continue
			[[ -n ${protected[${f##*/}]+x} ]] && continue
			mt=$("$OG_STAT" -c %Y "$f" 2>/dev/null || echo 0)
			((mt < server_start)) && rm -f "$f"
		done
	done
	mkdir -p "$CLAUDE_STATUS_DIR"
	if [[ -n $server_pid ]]; then
		printf '%s\n' "$server_start" >"$gate"
		local m mpid
		for m in "$marker".*; do
			[[ -f $m ]] || continue
			mpid="${m##*.server_start.}"
			[[ $mpid == "$server_pid" ]] && continue
			kill -0 "$mpid" 2>/dev/null || rm -f "$m"
		done
	fi
	printf '%s\n' "$server_start" >"$marker"
}

# claude_progress_emit PANE_ID STATE
# Writes ConEmu/kitty OSC 9;4 to the pane's tty. processing/compacting set
# indeterminate progress (no fake percent); anything else, including clear,
# unsets it. One-shot: tmux resends the stored sequence when the client later
# focuses the pane. Never aborts the caller — missing tmux, empty tty, and a
# failed write (dead pane, Permission denied) are all success.
claude_progress_emit() {
	local pane=$1 state=$2 tty seq
	command -v tmux >/dev/null || return 0
	[[ $pane == %* ]] || pane="%${pane}"
	tty=$(tmux display-message -p -t "$pane" '#{pane_tty}' 2>/dev/null) || return 0
	[[ -n $tty ]] || return 0
	case "$state" in
	processing | compacting) seq=$'\033]9;4;3;0\033\\' ;;
	*) seq=$'\033]9;4;0;0\033\\' ;;
	esac
	{ printf '%s' "$seq" >"$tty"; } 2>/dev/null || true
	return 0
}

# claude_reap_pane PANE_ID
# Single-id unlink for pane-exited/pane-died. CLAUDE_STATUS_DIR is a bare /tmp
# path shared by every tmux server on the machine, and pane ids are per-server
# %N counters, so this never iterates a directory: the file's session= field is
# the only ownership evidence once the pane is gone. Guard residuals, accepted:
# two servers with a same-named session and a colliding id pass; a
# rename-session between the last write and death fails closed and waits for
# the backstop. names/ and live/ are excluded — ids are monotonic within a
# server run, so that residue can never attach to a new pane;
# claude_prune_stale_state already owns them.
claude_reap_pane() {
	local id="${1:-}"
	[[ $id == %* ]] || id="%${id}"
	# Fail closed on anything that isn't a pane id, same posture as the sweep's
	# row check (#373). Files on disk are the bare N, so strip after validating.
	[[ $id =~ ^%[0-9]+$ ]] || return 0
	id="${id#%}"

	local pane_file="$CLAUDE_PANES_DIR/$id"
	if [[ -f $pane_file ]]; then
		local sess="" key val
		while IFS='=' read -r key val; do
			[[ $key == session ]] && {
				sess="$val"
				break
			}
		done <"$pane_file"
		if [[ -n $sess ]]; then
			tmux has-session -t "=$sess" 2>/dev/null || return 0
		fi
	fi

	claude_progress_emit "$id" clear
	rm -f "$CLAUDE_PANES_DIR/$id" "$CLAUDE_SCREEN_DIR/$id" "$CLAUDE_INTERRUPT_DIR/$id" \
		"$CLAUDE_TASKS_DIR/$id" "$CLAUDE_ISSUES_DIR/$id" "$CLAUDE_WATCHERS_DIR/$id"
}

# claude_clear_agent_state PANE_ID SESSION
# Clears an exited agent's state from the pane-shell-prompt hook (OSC 133;A,
# #646): the shell redrew its prompt, so any foreground agent has finished. The
# ownership guard is stronger than claude_reap_pane's because the pane is ALIVE
# here — the firing pane's own session name (passed from #{session_name}) is
# compared against the state file's session= field, not just existence-checked.
#
# Clears the modern state only: panes/screen/interrupt (so shell AND Go
# consumers stop rendering it) plus the @claude_status/@agent_screen pane
# options the bridge shipper reads. tasks/issues/names are deliberately left:
# they are the workspace identity the floor never touched, owned by
# claude-status-update clear (clean SessionEnd) and claude_reap_pane (death).
claude_clear_agent_state() {
	local id="${1:-}" sess="${2:-}"
	[[ $id == %* ]] || id="%${id}"
	[[ $id =~ ^%[0-9]+$ ]] || return 0
	id="${id#%}"

	# Short-circuit: no agent state on this pane — one [[ -f ]] per prompt, no
	# forks. The clearable options are written atomically with their file, so a
	# file-less stale option is unreachable except by a partial failure, which
	# the next write/clear re-syncs.
	[[ -f $CLAUDE_PANES_DIR/$id || -f $CLAUDE_SCREEN_DIR/$id || -f $CLAUDE_INTERRUPT_DIR/$id ]] || return 0

	# Ownership: clear only when the file's session= matches the firing pane's
	# (or names none — a screen-only pane, same residual as claude_reap_pane).
	local pane_file="$CLAUDE_PANES_DIR/$id"
	if [[ -f $pane_file ]]; then
		local file_sess="" key val
		while IFS='=' read -r key val || [[ -n $key ]]; do
			[[ $key == session ]] && {
				file_sess="$val"
				break
			}
		done <"$pane_file"
		# Fail closed: an empty caller session proves nothing, so it must not
		# clear a file naming a real owner — and an empty $sess would defeat
		# the mismatch check below by never being "!=" anything real.
		[[ -z $sess ]] && return 0
		[[ -n $file_sess && $file_sess != "$sess" ]] && return 0
	fi

	claude_progress_emit "$id" clear
	rm -f "$CLAUDE_PANES_DIR/$id" "$CLAUDE_SCREEN_DIR/$id" "$CLAUDE_INTERRUPT_DIR/$id"
	tmux set -pq -t "%${id}" @claude_status "" \; set -pq -t "%${id}" @agent_screen "" 2>/dev/null || true
}

# claude_clear_window_display WINDOW_TARGET MANUAL_NAME
# The option half of the #671 reset, safe on a client-independent timer: no
# file deletions. CLAUDE_STATUS_DIR is a bare /tmp path shared by every tmux
# server on the machine, and both callers — tmux-update-icons.sh's sweep
# (arm_agent_detect, #692) and tmux-shell-prompt.sh's OSC-133 prompt hook — run
# with no client guarantee, so a deletion there would let a scratch server wipe
# the real server's live naming state. Always clears @window_has_agent; clears
# @window_ai_name/@window_task only when MANUAL_NAME != 1, the same rule
# claude_clear_window_naming applies to its file half. @crew_name/@crew_color
# are dispatcher-owned and never touched.
claude_clear_window_display() {
	local target="$1" manual="$2"

	tmux set -qw -t "$target" @window_has_agent ""
	[[ $manual == 1 ]] && return 0
	tmux set -qw -t "$target" @window_ai_name "" \; set -qw -t "$target" @window_task ""
}

# claude_clear_window_naming WINDOW_TARGET MANUAL_NAME PANE_ID...
# The one path that may delete a window's shared-dir naming state, and the only
# caller left is tmux-update-icons.sh's client-gated per-tick backstop under the
# @window_naming_dirty mark — the OSC-133 event hook and the #692 sweep both
# clear the options and stamp that mark instead (they have no client guarantee,
# and CLAUDE_STATUS_DIR is machine-global). Always removes issues/<pane> for
# every PANE_ID — issue self-reports "die with the pane or CC session" per the
# CLAUDE.md "Issue self-report" bullet, and losing the window's last agent is
# exactly that death, independent of naming/display mode. When MANUAL_NAME != 1
# (the window has never had @window_manual_name stamped by the user's own
# prefix + , rename), also removes names/<pane>/tasks/<pane>. Delegates the
# option writes to claude_clear_window_display, and consumes the
# @window_naming_dirty mark last, after the files are gone, so a failed delete
# leaves the deletion still owed.
claude_clear_window_naming() {
	local target="$1" manual="$2"
	shift 2
	local id

	for id in "$@"; do
		id="${id#%}"
		rm -f "$CLAUDE_ISSUES_DIR/$id"
	done
	claude_clear_window_display "$target" "$manual"
	if [[ $manual != 1 ]]; then
		for id in "$@"; do
			id="${id#%}"
			rm -f "$CLAUDE_NAMES_DIR/$id" "$CLAUDE_TASKS_DIR/$id"
		done
	fi
	tmux set -qw -t "$target" @window_naming_dirty ""
}

# claude_reap_dead_panes ROWS
# BACKSTOP for death paths no pane hook fires on: kill-pane, kill-window,
# kill-session, respawn-pane -k, and a server crash (measured matrix in
# SPEC.md). pane-exited/pane-died own the common process-exit path via
# claude_reap_pane. ROWS is tmux list-panes -a output: "%N|..." per line, extra
# columns ignored. Removes panes/screen/interrupt/tasks/issues/watchers state
# for any pane id not present in ROWS. Full-server positive evidence: the
# caller must only pass ROWS from a list-panes call that is known to have
# succeeded and returned real data (see tmux-update-icons.sh) — an empty/failed
# ROWS is a no-op here, never reaping anything, so a bad call site can only
# under-reap, not wipe.
#
# '|' and not a tab: tmux rewrites non-printable bytes to "_" unless the
# querying client's locale is UTF-8, so a tab-delimited format collapses to one
# field, every pane id reads as dead, and this deleted every state file once
# per 5s (#373).
claude_reap_dead_panes() {
	local rows="$1"
	[[ -n $rows ]] || return 0

	local -A live=()
	local pid rest
	while IFS='|' read -r pid rest; do
		[[ -n $pid ]] || continue
		# Fail closed on a row that isn't a pane id. The empty-ROWS guard above
		# only catches "no rows at all"; in #373 the rows were non-empty and
		# merely unsplit, so every id read as dead and the sweep wiped
		# everything. A malformed row means don't reap, not reap all.
		[[ $pid =~ ^%[0-9]+$ ]] || return 0
		live["${pid#%}"]=1
	done <<<"$rows"

	local dir f id
	local -A cleared=()
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" "$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[[ -d $dir ]] || continue
		for f in "$dir"/*; do
			[[ -f $f ]] || continue
			id="${f##*/}"
			[[ -n ${live[$id]:-} ]] && continue
			if [[ -z ${cleared[$id]:-} ]]; then
				claude_progress_emit "$id" clear
				cleared[$id]=1
			fi
			rm -f "$f"
		done
	done
}

# claude_live_epoch PATH
# Sets REPLY to the epoch second a presence stamp holds; returns 1 for anything
# that isn't one complete decimal line. `read`'s exit status is the completeness
# check (0 only once it reaches the newline) because the writer truncates in
# place: a prefix caught mid-write still matches ^[0-9]+$ and reads as decades
# old, which would fake a dead pane.
claude_live_epoch() {
	local line
	# 2> before <, not after: redirections apply left to right, so a trailing
	# stderr redirect is not yet in effect when the open fails — an unstamped
	# pane would log to the tmux server once per tick.
	IFS= read -r line 2>/dev/null <"$1" || return 1
	[[ $line =~ ^[0-9]+$ ]] || return 1
	REPLY=$line
}

# claude_agent_gone STATE TIMESTAMP PANE_ID
# True when STATE should be withdrawn because the presence sweep says PANE_ID
# stopped running an agent (see CLAUDE_ASSUME_DEAD_AFTER above).
claude_agent_gone() {
	local state="$1" timestamp="$2" id="$3"
	[[ -n $timestamp ]] || return 1

	# The screen override's correctable set, and for its reasons: `waiting`/
	# `error`/`denied` block a human and are hook-only, `interrupted` is pinned
	# bright on purpose, `idle` has no staleness threshold to be past.
	local stale_after=0
	case "$state" in
	compacting) stale_after=$CLAUDE_STALE_COMPACTING ;;
	processing) stale_after=$CLAUDE_STALE_PROCESSING ;;
	done) stale_after=$CLAUDE_STALE_DONE ;;
	*) return 1 ;;
	esac
	((CLAUDE_NOW - timestamp > stale_after)) || return 1

	# Publisher liveness: a stale .sweep means no sweep is running, so no
	# per-pane stamp can be read as evidence of anything.
	claude_live_epoch "$CLAUDE_LIVE_DIR/.sweep" || return 1
	((CLAUDE_NOW - REPLY <= CLAUDE_LIVE_SWEEP_FRESH)) || return 1

	# The floor keeps a misconfigured tiny threshold from vetoing a live agent
	# in the gap between two sweeps.
	local dead_after=$CLAUDE_ASSUME_DEAD_AFTER
	((dead_after < CLAUDE_LIVE_SWEEP_FRESH)) && dead_after=$CLAUDE_LIVE_SWEEP_FRESH
	claude_live_epoch "$CLAUDE_LIVE_DIR/$id" || return 1
	((CLAUDE_NOW - REPLY > dead_after))
}

# read_pane_state PANE_FILE_PATH
# Reads a pane state file and computes its staleness fade.
# Sets REPLY to the state string, REPLY_FADE to 0..100 (0 = fresh/full color,
# 100 = fully dim), REPLY_UNSEEN to 0 or 1, REPLY_TS to the pane's last-write
# epoch (the "last active" time; empty when the file carries no timestamp), and
# REPLY_BG to the scraper's count of still-running background shells.
# Unseen means the agent reached a terminal state while the user was in another
# window. It pins the fade to 0 — the icon stays bright until the user focuses
# that window.
#
# Merges two sources keyed by the same pane id: the hook-written pane file
# (PANE_FILE_PATH, e.g. panes/<id>) and a screen-scraped state at
# screen/<id> (written by agent-detect for non-Claude agents). A fresh hook
# always wins — screen is only ground truth once the hook has gone stale
# (past its CLAUDE_STALE_* threshold) with no fresher signal of its own. The
# interrupt reclassifier below only makes sense for a stale *hook* reading
# with no screen fallback, so it's gated to that path.
# Returns 1 if neither source has a state, or if claude_agent_gone withdraws the
# one they resolved — a derived verdict, like `interrupted`: nothing on disk is
# rewritten or deleted.
read_pane_state() {
	local pane_file="$1"

	local state="" timestamp="" unseen="" session="" transcript="" key val
	if [[ -f $pane_file ]]; then
		while IFS='=' read -r key val; do
			case "$key" in
			state) state="$val" ;;
			timestamp) timestamp="$val" ;;
			unseen) unseen="$val" ;;
			session) session="$val" ;;
			transcript) transcript="$val" ;;
			esac
		done <"$pane_file"
	fi

	local screen_file="$CLAUDE_SCREEN_DIR/${pane_file##*/}"
	local screen_state="" screen_timestamp="" screen_bg=""
	if [[ -f $screen_file ]]; then
		while IFS='=' read -r key val; do
			case "$key" in
			state) screen_state="$val" ;;
			timestamp) screen_timestamp="$val" ;;
			bg) screen_bg="$val" ;;
			esac
		done <"$screen_file"
	fi

	# The badge is orthogonal to the state, so it is taken from the screen file
	# whichever source goes on to win the state below: a hook-fresh `processing`
	# pane can still be holding background shells, and only the scraper sees them.
	REPLY_BG=0
	[[ $screen_bg =~ ^[0-9]+$ ]] && REPLY_BG="$screen_bg"

	if [[ -n $state ]]; then
		# max_age gates the screen-override only. The scraper distinguishes an
		# active spinner from a quiet input box and nothing more, so it must not
		# override a human-blocking hook state: `waiting`/`error`/`denied` look
		# identical to `idle` on screen and would be wrongly downgraded (the
		# session tally keeps them, so the window would silently disagree). Those
		# states are self-correcting — the next hook write clears them — so only
		# stale *active* states, which can stick when a completion hook is
		# missed, are corrected from the screen's live reading.
		local max_age=0
		case "$state" in
		compacting) max_age=$CLAUDE_STALE_COMPACTING ;;
		processing) max_age=$CLAUDE_STALE_PROCESSING ;;
		done) max_age=$CLAUDE_STALE_DONE ;;
		esac

		if ((max_age > 0)) && ((CLAUDE_NOW - timestamp > max_age)) && [[ -n $screen_state ]]; then
			# Hook stale past its threshold and a parser reading exists — screen
			# is live ground truth. Skip the reclassifier below: it only makes
			# sense for the hook's own `processing` state.
			state="$screen_state"
			timestamp="$screen_timestamp"
			unseen=""
			session=""
			transcript=""
			if [[ $state != processing && $state != compacting ]]; then
				claude_progress_emit "${pane_file##*/}" clear
			fi
		else
			# Hook governs (fresh, no threshold, or stale with no screen
			# fallback). Reclassify a long-quiet `processing` pane as
			# `interrupted` when the transcript tail holds the interrupt marker
			# (see CLAUDE_INTERRUPT_* above). Pinned bright (no stale entry →
			# fade 0; unseen=1) so the window stays noticeable until the next
			# prompt overwrites the file back to `processing`.
			if [[ $state == processing && -n $transcript && -n $timestamp ]] &&
				((CLAUDE_NOW - timestamp > CLAUDE_INTERRUPT_CHECK_AGE)); then
				# Reuse the cached verdict while nothing was appended to the
				# transcript ([[ -nt ]] is a fork-free mtime compare); a new
				# marker makes the transcript newer and forces a re-read. An
				# empty read (stamp caught mid-write by a concurrent poller)
				# falls through to a recompute.
				local stamp="$CLAUDE_INTERRUPT_DIR/${pane_file##*/}" verdict=""
				[[ $stamp -nt $transcript ]] && IFS= read -r verdict <"$stamp" 2>/dev/null
				if [[ -z $verdict ]]; then
					local tailbuf
					tailbuf=$(tail -n 2 "$transcript" 2>/dev/null) || tailbuf=""
					verdict=0
					[[ $tailbuf == *"$CLAUDE_INTERRUPT_MARKER"* ]] && verdict=1
					[[ -d $CLAUDE_INTERRUPT_DIR ]] || mkdir -p "$CLAUDE_INTERRUPT_DIR"
					printf '%s\n' "$verdict" >"$stamp"
				fi
				if [[ $verdict == 1 ]]; then
					state="interrupted"
					unseen="1"
					claude_progress_emit "${pane_file##*/}" clear
				fi
			fi
		fi
	elif [[ -n $screen_state ]]; then
		state="$screen_state"
		timestamp="$screen_timestamp"
	else
		return 1
	fi

	if ((CLAUDE_ASSUME_DEAD_AFTER > 0)) && claude_agent_gone "$state" "$timestamp" "${pane_file##*/}"; then
		claude_progress_emit "${pane_file##*/}" clear
		return 1
	fi

	REPLY_FADE=0
	if [[ -n $timestamp ]]; then
		local age=$((CLAUDE_NOW - timestamp)) start=0
		case "$state" in
		waiting) start=$CLAUDE_STALE_WAITING ;;
		compacting) start=$CLAUDE_STALE_COMPACTING ;;
		processing) start=$CLAUDE_STALE_PROCESSING ;;
		done) start=$CLAUDE_STALE_DONE ;;
		error) start=$CLAUDE_STALE_ERROR ;;
		denied) start=$CLAUDE_STALE_DENIED ;;
		esac
		if ((start > 0 && age > start)); then
			if ((age >= start + CLAUDE_FADE_DURATION)); then
				REPLY_FADE=100
			else
				REPLY_FADE=$(((age - start) * 100 / CLAUDE_FADE_DURATION))
			fi
		fi
	fi

	REPLY_UNSEEN=0
	[[ $unseen == "1" ]] && REPLY_UNSEEN=1

	REPLY_SESSION="$session"
	REPLY_TRANSCRIPT="$transcript"
	REPLY_TS="$timestamp"
	REPLY="$state"
}

# claude_pane_ids
# Emits the union of pane ids (basenames, no leading %) that have EITHER a
# hook state file (panes/<id>) OR a screen state file (screen/<id>), one per
# line, deduped, sorted numerically. Renderers iterate this instead of
# globbing panes/* alone so screen-only panes (non-Claude agents with no
# hook, e.g. Codex) surface too — read_pane_state already merges both sources
# once handed a panes/<id> path. Sorted output gives callers (e.g. issue-id
# collection) a stable order — associative-array iteration order is
# unspecified. Sorted in bash (ids are small integers, the set is a handful):
# forking `sort` costs more than sorting on this once-a-second path.
claude_pane_ids() {
	local -A seen=()
	local f id
	for f in "$CLAUDE_PANES_DIR"/* "$CLAUDE_SCREEN_DIR"/*; do
		[[ -f $f ]] || continue
		id="${f##*/}"
		seen["$id"]=1
	done
	local ids=("${!seen[@]}") i j k
	for ((i = 1; i < ${#ids[@]}; i++)); do
		k=${ids[i]}
		for ((j = i - 1; j >= 0 && ids[j] > k; j--)); do
			ids[j + 1]=${ids[j]}
		done
		ids[j + 1]=$k
	done
	[[ -n ${ids[*]:-} ]] || return 0
	printf '%s\n' "${ids[@]}"
}

# claude_ago SECONDS
# Formats an age in seconds as a single compact unit: 47s, 5m, 2h, 3d.
# Sets REPLY. Mirrors relAgo in picker/statusline/claude.go.
claude_ago() {
	local s="$1"
	if ((s < 60)); then
		REPLY="${s}s"
	elif ((s < 3600)); then
		REPLY="$((s / 60))m"
	elif ((s < 86400)); then
		REPLY="$((s / 3600))h"
	else
		REPLY="$((s / 86400))d"
	fi
}

# claude_state_icon STATE
# Maps state to its plain icon glyph (no color codes).
# Sets REPLY to the icon string, empty if unknown state.
claude_state_icon() {
	case "$1" in
	processing) REPLY="${CLAUDE_SPINNER_FRAMES[$((CLAUDE_NOW % ${#CLAUDE_SPINNER_FRAMES[@]}))]}" ;;
	waiting) REPLY="$CLAUDE_ICON_WAITING" ;;
	compacting) REPLY="$CLAUDE_ICON_COMPACTING" ;;
	done) REPLY="$CLAUDE_ICON_DONE" ;;
	idle) REPLY="$CLAUDE_ICON_IDLE" ;;
	error) REPLY="$CLAUDE_ICON_ERROR" ;;
	denied) REPLY="$CLAUDE_ICON_DENIED" ;;
	interrupted) REPLY="$CLAUDE_ICON_INTERRUPTED" ;;
	*) REPLY="" ;;
	esac
}

# setup_claude_colors [FLAVOR]
# Detects light/dark theme and sets per-state raw hex (H_*) plus the formatted
# C_* tmux color strings built from them. Must be called before any of the
# color/icon helpers below.
#
# Precedence: an explicit FLAVOR argument (a pre-expanded @catppuccin_flavor
# value, e.g. "latte"/"mocha" — not "light"/"dark") wins outright, fork-free.
# Otherwise, when $TMUX is set, one show-options fork reads the live
# @catppuccin_flavor. Only when neither yields a flavor does this fall back to
# parsing $XDG_STATE_HOME/theme-state.json (written by the external
# theme-toggle tool), defaulting to dark.
setup_claude_colors() {
	local flavor=${1:-}
	if [[ -z $flavor && -n ${TMUX:-} ]]; then
		flavor="$(tmux show-options -gv @catppuccin_flavor 2>/dev/null)"
	fi

	local theme
	if [[ -n $flavor ]]; then
		theme="dark"
		[[ $flavor == latte ]] && theme="light"
	else
		local theme_file="${XDG_STATE_HOME:-$HOME/.local/state}/theme-state.json"
		theme="dark"
		if [[ -f $theme_file ]]; then
			# Fork-free parse: slurp the small file and regex out the theme value
			# (grep|cut here forked twice on every colored status render).
			local content=""
			IFS= read -r -d '' content <"$theme_file" 2>/dev/null || true
			[[ $content =~ \"theme\"[[:space:]]*:[[:space:]]*\"([^\"]*)\" ]] && theme="${BASH_REMATCH[1]}"
		fi
	fi

	if [[ $theme == "light" ]]; then
		H_W="#fe640b" H_K="#04a5e5" H_P="#179299" H_D="#40a02b" H_I="#6c6f85" H_E="#d20f39" H_DN="#df8e1d" H_INT="#8839ef"
	else
		H_W="#fab387" H_K="#89dceb" H_P="#94e2d5" H_D="#a6e3a1" H_I="#6c7086" H_E="#f38ba8" H_DN="#f9e2af" H_INT="#cba6f7"
	fi
	C_W="#[fg=$H_W]" C_K="#[fg=$H_K]" C_P="#[fg=$H_P]" C_D="#[fg=$H_D]" C_I="#[fg=$H_I]" C_E="#[fg=$H_E]" C_DN="#[fg=$H_DN]" C_INT="#[fg=$H_INT]"
	C_R="#[fg=default]"
}

# fade_hex FROM_HEX TO_HEX PCT
# Linearly interpolates between two #rrggbb colors; PCT 0 = FROM, 100 = TO.
# Sets REPLY to the resulting #rrggbb. Pure arithmetic — safe in hot paths.
fade_hex() {
	local from=$1 to=$2 pct=$3
	local fr=$((16#${from:1:2})) fg=$((16#${from:3:2})) fb=$((16#${from:5:2}))
	local tr=$((16#${to:1:2})) tg=$((16#${to:3:2})) tb=$((16#${to:5:2}))
	printf -v REPLY '#%02x%02x%02x' \
		$((fr + (tr - fr) * pct / 100)) \
		$((fg + (tg - fg) * pct / 100)) \
		$((fb + (tb - fb) * pct / 100))
}

# claude_faded_hex STATE [FADE] [UNSEEN]
# Sets REPLY to the state's hex, eased toward dim grey (H_I) by FADE (0..100).
# UNSEEN=1 pins to full color. Empty for an unknown state.
# Must call setup_claude_colors first.
claude_faded_hex() {
	case "$1" in
	waiting) REPLY=$H_W ;;
	compacting) REPLY=$H_K ;;
	processing) REPLY=$H_P ;;
	done) REPLY=$H_D ;;
	idle) REPLY=$H_I ;;
	error) REPLY=$H_E ;;
	denied) REPLY=$H_DN ;;
	interrupted) REPLY=$H_INT ;;
	*)
		REPLY=""
		return
		;;
	esac
	local fade=${2:-0}
	[[ ${3:-0} == 1 ]] && fade=0
	# `if`, not `&&` — a trailing false `((...))` would make this the function's
	# non-zero exit status and trip `set -e` in callers (claude-status).
	if ((fade > 0)); then
		fade_hex "$REPLY" "$H_I" "$fade"
	fi
}

# claude_colored_icon STATE [FADE] [UNSEEN]
# Returns tmux-colored icon string for a state.
# Sets REPLY to "#[fg=...]ICON#[fg=default] " or empty if unknown.
# FADE 0..100 eases the color toward dim grey; UNSEEN=1 keeps it bright.
# Must call setup_claude_colors first.
claude_colored_icon() {
	claude_state_icon "$1"
	local icon=$REPLY
	[[ -n $icon ]] || {
		REPLY=""
		return
	}
	claude_faded_hex "$1" "${2:-0}" "${3:-0}"
	REPLY="#[fg=${REPLY}]${icon}${C_R} "
}

# claude_bg_badge COUNT
# Returns the tmux-colored background-shell badge, or empty for a zero count.
# Additive to the state icon rather than part of it — a pane can hold background
# shells in any state, so this never enters claude_priority_state (same shape as
# the draft marker on a PR badge). Never faded: an old background shell is more
# interesting than a fresh one, not less.
# Must call setup_claude_colors first.
claude_bg_badge() {
	if (($1 > 0)); then
		REPLY="#[fg=${H_K}]${CLAUDE_ICON_BG}${1}${C_R} "
	else
		REPLY=""
	fi
}

# claude_priority_state WAITING COMPACTING PROCESSING DONE IDLE ERROR DENIED INTERRUPTED
# Given counts per state, returns the highest-priority non-zero state.
# Sets REPLY to state string, empty if all zero.
claude_priority_state() {
	local w=$1 k=$2 p=$3 d=$4 i=$5 e=${6:-0} dn=${7:-0} int=${8:-0}
	if ((e > 0)); then
		REPLY="error"
	elif ((w > 0)); then
		REPLY="waiting"
	elif ((dn > 0)); then
		REPLY="denied"
	elif ((k > 0)); then
		REPLY="compacting"
	elif ((int > 0)); then
		REPLY="interrupted"
	elif ((p > 0)); then
		REPLY="processing"
	elif ((d > 0)); then
		REPLY="done"
	elif ((i > 0)); then
		REPLY="idle"
	else
		REPLY=""
	fi
}

# tally_claude_state STATE ARRAY_PREFIX
# Increments the associative array entry ${ARRAY_PREFIX}[$STATE] by 1.
# Usage: tally_claude_state "processing" "win_claude"
#   → increments win_claude_processing
# This is a convenience for the common tally pattern.
tally_claude_state() {
	local state="$1" prefix="$2"
	local varname="${prefix}_${state}"
	# Use nameref for clean indirect access
	declare -n _ref="$varname" 2>/dev/null || return 0
	_ref=$((_ref + 1))
}

# format_issue_list MAX [ID...]
# Joins issue ids with spaces, capped at MAX ids followed by "+N" overflow.
# Sets REPLY to e.g. "ENG-1 ENG-2 ENG-3 +2", empty if no ids.
format_issue_list() {
	local max="$1"
	shift
	if (($# == 0)); then
		REPLY=""
		return
	fi
	if (($# <= max)); then
		REPLY="$*"
		return
	fi
	local overflow=$(($# - max))
	REPLY="${*:1:max} +$overflow"
}
