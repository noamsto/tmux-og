# tmux upstream survey (2026-08-01 → 2026-09-24)

Pin: `3a6c2e7877e8c017edb84c8d3ee41b98abee27d3` (`flake.nix:23`). Window: commits since 2026-08-01 plus `pin...master`. Fetched: 2026-09-24T18:59Z. Commits triaged: 249. Candidates opened: 17.

| Commit | What changed | tmux-og site | Verdict | Effort |
|---|---|---|---|---|
| `b8cadef82d4186c9937d62eaa7688ad85cb764ed` | Open PR tmux#5402 adds hide-pane and show-pane; it is not on master, and #751 asks to verify the tiled-pane resize then comment. | `no site — docs/agents/floats.md#Floating Panes` | Watch | 2-4 hours |
| `d202aa7ac13f9d3e5c6153d5a07604a7f5ffd0d0` | Adds remain-on-exit value failed-key and new-pane -D so a modal pane can close on Escape or C-c. | `picker/remotebridge/daemon/daemon.go:225` | Watch | 2-4 hours |
| `4624bc2129c5e723a5386d36003599eb3f2e6bb7` | Zoomed v2 layout dumps now always emit a z index for every floating cell, including saved cells of a zoomed pane (tmux#5624). The `z` key is unchanged. | `docs/agents/bridge-daemon.md:250` | Watch | 1-2 hours |

## Recommendation

Do not bump the pin now. Nothing to adopt. The 32 commits after the pin leave the version string `next-3.9` (`flake.nix:65`), leave the `CLIENT_CONTROL` bail the popup patch covers (`flake.nix:71`), and do not change `%begin`/`%end` or control-mode subscriptions, so no Risk row blocks a later bump. A pin bump should re-check zoomed float z-order because of `4624bc2129c5e723a5386d36003599eb3f2e6bb7`, and that is not a Risk because the `z` key is unchanged. The hide-pane follow-up is the verify-and-comment already tracked by #751, and the failed-key follow-up is a design choice for #748; each is 2-4 hours and neither includes a pin bump.

tmux-remux pin `d5afb67a81d8a30379e0d4186ec4b968244393bf` (lock node `tmux-upstream`, ≈ 2026-08-04): does not lag any Adopt or Risk row.

## Filed

None.

## Ignored

249 commits triaged, 247 ignored as unrelated.
