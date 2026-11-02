# Replace polling with tmux events where it removes work (#1000)

## Goal

Survey every polling site in tmux-og, replace the ones where a tmux event
removes real, recurring work, keep the rest with a recorded reason, and
measure before and after.

## What the pinned tmux offers (verified)

The root `tmux-upstream` input is lock node `tmux-upstream_2`, rev
`a94699c3` (`flake.nix` pins it; `flake.lock` also carries a stale
`tmux-upstream` node at `d5afb67a` that nothing uses). Checked in that
source and on a scratch server built from it (`tmux next-3.9`):

| Candidate | Present | Notes |
| --- | --- | --- |
| `pane-resized` hook/event | yes | `window_pane_resize` returns early when `sx == wp->sx && sy == wp->sy`, so it fires only on a real size change, never on the #793 same-size `refresh-client -C` storm. Payload: `pane`, `window`, `width`, `height`, `old_width`, `old_height`. |
| `pane-moved`, `window-zoomed`, `window-unzoomed`, `window-closed`, `client-closed` | yes | No polling site watches for any of these (below). |
| `wait-for -E name [-F fmt]` | yes | Blocks the calling client until event `name` fires with `fmt` true. Payload keys are bare format names (`#{pane}`, not `#{hook_pane}`). Flags must precede the name: `wait-for -E pane-resized -F …` is a parse error (`too many arguments`). Probed: the waiter is not an attached client (absent from `list-clients` and `#{C:}`), and killing the waiting client with TERM or KILL removes its sink (`wait-for -l -E` drops to 0) and a later event does not touch it. In a command list `display -p … ; wait-for -E …`, the `display` line reaches stdout before the wait blocks. |
| `set-hook -E @name` | yes | No consumer needs a user event. |
| `#{pane_last_output_time}` | yes | `time_t`, one-second resolution, set in `input.c` when the pane produces output. |
| `#{pane_output_generation}` | yes | Already used by `tmux-agent-repaint`. |
| `#{hook_fire_count}` / `#{hook_fire_time}` | yes, but only inside `show-hooks -F` | Not a general format. Useful for measuring how often a monitor fires, nothing else. |

## Polling sites and decisions

Fork counts are per occurrence on an idle server unless stated.

| # | Site | Cadence and cost today | Decision | Reason |
| --- | --- | --- | --- | --- |
| 1 | `agent-detect` geometry poll (`picker/agentdetect/main.go`, `geometryInterval`) | One `tmux display` fork per agent pane every 5 s, forever, whether or not anything resized. Resize desync bounded by 5 s. | **Replace** with a `pane-resized` wait (`wait-for -E`), fallback to the 5 s poll | The only per-agent-pane recurring fork. Scales with agent panes (12 on a halo-shaped host = 2.4 forks/s). The event fires only on a real resize, needs no client, and installs no hook (so nothing shadows a global hook, #647). |
| 2 | `tmux-agent-repaint` readiness wait (`#{pane_output_generation}` pair a second apart) | Two `display` forks per check, at least one check, for each newborn agent pane | **Replace** with one `#{pane_last_output_time}` read per check | Halves the forks per check and lets the wait end without a second sample. Once per newborn agent pane, so a small win; it is the site the issue named. |
| 3 | `-B` monitor ticks: `@og-sweep-tick`, `@og-pr-tick`, `@og-backfill-tick`, `@og-usage-tick`, `@og-res-tick` | Each fires every 5 s: one `run-shell` `sh` fork plus the script. Floors after that: sweep 1 `list-panes`; pr-enrich 1 `stat`; backfill 1 `mkdir` + 1 `stat`; usage 1 `stat` + 1 `list-panes` + a subshell per pane while no agent is open; res 2 tmux calls (`list-clients`, `show-hooks`) | **Keep** | They are the client-independent floor a control-only bridge host depends on (#603). Their "anything to do?" decisions read disk state (TTL stamps, cache dirs, `-d` checks) that a monitor format cannot see. The one fully format-expressible gate, res's "control client attached or usage armed", is static-true on every default config (`agentUsage.enable` defaults to `true`), so gating it saves nothing in practice. A per-second `#{S:#{W:#{P:#{pane_current_command}}}}` gate for usage would put a `/proc` read per pane per second on the server thread, which is the cost #793 removed. |
| 4 | `status-format[0]` `#()` jobs (`tmux-update-icons`, `claude-status`, `tmux-branch-display`, `tmux-dir-display`) | Every `status-interval` (1 s), per status-drawing client only | **Keep** | They produce the status text a client draws; their cadence is the redraw. Pushing the same data from hooks would need option writes on every event, and every option write runs `recalculate_sizes()` plus a redraw of every client (#1026). Never run on a bridge-only host, which needs nothing from them. |
| 5 | Bridge resize watcher (`watchLocalClient`, `daemon.go`) | One `os.Stat` per second of the hook-touched nudge file; one `display`/`list-clients` fork only when the mtime moves, plus one every 30 s as a coarse-mtime backstop | **Keep** | Already event-driven: the config's own hooks touch `@bridge_nudge` (#433, #820). Steady state is a stat, not a fork. `daemon.go` is also being edited for #1035. |
| 6 | Bridge reveal watcher (`watchReveal`, `reveal.go`) | One `list-clients` fork per daemon per second | **Keep** (follow-up candidate) | It also samples `#{client_activity}` every tick for controller ownership (`noteActivity`), and no event reports client activity, so the poll stays even if reveals moved to an event. A `wait-for -E session-window-changed` would cover reveals without a session hook. (Coordination note: #1035 is editing `daemon.go`'s teardown, where the watcher is wired.) |
| 7 | Bridge main-loop tick (5 s), park focus tick (1 s `os.Stat`), park re-probe ticker (`parkProbeEvery`), reopen-notice 15 s recheck (`showReopenNotice`), `holdPoll` | Backstops inside the daemon; stat, in-memory, or a network re-probe while parked | **Keep** | Backstops for missed control-stream events or a network reconnect probe; none waits on local tmux state an event could report. |
| 7a | Paste-progress spinner ticker (`pasteprogress.go`), picker preview throttle sleep (`picker/tui.go`) | Only while a paste or a picker is in flight | **Keep** | Animation and rate limiting for a transient UI, not polling. |
| 8 | `agent-detect` liveness probe (30 s `capture-pane`) | One fork per agent pane per 30 s | **Keep** | Backstop for a lost EOF: a dead server or a pane whose pipe never closed. No event reports "server gone". |
| 9 | `agent-detect` sample ticker | Runs only while the debouncer has a mark pending | **Keep** | Already idle-free. |
| 10 | Bounded waits: reflow's 0.12 s debounce and lock spin, `tmux-client-theme` lock/toggle wait, `tmux-status-fold` animation, `tmux-carousel-restore` restore retry, `og-remote-open`/`-detach`/`-loading` socket and spinner waits, picker attach waits | One-shot, capped at a few seconds | **Keep** | Not periodic. A `wait-for -L` channel lock would replace the lock spin but is never released if its holder is killed, which the stale-lock recovery exists for. The fold animation's sleep is the animation. |
| 11 | Window picker 1 s refresh | One `tmux` fork per second while the popup is open | **Keep** | Lives only while a human is looking at it (#874 already cut it to one fork). |

The task's other candidates (`pane-moved`, `window-zoomed`, `window-closed`,
`client-closed`, `set-hook -E`) have no polling site to replace: zoom and
layout consumers already ride `window-layout-changed`, and window/client
teardown is handled by the existing `window-unlinked`, `pane-exited` and
`client-detached` hooks.

## Design: site 1, agent-detect geometry

A per-watcher goroutine runs, in a loop, one tmux client per round:

```
tmux display -p -t %N '#{pane_width} #{pane_height}' \; \
     wait-for -E -F '#{==:#{pane},%N}' pane-resized
```

- The `display` line is the current geometry, read in the same command list
  that registers the wait. Commands in one list run back to back on the
  server thread, so no resize can land between the read and the wait: there
  is no gap to back-stop. A resize wakes the client, it exits 0, and the next
  round's `display` reads the post-resize size.
- Each geometry line goes to the watcher's main loop over a channel; the loop
  compares it with `curCols`/`curRows` and re-seeds on a change, exactly as
  the poll does today.
- A drag fires `pane-resized` once per step. The goroutine waits a settle
  delay of 1 s after a wake before its next round. Steps inside the delay are
  not lost: the next round's `display` reads the size they left. A round that
  sees a new size makes the main loop re-seed (`seededScreen`: a `display` and
  a `capture-pane`), so while a drag lasts each agent pane costs up to three
  forks a second (the round plus the re-seed), against 0.2 a second today;
  and a terminal resize resizes every window of the session, so every agent
  pane in it pays that at once. The idle cost drops from 0.2 a second to 0,
  and the post-resize desync drops from up to 5 s to about 1 s. The harness
  measures a drag scenario as well as idle, so the burst is a number, not a
  claim.
- Fallback: a round that exits non-zero without printing a geometry line
  means the command list was rejected at parse time — a resident server that
  predates `wait-for -E` (#407) — or a transient failure (the server
  restarting, a refused connection). That is the version gate: it probes the
  live server's own parser, never `tmux -V`. After three consecutive such
  rounds (settle delay between them) the watcher falls back to the 5 s
  `display` poll for the rest of its life, i.e. today's behaviour; a single
  transient failure does not pin it there. A round that printed a line and
  then failed (pane gone, client killed) resets that count and retries after
  the settle delay; the watcher itself exits on stdin EOF as today.
- The 5 s ticker stays. In event mode it runs the `stillOwner` file check
  (no fork; ownership is checked there while idle today) and, every 12th
  tick (60 s), the old `display` comparison: insurance against a waiter that
  blocks but never matches on some build, which the parse-rejection fallback
  cannot detect. Idle cost: one fork per agent pane per minute instead of
  twelve.
- The child is started with a context cancelled when the watcher returns, so
  a superseded or EOF'd watcher kills its waiter; killing the waiting client
  removes its sink cleanly (probed). Residual: a watcher killed with SIGKILL
  leaves its waiter blocked until that pane next resizes or the server exits
  (`cmd_wait_for_flush` wakes waiters on shutdown).
- Residual on older resident servers: tmux-og pinned upstream builds between
  d29aa121 (`wait-for -E` added) and 0e1f6ed8 (`cmd_wait_for_client_lost`,
  in the current pin). A resident server from that range keeps a killed
  waiter's wait item and client struct until that pane next resizes or the
  server restarts. The parser cannot tell those builds apart (all report
  `next-3.x`), so this is documented, not gated.
- The trade: each agent pane gains one resident blocked `tmux` client process
  and a server-side client struct and socket. It costs no forks or ticks; its
  RSS is measured and recorded with the fork numbers.

Client-less servers: `pane-resized` fires on `resize-window`/`resize-pane`
with no client attached, and the waiter is a command client, not an attached
one. A Go test on a scratch server with no clients pins that a resize reaches
the watcher's channel well inside the old 5 s bound.

## Design: site 2, agent-repaint readiness

Replace the `pane_output_generation` pair with one read per check:

```
last=$(tmux display -p -t "$pane" '#{pane_last_output_time}')
(( EPOCHSECONDS - last >= 2 ))  → quiet
```

- `pane_last_output_time` has one-second resolution, so `>= 2` guarantees at
  least one full quiet second (today's bar). Not quiet → sleep until the
  earliest second that could satisfy it, then read again.
- The 3 s startup sleep and the 12 s cap stay.
- An empty read (resident server predating the format) counts as quiet, as an
  empty generation does today.
- An empty read also means a pane that never produced output
  (`format_cb_pane_last_output_time` returns NULL when the time is 0); that
  pane is quiet by definition.
- Client-less: the script is started by the `@og-sweep-tick` claim, which is
  client-independent; a bats test runs it on a scratch server with no client
  and asserts the resize pair happens once output goes quiet. The test first
  asserts the pane's `pane_last_output_time` reads non-empty, so it cannot
  pass on the empty-means-quiet path without exercising the new read.

## Measurement

- Fork counts: a new `tests/perf/agent-detect-forks.sh` builds a scratch
  server with N panes running a manifest-known agent command, arms
  `agent-detect` on each via `pipe-pane`, idles for a fixed window, and counts
  command-client connections in the server's `-v` log; then a resize series
  and a drag (window resized at 10 Hz for 3 s). Run once with `main`'s
  `agent-detect` and once with this branch's. Waiter RSS is read from
  `/proc` for the record.
- Latency: `tests/perf/keystroke-latency.sh quiet busy`, two rounds per
  build, before (`main`) and after. Expected neutral: the harness's synthetic
  agent windows are not manifest agents, so it never arms `agent-detect`;
  that is recorded rather than assumed.
- agent-repaint: execs per nudge counted with the bats test's command log.

## Out of scope

- Any change to the bridge daemon: sites 5–7 are kept, and #1035 is editing
  `relayenv.go` and `daemon.go`'s teardown.
- The `-B` tick scripts' internals.
- `flake.lock`'s stale `tmux-upstream` node.

## Docs

`docs/agents/performance.md` gains a section with the survey summary and the
numbers; `docs/agents/agent-state.md` (or the scripts row) records the
geometry event; `scripts.md`'s `tmux-agent-repaint` row records the new
readiness read.
