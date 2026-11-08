#!/usr/bin/env bats
#
# Writer side of launcher-hosted agent occupancy (#1091): claude-status-update
# records `agent_pid=<N>` in panes/<id> — the pid of the agent process hosting
# the hook. Run RAW (as tests/claude-issues.bats does) with a fake `tmux` so the
# script reaches the pane-file write, and a fake `ps` for the ancestor walk.

load helper

CSU="scripts/claude-status-update.sh"

setup() {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	# Hermetic: ignore the tmux session the developer runs bats from
	unset TMUX TMUX_PANE CLAUDE_AGENT_PID
	# The raw placeholder must match nothing; pin a known list for the walk.
	export AGENT_COMMANDS="claude opencode pi"

	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	PS_LOG="$BATS_TEST_TMPDIR/ps.log"
	PS_TABLE="$BATS_TEST_TMPDIR/ps.table"
	PS_COUNT="$BATS_TEST_TMPDIR/ps.count"
	export PS_LOG PS_TABLE PS_COUNT
	: >"$PS_LOG"
	: >"$PS_TABLE"
	printf '0\n' >"$PS_COUNT"

	# Answers the ancestor walk from PS_TABLE by call order, not by pid (the
	# hook's real $PPID is a bats-internal pid). Each line: "<ppid> <comm>|<args>".
	# A `-o ppid=,comm=` query advances to the next line; `-o args=` answers from
	# the line the last comm query read. Every query's pid is logged.
	cat >"$FAKEBIN/ps" <<'EOF'
#!/usr/bin/env bash
pid="${*: -1}"
printf '%s %s\n' "$1 $2" "$pid" >>"$PS_LOG"
n=$(<"$PS_COUNT")
if [[ $2 == ppid=,comm= ]]; then
	n=$((n + 1))
	printf '%s\n' "$n" >"$PS_COUNT"
fi
line=$(sed -n "${n}p" "$PS_TABLE")
[[ -n $line ]] || exit 1
left=${line%%|*}
args=${line#*|}
if [[ $2 == ppid=,comm= ]]; then
	printf ' %s\n' "$left"
else
	printf '%s\n' "$args"
fi
EOF
	# The flake sandbox has no /usr/bin/env: point the shebang at this bash.
	sed -i "1s|.*|#!$BASH|" "$FAKEBIN/ps"
	chmod +x "$FAKEBIN/ps"

	# Fake tmux: enough for the hook to reach the write. pid|session for the
	# combined query, "1" (focused window) for anything else.
	cat >"$FAKEBIN/tmux" <<'EOF'
#!/usr/bin/env bash
case "$*" in
*'#{pid}|#{session_name}'*) echo '4242|sess' ;;
*'#{pid}'*) echo 4242 ;;
*) echo 1 ;;
esac
EOF
	sed -i "1s|.*|#!$BASH|" "$FAKEBIN/tmux"
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
}

pane_file() { printf '%s/panes/%s' "$CLAUDE_STATUS_DIR" "$1"; }

# Value of KEY in pane file ID, empty when the line is absent.
pane_val() {
	grep -m1 "^$2=" "$(pane_file "$1")" | cut -d= -f2 || true
}

ps_calls() { wc -l <"$PS_LOG" | tr -d ' '; }

# A pid that is guaranteed dead: spawn, reap.
dead_pid() {
	sleep 0 &
	local p=$!
	wait "$p"
	printf '%s' "$p"
}

@test "agent_pid: CLAUDE_AGENT_PID env is written" {
	CLAUDE_AGENT_PID=$$ run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ "$(pane_val 7 agent_pid)" = "$$" ]
	[ "$(pane_val 7 server)" = "4242" ]
	# Seam wins: no ancestor walk at all.
	[ "$(ps_calls)" -eq 0 ]
}

@test "agent_pid: non-numeric CLAUDE_AGENT_PID is ignored" {
	CLAUDE_AGENT_PID='12;x' run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -z "$(pane_val 7 agent_pid)" ]
}

@test "agent_pid: preserved while the stored pid is alive, without any ps fork" {
	CLAUDE_AGENT_PID=$$ bash "$CSU" processing --pane %7
	# Second hook: no env seam, ps would answer with a different agent if asked.
	printf '1 claude|claude\n' >"$PS_TABLE"
	run bash "$CSU" "done" --pane %7
	[ "$status" -eq 0 ]
	[ "$(pane_val 7 agent_pid)" = "$$" ]
	[ "$(ps_calls)" -eq 0 ]
}

@test "agent_pid: re-resolved when the stored pid is dead" {
	dead=$(dead_pid)
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	printf 'state=idle\ntimestamp=1\nsession=sess\nserver=4242\nagent_pid=%s\n' "$dead" >"$(pane_file 7)"
	CLAUDE_AGENT_PID=$$ run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ "$(pane_val 7 agent_pid)" = "$$" ]
}

@test "agent_pid: dead stored pid with no seam walks ancestors again" {
	dead=$(dead_pid)
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	printf 'state=idle\ntimestamp=1\nsession=sess\nserver=4242\nagent_pid=%s\n' "$dead" >"$(pane_file 7)"
	printf '555 claude|claude --resume\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	# The hook's own parent is the first ancestor asked about, and it matched.
	[ "$(pane_val 7 agent_pid)" != "$dead" ]
	[ -n "$(pane_val 7 agent_pid)" ]
	[ "$(ps_calls)" -eq 1 ]
}

@test "agent_pid: ancestor walk resolves to the agent ancestor" {
	# parent: status.sh shell (bash) -> its parent 801: claude
	printf '801 bash|bash status.sh\n900 claude|claude\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ "$(pane_val 7 agent_pid)" = "801" ]
	# Second query asked about the first line's ppid.
	[ "$(grep 'ppid=,comm=' "$PS_LOG" | sed -n 2p | awk '{print $NF}')" = "801" ]
}

@test "agent_pid: a makeWrapper .claude-wrapped comm matches" {
	printf '801 .claude-wrapped|claude\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -n "$(pane_val 7 agent_pid)" ]
}

@test "agent_pid: a full-path comm (darwin) and a truncated name match by basename" {
	printf '801 /nix/store/abc-claude/bin/claude|x\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -n "$(pane_val 7 agent_pid)" ]
}

@test "agent_pid: falls back to argv[0] basename when comm does not match" {
	printf '801 node|/opt/bin/claude --flag\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -n "$(pane_val 7 agent_pid)" ]
}

@test "agent_pid: absent when nothing resolves (fail closed)" {
	printf '801 bash|bash\n802 sshd|sshd\n' >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -f "$(pane_file 7)" ]
	[ -z "$(pane_val 7 agent_pid)" ]
	[ "$(pane_val 7 state)" = "processing" ]
}

@test "agent_pid: unsubstituted placeholder list matches nothing" {
	printf '801 claude|claude\n' >"$PS_TABLE"
	AGENT_COMMANDS="" run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -z "$(pane_val 7 agent_pid)" ]
}

@test "agent_pid: the ancestor walk is capped" {
	for i in $(seq 2 30); do printf '%s bash|bash\n' "$((i + 100))"; done >"$PS_TABLE"
	run bash "$CSU" processing --pane %7
	[ "$status" -eq 0 ]
	[ -z "$(pane_val 7 agent_pid)" ]
	# 8 comm queries plus up to 8 argv fallbacks.
	[ "$(ps_calls)" -le 16 ]
}

@test "agent_pid: clear removes the pane file and never calls ps" {
	CLAUDE_AGENT_PID=$$ bash "$CSU" processing --pane %7
	[ -f "$(pane_file 7)" ]
	run bash "$CSU" clear --pane %7
	[ "$status" -eq 0 ]
	[ ! -e "$(pane_file 7)" ]
	[ "$(ps_calls)" -eq 0 ]
}

@test "agent_pid: mark-seen keeps the agent_pid line and never calls ps" {
	CLAUDE_AGENT_PID=$$ bash "$CSU" "done" --pane %7
	# Force the unseen flag the way a background-window hook would have.
	printf 'unseen=1\n' >>"$(pane_file 7)"
	cat >"$BATS_TEST_TMPDIR/bin/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-panes) echo '%7' ;;
*) echo 1 ;;
esac
EOF
	sed -i "1s|.*|#!$BASH|" "$BATS_TEST_TMPDIR/bin/tmux"
	run bash "$CSU" mark-seen --session sess --window 1
	[ "$status" -eq 0 ]
	[ -z "$(pane_val 7 unseen)" ]
	[ "$(pane_val 7 agent_pid)" = "$$" ]
	[ "$(pane_val 7 state)" = "done" ]
	[ "$(ps_calls)" -eq 0 ]
}
