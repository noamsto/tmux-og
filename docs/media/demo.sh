#!/usr/bin/env bash
# Regenerates docs/media/*.gif from docs/media/tapes/*.tape against a throwaway
# tmux-og server. Run from the repo root: `nix run .#demo [tape...]`.
#
# The flake app provides OG_DEMO_TMUX (the wrapped tmux-og), OG_DEMO_TMUX_RAW
# (the same tmux with no config), OG_DEMO_SHELL and a FONTCONFIG_FILE pinning
# the Nerd Font the tapes name, and runs this with no inherited PATH — so no
# `gh`/`linear` exists to overwrite the seeded enrich options.
set -euo pipefail

root=$PWD
tapes=$root/docs/media/tapes
if [[ ! -d $tapes ]]; then
	echo "og-demo: run from the tmux-og repo root (no docs/media/tapes here)" >&2
	exit 1
fi

# Short on purpose: the server socket lives under TMUX_TMPDIR and socket paths
# cap near 108 bytes.
work=$(mktemp -d /tmp/ogd.XXXXXX)
export TMUX_TMPDIR=$work/t
# A private TMUX_TMPDIR does not isolate agent state: CLAUDE_STATUS_DIR
# defaults to /tmp/claude-status, shared with every tmux server on the machine.
export CLAUDE_STATUS_DIR=$work/cs
# Same story for the usage cache: /tmp/og-agent-usage holds the user's real
# rate-limit numbers.
export OG_AGENT_USAGE_DIR=$work/usage
# Keeps the user's shell rc, zoxide db, gh/agent credentials and theme state out
# of the frame.
export HOME=$work/home
export XDG_CONFIG_HOME=$HOME/.config XDG_DATA_HOME=$HOME/.local/share
export XDG_STATE_HOME=$HOME/.local/state XDG_CACHE_HOME=$HOME/.cache
export GIT_CONFIG_GLOBAL=$HOME/.gitconfig
export SHELL=$OG_DEMO_SHELL
unset TMUX TMUX_PANE
mkdir -p "$TMUX_TMPDIR" "$CLAUDE_STATUS_DIR" "$OG_AGENT_USAGE_DIR" "$work/bin" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME"

cleanup() {
	"$OG_DEMO_TMUX_RAW" -L ogd-outer kill-server 2>/dev/null || true
	"$OG_DEMO_TMUX" -L ogd kill-server 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

# Every tmux call — this script's, and the ones the tapes type — lands on the
# demo server, never the user's.
cat >"$work/bin/tmux" <<EOF
#!$OG_DEMO_SHELL
exec "$OG_DEMO_TMUX" -L ogd "\$@"
EOF

# A fake agent: a transcript, then a process named `claude`, so the window icon
# and the agent-pane gates read it as one. A symlink to bash rather than
# `exec -a claude sleep`: nix's coreutils is multicall and dispatches on argv0.
ln -s "$OG_DEMO_SHELL" "$work/bin/claude"
cat >"$work/bin/og-demo-agent" <<EOF
#!$OG_DEMO_SHELL
cat "$work/transcripts/\$1"
exec claude -c 'while :; do sleep 86400; done'
EOF

# Attaches through an outer, config-less tmux whose pane the driver resizes, so
# the demo client's width changes mid-clip — vhs cannot resize its terminal.
# OG_DEMO_WIDTHS overrides the width sequence a tape walks through.
cat >"$work/bin/og-demo-reflow" <<EOF
#!$OG_DEMO_SHELL
outer() { "$OG_DEMO_TMUX_RAW" -L ogd-outer -f /dev/null "\$@"; }
outer new-session -d -s o -x "\$(tput cols)" -y "\$(tput lines)" 'env -u TMUX tmux attach -t tmux-og'
outer set -g status off
outer set -g prefix None
outer set -g pane-border-style 'fg=#1e1e2e'
outer set -g pane-active-border-style 'fg=#1e1e2e'
outer split-window -h -d -t o -l 1 'exec sleep 86400'
(
	sleep 3
	for w in \${OG_DEMO_WIDTHS:-110 88 68}; do
		outer resize-pane -t o:0.0 -x "\$w"
		sleep 2.5
	done
) >/dev/null 2>&1 &
exec "$OG_DEMO_TMUX_RAW" -L ogd-outer attach -t o
EOF
chmod +x "$work/bin/tmux" "$work/bin/og-demo-agent" "$work/bin/og-demo-reflow"
export PATH=$work/bin:$PATH

mkdir -p "$work/transcripts"
cat >"$work/transcripts/api" <<'EOF'
> add retries with jitter to the rate limiter

  Read internal/ratelimit/limiter.go
  Edit internal/ratelimit/limiter.go (+31 -4)
  Bash go test ./internal/ratelimit/...
    ok  internal/ratelimit  0.412s

  Working on the backoff cap...
EOF
cat >"$work/transcripts/web" <<'EOF'
> wire the dark mode toggle into the settings page

  Edit src/settings/Appearance.tsx (+48 -2)

  Allow Bash: pnpm add @radix-ui/react-switch ?
    1. Yes
    2. Yes, and don't ask again this session
    3. No
EOF
cat >"$work/transcripts/docs" <<'EOF'
> document the new retry settings

  Edit docs/configuration.md (+22)
  Edit CHANGELOG.md (+3)

  Done. Added a "Retries" section covering max_attempts,
  base_delay and jitter, with an example config.
EOF

# The poller finds no credentials under the private HOME and keeps this as is.
cat >"$OG_AGENT_USAGE_DIR/claude.json" <<'EOF'
{"windows":[{"label":"5h","pct":34},{"label":"7d","pct":12}]}
EOF

cat >"$HOME/.bash_profile" <<'EOF'
. ~/.bashrc
EOF
cat >"$HOME/.bashrc" <<'EOF'
PS1='\[\e[1;35m\]\W\[\e[0m\] \[\e[1;32m\]❯\[\e[0m\] '
HISTFILE=
EOF
git config --global user.name demo
git config --global user.email demo@example.com
git config --global init.defaultBranch main

mkrepo() {
	local dir=$HOME/code/$1 branch=$2
	mkdir -p "$dir/src"
	printf '# %s\n' "$1" >"$dir/README.md"
	printf 'package main\n' >"$dir/src/main.go"
	git -C "$dir" init -q
	git -C "$dir" add -A
	git -C "$dir" commit -qm "initial commit"
	[[ $branch == main ]] || git -C "$dir" switch -qc "$branch"
}
mkrepo api eng-412-rate-limit-retries
mkrepo web feat/218-dark-mode-toggle
mkrepo docs docs/retry-settings
mkrepo infra main
mkrepo dotfiles main
mkrepo blog main
mkrepo notes main

t() { tmux "$@"; }

declare -A pane
pane[api]=$(t new-session -d -P -F '#{pane_id}' -s tmux-og -x 160 -y 45 -c "$HOME/code/api" 'og-demo-agent api')
t set -g @splash_shown 1
pane[web]=$(t new-window -P -F '#{pane_id}' -t tmux-og: -c "$HOME/code/web" 'og-demo-agent web')
pane[docs]=$(t new-window -P -F '#{pane_id}' -t tmux-og: -c "$HOME/code/docs" 'og-demo-agent docs')
for w in infra blog dotfiles notes; do
	pane[$w]=$(t new-window -d -P -F '#{pane_id}' -t tmux-og: -c "$HOME/code/$w")
done
# By pane id: automatic-rename relabels windows, so a name target is a race.
t split-window -d -h -t "${pane[infra]}" -c "$HOME/code/infra"
t new-session -d -s dotfiles -x 160 -y 45 -c "$HOME/code/dotfiles"
t new-window -d -t dotfiles: -c "$HOME/code/dotfiles/src"
t new-session -d -s blog -x 160 -y 45 -c "$HOME/code/blog"
zoxide add "$HOME/code/infra" 2>/dev/null || true

seed_enrich() {
	local w=${pane[api]}
	t set -w -t "$w" @issue_provider linear
	t set -w -t "$w" @issue_id ENG-412
	t set -w -t "$w" @issue_title "Rate limiter retries without jitter"
	t set -w -t "$w" @issue_url https://linear.app/acme/issue/ENG-412
	# reflow discards a stamp whose @issue_branch does not match the window's
	# @branch, so the seed has to carry it like tmux-issue-stamp does.
	t set -w -t "$w" @issue_branch "$(t show-options -t "$w" -wqv @branch)"
	t set -w -t "$w" @pr_number 631
	t set -w -t "$w" @pr_title "feat(ratelimit): jittered retries"
	t set -w -t "$w" @pr_state open
	t set -w -t "$w" @pr_check_state pending
	t set -w -t "$w" @pr_mergeable mergeable
	t set -w -t "$w" @pr_url https://github.com/acme/api/pull/631
	w=${pane[web]}
	t set -w -t "$w" @issue_provider github
	t set -w -t "$w" @issue_id "#218"
	t set -w -t "$w" @issue_title "Dark mode toggle in settings"
	t set -w -t "$w" @issue_url https://github.com/acme/web/issues/218
	t set -w -t "$w" @issue_branch "$(t show-options -t "$w" -wqv @branch)"
	t set -w -t "$w" @pr_number 224
	t set -w -t "$w" @pr_title "feat(settings): dark mode toggle"
	t set -w -t "$w" @pr_state open
	t set -w -t "$w" @pr_check_state success
	t set -w -t "$w" @pr_mergeable mergeable
	t set -w -t "$w" @pr_draft 1
	t set -w -t "$w" @pr_url https://github.com/acme/web/pull/224
}

# Rewritten before every tape: timestamps drive the staleness fade, and a tape
# that visits the `done` window clears its unseen mark.
seed_agents() {
	local now
	now=$(date +%s)
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	# Named without the `%`, as claude-status-update writes them; the reap sweep
	# deletes any name that is not a live pane.
	printf 'state=processing\ntimestamp=%s\nsession=demo-api\n' "$now" >"$CLAUDE_STATUS_DIR/panes/${pane[api]#%}"
	printf 'state=waiting\ntimestamp=%s\nsession=demo-web\nunseen=1\n' "$now" >"$CLAUDE_STATUS_DIR/panes/${pane[web]#%}"
	printf 'state=done\ntimestamp=%s\nsession=demo-docs\nunseen=1\n' "$now" >"$CLAUDE_STATUS_DIR/panes/${pane[docs]#%}"
}

# The creation hooks stamp @issue_id from the branch and the backfill sweep
# retries a stamp missing its title; seeding twice across a sweep keeps the
# complete values last.
sleep 3
seed_enrich
sleep 6
seed_enrich

mkdir -p "$root/docs/media"
if (($#)); then
	names=("$@")
else
	names=()
	for f in "$tapes"/*.tape; do
		names+=("$(basename "$f" .tape)")
	done
fi

for name in "${names[@]}"; do
	# vhs ends a tape by killing its client, and a popup killed that way leaves
	# its "returned 129" in view mode on the pane underneath.
	t list-panes -s -t tmux-og -F '#{pane_id}|#{pane_in_mode}' | while IFS='|' read -r p mode; do
		if [[ $mode == 1 ]]; then
			t send-keys -t "$p" -X cancel
		fi
	done
	t select-window -t "${pane[api]}"
	seed_agents
	echo "og-demo: rendering $name.gif"
	vhs -o "$root/docs/media/$name.gif" "$tapes/$name.tape"
	"$OG_DEMO_TMUX_RAW" -L ogd-outer kill-server 2>/dev/null || true
done
