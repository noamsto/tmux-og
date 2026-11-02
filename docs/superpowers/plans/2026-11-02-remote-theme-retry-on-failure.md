# Plan: remote theme fan-out retries until delivered, syncs on bridge connect (#1064)

## Design choice
Fan-out: option (a). Stamp `@og_theme_applied` only after the loop and only when every `ctl theme` exited 0; any failure leaves it unset so the next reload re-sends to all daemons (repeat apply is idempotent). Chosen over (b) because per-socket state needs a new option per socket plus cleanup of dead ones, while (a) is 5 lines and the verb is idempotent.
Stale `@bridge_sock`: ctl dialing a missing/refusing unix socket fails immediately (no timeout), so no wait — but while it exists the stamp stays unset, so every reload re-sends to ALL daemons (one remote `theme-toggle apply` each, idempotent); documented in scripts.md, with (b) as follow-up if it bites; the stamp stays unset while it exists (retry), bounded by the stale pane's lifetime. Documented. Script still always exits 0.
Daemon: on connect, after the existing theme probe, if remote has `theme-toggle`, read local `@catppuccin_flavor` via `cfg.LocalTmuxOut("show-options","-gv","@catppuccin_flavor")` (nil LocalTmuxOut or unreadable → no apply), map latte→light else dark; read remote flavor with `display-message -p -t <sess> '#{@catppuccin_flavor}'` (empty/Error → not a match → apply), map the same way and compare mapped themes (not raw flavors); skip when equal, else send `run-shell -b` with `themeApplyScript`. A bare `run-shell -b` resolves its output pane to the client's current pane (the mirrored pane, #871); safe only because the script discards output and exits 0 — the code comment says so. Residual: N daemons of one host connecting at once all read the old flavor and all apply; documented, not claimed fixed.

## Files
- `scripts/og-remote-theme.sh` — stamp after loop only on all-success.
- `tests/remote-theme.bats` — replace the "stamp advances before fan-out" test; add retry + no-resend tests.
- `picker/remotebridge/daemon/themesync.go` (new) — `syncThemeOnConnect`.
- `picker/remotebridge/daemon/daemon.go` — Run() keeps the probe's `available` and calls it beside the probe (~1119); `repair()` (~1795, next to the RelayEnvCmd re-send at ~1890) calls it too, since a reconnect never goes through Run().
- `picker/remotebridge/daemon/themesync_test.go` (new) — Go tests.
- `docs/agents/scripts.md`, `bridge-daemon.md`, `theme.md` — describe behaviour.

## Steps
- [ ] **Step 1: bats red tests** in `tests/remote-theme.bats`: (i) FAKE_CTL_RC=1 leaves stamp empty (also assert `[ ! -s "$STAMP" ]` in the existing 'one unreachable daemon' test) and a second run re-sends (ctl log 2 lines total per sock); (ii) after a successful run a second sends nothing (exists); delete the "stamp advances before" test. Run `nix develop -c bats tests/remote-theme.bats` — new tests fail.
- [ ] **Step 2: script fix**: track `failed=0`, set 1 on non-zero ctl exit; after loop `(( failed )) || tmux set-option -g @og_theme_applied "$theme"`; remove the early stamp; update header comment. Re-run bats: green.
- [ ] **Step 3: Go red tests** (add a stub `syncThemeOnConnect` first so failures are assertions, not compile errors; record the red output; also a test that repair() invokes the sync, via the existing reattach/repair test harness; mapped-theme compare row; remote read empty → apply) `themesync_test.go`: table over (toggle absent → no apply; remote flavor matches → no apply; differs → apply with `themeApplyScript` for the mapped theme; local flavor unreadable → no apply) using scriptedRT and a `LocalTmuxOut` stub. Run `cd picker && go test ./remotebridge/daemon -run ThemeSync` — fails to compile/red.
- [ ] **Step 4: implement** `syncThemeOnConnect(rt, cfg, available)`; apply via `rt("run-shell -b "+tmuxQuote("exec /bin/sh -c "+tmuxQuote(themeApplyScript(t))))` (no `-t`, no pane involved; output discarded, exit 0). Wire into daemon.go inside the probe block: `available` → sync; absent → existing notify. Run go test: green.
- [ ] **Step 5: docs** — three docs updated; the scripts.md row's "no per-host success/failure (#545)" sentence replaced by the new all-or-nothing rule and stale-socket cost.
- [ ] **Step 6: gate** — `nix build .#default`, `nix flake check`, `nix build .#lint --accept-flake-config`.

## Acceptance
- bats retry/no-resend tests → Step 1/2. Go tests (differs/matches/absent) → Step 3/4. Docs → Step 5. Three nix commands → Step 6.
