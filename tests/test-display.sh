#!/usr/bin/env bash
# Mock-mode window-list regression test.
# Spawns a throwaway tmux server using the built wrapper, sets @issue_*/@pr_*
# options for the documented display states, and diffs window names against a
# golden file. Run from repo root after `nix build .#default`.
#
# Why no -n on new-session/new-window: an explicit window name disables
# automatic-rename, so the window would keep its literal name instead of
# rendering automatic-rename-format. We let auto-rename (on by default in the
# baked config) drive the name so the test captures the RENDERED enrichment.
#
# Window indices: the config sets `base-index 1`, so the initial window is
# s:1 and each new-window fills the lowest free index (s:2..s:5).
set -euo pipefail

TMUX_BIN="${TMUX_BIN:-./result/bin/tmux}"
SOCKET="enrichtest-$$"
SHIM_DIR="$(mktemp -d)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXPECTED="$SCRIPT_DIR/fixtures/window-list.expected"
CLAUDE_STATUS_DIR="$(mktemp -d)"
export CLAUDE_STATUS_DIR

cleanup() {
	"$TMUX_BIN" -L "$SOCKET" kill-server 2>/dev/null || true
	rm -rf "$SHIM_DIR" "$CLAUDE_STATUS_DIR"
}
trap cleanup EXIT

t() { "$TMUX_BIN" -L "$SOCKET" "$@"; }

cat >"$SHIM_DIR/tmux" <<EOF
#!$(command -v bash)
exec "$TMUX_BIN" -L "$SOCKET" "\$@"
EOF
chmod +x "$SHIM_DIR/tmux"

t new-session -d -s s # state: no provider (branch fallback)
t set-option -t s:1 -w @branch "feat/plain-branch"

t new-window -t s # Linear + open PR pending
t set-option -t s:2 -w @issue_provider linear
t set-option -t s:2 -w @issue_id NOA-123
t set-option -t s:2 -w @pr_number 247
t set-option -t s:2 -w @pr_state open
t set-option -t s:2 -w @pr_check_state pending
t set-option -t s:2 -w @pr_glyph "*"

t new-window -t s # Linear + open PR passing
t set-option -t s:3 -w @issue_provider linear
t set-option -t s:3 -w @issue_id NOA-124
t set-option -t s:3 -w @pr_number 248
t set-option -t s:3 -w @pr_state open
t set-option -t s:3 -w @pr_check_state success
t set-option -t s:3 -w @pr_glyph "OK"

t new-window -t s # GitHub + open PR passing
t set-option -t s:4 -w @issue_provider github
t set-option -t s:4 -w @issue_id "#142"
t set-option -t s:4 -w @pr_number 249
t set-option -t s:4 -w @pr_state open
t set-option -t s:4 -w @pr_check_state success
t set-option -t s:4 -w @pr_glyph "OK"

t new-window -t s # Linear + no PR yet
t set-option -t s:5 -w @issue_provider linear
t set-option -t s:5 -w @issue_id NOA-125
t set-option -t s:5 -w @pr_number none

# Trailing whitespace is stripped per line: an unset @window_icon_display (no
# process icon on a mock pane) renders as a trailing space, which the
# trim-trailing-whitespace pre-commit hook would strip from the golden anyway.
# Normalizing here keeps the golden trim-clean and the comparison stable. This
# only strips the structurally-empty icon slot; a real icon is non-whitespace
# content and would still be captured and compared.
#
# Wait for auto-rename to settle. Renames fire window-by-window over ~300ms
# (status-interval is 1s), and a partially-renamed read can look stable for two
# consecutive polls. Require three consecutive identical reads to skip past that
# transient before locking in; cap ~3s.
prev=""
got=""
stable=0
for _ in $(seq 1 30); do
	got="$(t list-windows -t s -F '#{window_name}' | sed 's/[[:space:]]*$//')"
	if [[ -n $got && $got == "$prev" ]]; then
		stable=$((stable + 1))
		[[ $stable -ge 2 ]] && break
	else
		stable=0
	fi
	prev="$got"
	sleep 0.1
done

if [[ ${UPDATE_GOLDEN:-0} == "1" ]]; then
	printf '%s\n' "$got" >"$EXPECTED"
	echo "golden updated"
	exit 0
fi

# Golden diff is checked but not immediately fatal: the @bridge_win check
# below must still run (and report) even if this one fails, so the two are
# combined into a single exit status at the bottom.
golden_ok=1
diff <(printf '%s\n' "$got") "$EXPECTED" || golden_ok=0
if ((golden_ok)); then
	echo "display test passed"
else
	echo "golden window-list diff FAILED (see diff above)" >&2
fi

# --- @bridge_win opt-out (#167): a remote-bridge mirror window must render its
# plain tmux name only, with issue/branch/PR enrichment suppressed even when
# stamped. Added after the golden diff above so it doesn't perturb the 5-window
# fixture. -n pins the name (disables automatic-rename), mirroring how the M2
# daemon holds the remote window's name via `rename-window`.
t new-window -t s -n bridge-remote-window
t set-option -t s:6 -w @bridge_win 1
t set-option -t s:6 -w @branch "totally/unrelated-branch"
t set-option -t s:6 -w @issue_provider linear
t set-option -t s:6 -w @issue_id NOA-999
t set-option -t s:6 -w @issue_title "should not render"
t set-option -t s:6 -w @pr_number 999
t set-option -t s:6 -w @pr_state open
t set-option -t s:6 -w @pr_check_state success
t set-option -t s:6 -w @pr_glyph "OK"

# Force a reflow now that the opt-out + enrichment fields are set (the
# after-new-window hook that fired at creation predates them). Resolve the
# reflow binary via the generated tmux.conf (the wrapper's PATH entries only
# carry the bin directory, not the full binary path) rather than hardcoding a
# store hash.
CONF="$(grep -o -- '-f /nix/store/[a-z0-9]*-tmux[.]conf' "$TMUX_BIN" | head -1 | cut -d' ' -f2)"
REFLOW_BIN="$(grep -o '/nix/store/[a-z0-9]*-tmux-reflow-windows/bin/tmux-reflow-windows' "$CONF" | head -1)"
PATH="$SHIM_DIR:$PATH" "$REFLOW_BIN" s 200 --force >/dev/null 2>&1 || true

bridge_id="$(t show-options -t s:6 -wqv @window_label_id)"
bridge_rest="$(t show-options -t s:6 -wqv @window_label_rest_short)"
bridge_pr="$(t show-options -t s:6 -wqv @window_pr_plain)"

bridge_ok=1
if [[ -n $bridge_id || -n $bridge_pr || $bridge_rest != "bridge-remote-window" ]]; then
	echo "bridge window not gated: id=[$bridge_id] rest=[$bridge_rest] pr=[$bridge_pr]" >&2
	bridge_ok=0
else
	echo "bridge window gate passed"
fi

# --- draft PR (#219): the draft marker prepends the check-state glyph rather
# than replacing it. Also added after the golden diff so the 5-window fixture
# stays the reference for the non-draft states.
t new-window -t s
t set-option -t s:7 -w @issue_provider ""
t set-option -t s:7 -w @issue_id ""
t set-option -t s:7 -w @issue_title ""
t set-option -t s:7 -w @issue_branch ""
t set-option -t s:7 -w @branch "feat/draft-pr"
t set-option -t s:7 -w @pr_number 250
t set-option -t s:7 -w @pr_state open
t set-option -t s:7 -w @pr_check_state success
t set-option -t s:7 -w @pr_glyph "D OK"
t set-option -t s:7 -w @pr_draft 1

t new-window -t s # same PR state, not a draft — the control
t set-option -t s:8 -w @issue_provider ""
t set-option -t s:8 -w @issue_id ""
t set-option -t s:8 -w @issue_title ""
t set-option -t s:8 -w @issue_branch ""
t set-option -t s:8 -w @branch "feat/ready-pr"
t set-option -t s:8 -w @pr_number 251
t set-option -t s:8 -w @pr_state open
t set-option -t s:8 -w @pr_check_state success
t set-option -t s:8 -w @pr_glyph "OK"

# In an issue-named worktree, the async reconcile/enrich hook can stamp inherited
# issue metadata onto these synthetic windows after creation. Keep branch state
# consistent so reflow does not discard the PR fields as stale metadata.
t set-option -t s:7 -w @branch "feat/draft-pr"
t set-option -t s:8 -w @branch "feat/ready-pr"
t set-option -t s:7 -w @issue_branch "feat/draft-pr"
t set-option -t s:8 -w @issue_branch "feat/ready-pr"
PATH="$SHIM_DIR:$PATH" "$REFLOW_BIN" s 200 --force >/dev/null 2>&1 || true

# Compare glyph runs with the PR numbers stripped: the draft badge must be the
# ready one with something prepended, never a different state glyph.
draft_pr="$(t show-options -t s:7 -wqv @window_pr_plain)"
ready_pr="$(t show-options -t s:8 -wqv @window_pr_plain)"
draft_pr="${draft_pr%#*}"
ready_pr="${ready_pr%#*}"
draft_ok=1
if [[ $draft_pr == "$ready_pr" || $draft_pr != *"$ready_pr" ]]; then
	echo "draft badge not marked: draft=[$draft_pr] ready=[$ready_pr]" >&2
	draft_ok=0
else
	echo "draft badge passed: [$draft_pr]"
fi

((golden_ok && bridge_ok && draft_ok))
