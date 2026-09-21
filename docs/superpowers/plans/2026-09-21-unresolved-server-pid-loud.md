# Make an unresolved server pid loud (#712)

Follow-up to #676 / #706 / #709 / #718. Three gaps in the pid plumbing, plus a
pipe-safety gap folded in from #717's review of #714.

## Steps

1. `scripts/tmux-update-icons.sh`: `source @lib_log@`. When `SERVER_PID` is
   unresolvable, **skip** `claude_prune_stale_state` and log
   `prune_skipped` rather than silently taking the mtime-only path — mtime
   alone cannot tell a dead server's leftovers from a live different server's
   fresh state under the shared `CLAUDE_STATUS_DIR`, so the fallback can delete
   live state (#676). The lookup is retried every tick and no marker is written
   while it fails, so a stale reap is only delayed, never lost.
2. Same script, per-tick `list-panes -a -F` row: wrap `#{@remux_relaunch}` in
   `#{s/[|]/ /:…}` like its neighbours. Unwrapped, a `|` shifts every later
   field left, corrupting `@window_manual_name`/`@window_naming_dirty` and
   letting the #692 per-tick clear delete a manually-named window's files.
3. `picker/remotebridge/daemon/agentstatus.go`: hoist `localServerPID` above
   `stamp`'s per-row loop. It caches only on success, so while unresolved it
   forked `tmux display-message` once per changed row instead of once per pass.
4. Tests: a bats case in `tests/update-icons-all-windows.bats` per script fix
   (both red before it), a Go case for the once-per-pass resolve, and the
   `@lib_log@` sed seam in the five suites that build a runnable
   `UPDATE_ICONS` copy.

## Known drift (not fixed here)

`scripts/lib-claude.sh`'s caveat (c) still calls the shared `.server_start` "the
only gate when SERVER_PID is empty". That path is now exercised only by
`tests/prune-stale-state.bats` — the production caller skips instead of falling
back. `lib-claude.sh` is owned by the sibling #711 worker, so it is untouched.
