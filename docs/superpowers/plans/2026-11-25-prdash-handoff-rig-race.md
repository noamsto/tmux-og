# prdash hand-off rig: stop racing the daemon for `@bridge_repo` / `@bridge_dir` (#1271)

## Root cause

`handoff_rig` (tests/remote-m2-integration.bats) hand-stamps `@bridge_repo` and
`@bridge_dir` on the mirror window. Both are daemon-owned: every label row the
daemon applies sets them from the remote window, and a row with no origin/dir
UNSETS them. The remote `origin` window carries no `@git_root`, so the daemon's
first label apply ships an empty repo/dir. On a loaded runner that apply lands
after the test's stamps and clears them (measured: the DST message log shows
`set-option -w -t @1 @bridge_repo o/n` followed by `set-option -uw -t @1
@bridge_repo` / `@bridge_dir` from the daemon, before the press).

`tmux-prdash-mirror.sh` then reads an empty `@bridge_repo`, takes its documented
fallback (`ctl tool <pane> prdash <dir>`, a remote prdash float) and no
hand-off is ever sent. The wt stub never runs, so whichever assertion polls
next times out: the window follow, the `handoff.args` file, or the notice.
That is why the whole family fails on a different case each run, and why the
placed-mirror cases (no stamps, a direct ctl call) never fail.

## Fix (test rig only; no daemon, launcher or docs/agents change)

1. `handoff_rig`: give the remote window `origin` `@git_root=$rig_co` before the
   bridge comes up, the way tmux-og stamps it. The daemon then probes the
   checkout's origin (`https://github.com/o/n.git`) and ships `@bridge_dir` and
   `@bridge_repo` itself.
2. Replace the two `$DST set-option` stamps with a readiness gate
   (`wait_mirror_repo`) that polls the mirror window until it carries
   `@bridge_repo=o/n` and `@bridge_dir=$rig_co`, and prints what it saw when
   it never does. This waits for a real asynchronous production pipeline
   (probe -> label row -> stamp); it is not a lengthened assertion wait.
3. Keep `@og_prdash_origin` (the bind's local stamp) and `@bridge_sock`.

## Evidence

Red loop (same command before and after; the six `prdash hand-off` cases per
iteration, vanilla pinned next-3.9 tmux rather than the tmux-og wrapper, whose
baked-in config adds hooks CI does not have):

    systemd-run --user --scope -p CPUQuota=50% taskset -c 0,1 \
      loaded.sh 'prdash hand-off' 10 2   # 10 iterations, 2 busy loops

- Before (old rig): 8/10 iterations red. An instrumented run showed the DST
  message log with `set-option -w -t @1 @bridge_repo o/n` followed by the
  daemon's `set-option -uw -t @1 @bridge_repo` / `@bridge_dir`, then the
  launcher's `display-message -p ... #{@bridge_repo}` and the `tool prdash`
  fallback float on the remote.
- After: 30/30 iterations green (3 runs of 10, each under the same load).
- Unloaded, 3 plain iterations stay green before and after: the race needs a
  slow daemon label apply.
- Gate: `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Other checks (no change needed)

No fixed `/tmp` path, session name or socket in the hand-off path: the launcher
uses `mktemp -d`, the rig uses `$BATS_TEST_TMPDIR`; `OG_TMUX_DIR` is per bats
pid. Not the cause.
