# Owner stamp for screen-only agent panes (#709)

Follow-up to #676 / #706. `claude_prune_stale_state` protects a pane id from
another live tmux server's boot prune only via a `server=<pid>` field in
`panes/<id>`. A pi/codex/cursor pane has `screen/<id>` and `watchers/<id>` but
no `panes/<id>`, so a second server's boot deleted them by mtime; the live
`agent-detect` watcher then exited on its missing registry file and was re-armed.

## Steps

1. `picker/agentdetect/main.go`: resolve the server pid once at startup
   (`serverPID`), pass it to `registerWatcher`, which writes `<pid>\nserver=<tmux pid>\n`
   (line 1 alone when unresolved). `ownerMatches` compares line 1 only, so old
   and new registry files both parse.
2. `statefile.Writer.WithServer`: `Update` writes `server=<pid>` after
   `timestamp=` and before flag lines.
3. Bridge shipper (`agentstatus.go`): the mirrored `screen/<id>` body carries
   the same `server=` line as its `panes/<id>` body.
4. `claude_prune_stale_state`: the owner scan covers `panes/`, `screen/` and
   `watchers/`; any foreign, live tmux owner in any of them protects the id
   across every swept dir.
5. Tests: bats cases in `tests/prune-stale-state.bats` (red before the fix);
   Go tests for `registerWatcher`/`ownerMatches`, `WithServer`, and the shipper.
6. `CLAUDE.md`: the `tmux-update-icons` row, the watcher-registry bullet and
   the screen-state bullet.

## Known residual

`claude_reap_dead_panes` (the ~60s sweep) still deletes `screen/`/`watchers/`
for ids absent from this server's pane list with no ownership guard, and files
written before this change carry no stamp. Both are out of scope here.
