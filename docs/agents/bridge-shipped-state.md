# Bridge: State Shipped from the Remote

A mirror pane runs a renderer, so everything a local consumer reads about a bridged window — resources, agent status, labels — is shipped across as `@bridge_*` stamps.

## Remote Session Resources

The session picker's CPU/Mem columns on a mirror row measure the **remote**
session, not the local renderers its panes actually run. The aggregation now
runs where the process tree is and crosses as an ordinary bridge stamp (#693);
the ssh `ps` leg survives only as the version-skew fallback
(`picker/remote_resources.go`).

- **One walk, one implementation.** `picker/proctree` is the only place the
  process-tree walk lives — the picker's local leg, the ssh fallback and the
  remote's own poller all call it over one `ps`-shaped table. Extracting it is
  what let the remote be measured where it is without a second copy of the walk
  drifting from this one.
- **The same walk reports which agent runs in a session.** `PSArgs` carries a
  trailing `comm` (BSD ps prints a full path, so it is basenamed and
  `.foo-wrapped`-normalised), and `proctree.Aggregate` collects any agent
  manifest command found anywhere in a session's tree. `pane_current_command`
  names a pane's process-group leader, so an agent tmux-remux relaunched —
  `cat-scrollback …; <agent>; exec <shell>` under one non-interactive shell,
  which has no job control, so the agent shares that shell's group — is
  invisible there while its state file, and so its agent icon, is live. The
  tree-found name joins `sessionData.procs`, restoring the program icon in the
  Procs column; every source gets it from the one walk — a mirror's names ride
  the stamp below — and the command list is the manifests' own
  `match_commands`, the same list `@AGENT_COMMANDS` and agent-detect read.
- **`tmux-session-resources` is armed by the `@og-res-tick` monitor hook**
  (every 5s, `--tick`) on every tmux-og host, and a pass runs **only while the
  server has a control-mode client attached** — i.e. only while something is
  bridged to it, so an unbridged host pays nothing for a column nobody reads. It
  stamps every session's own `@og_session_res`, targeting it by id (`-t '$N'`,
  never the name: `set-option -t` takes a target-pane, and a name like `0` also
  resolves as a pane index in the current window) with
  `"<cpu> <mem> <cores> <tick> <agents>"`, where `agents` is the tree's agent
  commands comma-joined, or `-` for none. Cores is `runtime.NumCPU()`, which is
  what retires `getconf`. The values are quantised to a coarse fixed precision and
  never pre-rendered: `formatCPU`/`formatMem` stay the sole owners of display
  precision.
- **The poller's `ps` is pinned at link time, and differs by platform.** Linux
  links the store's procps; darwin links Apple's `/bin/ps`, because the store's
  adv_cmds `ps` refuses the `rss` keyword ("requires entitlement"), drops the
  column and exits 1 — which the poller reads as a failed pass and stamps
  nothing. The Nix darwin build sandbox in turn cannot exec `/bin/ps`, so the
  darwin arm of `tests/tick-floor.bats` points `OG_PS_BIN` at a stand-in over the
  store `ps` (constant rss); the linked-`/bin/ps` path itself is not exercised
  by any check.
- **The `tick` (the remote's epoch seconds) exists only so the value changes
  every pass.** A tmux option outlives the process that wrote it, so no amount
  of re-reading one can distinguish a live poller from a dead one — the absence
  of notifications is the only evidence there is, and a row that never changes
  emits none.
- **The daemon subscribes session-scoped**, the third subscription beside
  `og_labels` and `og_agents`: `refresh-client -B 'og_res::#{@og_session_res}'`,
  where the **empty `what` field is the session-scoped spelling**, reported back
  as `%subscription-changed og_res $N - - - : <value>`. It deduplicates on
  every field but the tick, so the per-pass tick costs no local write, and
  re-stamps an unchanged row only once its 30s refresh floor has elapsed. Three
  reports are dropped whole: one over 128 bytes (every carried value is
  length-capped), one naming a session other than the pinned one (the
  subscription follows the control client's CURRENT session, so a session-pin
  excursion reports someone else's), and one whose tick, corrected by the
  measured clock skew, is over 30s old — the subscription reports the option on
  every subscribe, and an option outlives the poller that wrote it.
- **A malformed `-B` spec is not reportable.**
  `cmd_refresh_client_update_subscription` silently removes the subscription and
  returns with no `%error`, and this shipper has no poll backstop to mask one —
  so its spec string is a constant with a live test.
- **`@bridge_res` carries the daemon's own local clock**, never the remote's
  tick: it writes the LOCAL mirror session's option as
  `"<cpu> <mem> <cores> <local-receive-epoch> <agents>"`, so the reader needs
  no skew correction; the picker merges `agents` into a covered mirror's Procs
  column the way the ssh leg merges its own tree walk's. `reattach` drops it at the moment it sets `@bridge_state
  disconnected`, and `repair` resets the shipper so the re-subscription's
  re-report re-stamps — that is how the freshness rule below gets its "the
  bridge is up" condition without the picker reading a second option.
- **Precedence is per session, not per host.** The picker reads `@bridge_res`
  out of the `list-panes -a` snapshot it already takes and treats it as fresh
  when it parses and its epoch is no more than 90s old (three of the daemon's
  refreshes) and no more than 2s in the future (past that it is a clock jump,
  not a measurement);
  a session with a fresh stamp keeps it, and the ssh path is probed only for
  hosts with at least one uncovered session, filling only those sessions. Two
  self-heals fall out of that epoch alone — a poller that stops while the bridge
  stays healthy, and a daemon killed without teardown leaving an orphaned mirror
  — both age out on the reader's own clock.
- **CPU is the raw per-core `ps` sum whatever the source, and the owning
  machine's core count rides beside it.** Normalising the remote by its own core
  count read as "% of that machine", but the local leg does no such division, so
  one column carried two units 32x apart — measured on a 32-core remote, every
  session rendered a permanent `<1%` (a whole core pegged reads 3%).
  `sessionData.cores` carries the owning machine's count (0 means this one) and
  `cpuColor` scales against *that*, where before it divided an already-divided
  remote value by the local `numCPU` again and pinned every mirror row to the
  grey tint. The column widens off the rendered strings, so `cpuColWidth` stays
  a floor. Unrelated and still true of every source: `ps` `%CPU` is a lifetime
  average, not a rate, so a long-lived pane's figure lags reality in either
  direction.
- **A session covered by neither source renders `-`, not its local figures**
  (`resUnknown`). Those figures measure the renderer, and a wrong number is
  worse than an absent one — an unreachable host shows `-` indefinitely, which
  is the truth.
- **The ssh `ps` leg is the version-skew fallback**, and its three workarounds
  are live constraints there and nowhere else now. The payload is one ssh
  round-trip per host — core count, then `<session>|<pane_pid>` lines, then a
  `PSTABLE` separator, then the whole process table. The separator **must start
  with a letter**: the remote's login shell is whatever the user set, and fish
  reads `echo --` as end-of-options and prints a blank line, which silently
  swallowed it and left every mirror at 0% / 0M. Core count comes from
  `getconf _NPROCESSORS_ONLN`, never `nproc` — coreutils-only, absent on macOS.
  The leg needs nothing new on the remote's PATH, `tmux` and `ps` only. Sunset
  condition: it is deletable once every host in `@remote_bridge_hosts` arms the
  poller — `tmux show -gv @og-res-tick` on that host's live server prints the
  `tmux-session-resources --tick` command. A host rebuilt from this revision
  whose resident server predates 3.8 (#407) arms nothing until it restarts.
- **That leg never blocks the render.** `remoteResourcesFor` returns what is
  cached and kicks a background refresh (`remoteResourceTTL`, 10s — an ssh
  round-trip where the local leg costs a fork). A host already in flight is
  skipped, not queued, so the 1s item rebuild cannot pile ssh processes behind a
  slow host, and a failed fetch keeps the previous values.
- Known limit, accepted: the poller can only ever be armed once the remote is
  rebuilt, so an older remote stamps nothing and degrades silently to the
  fallback.

## Remote Agent Status

A mirror window's local panes run renderers, so nothing writes
`/tmp/claude-status/panes/<local_pane_id>` and every local consumer would read a
bridged window as agent-free. The bridge ships the remote's state instead:

- **A control-mode client renders no status line**, so the remote's 1s `#()`
  pollers never run for a session whose only client is the bridge. The state
  therefore has to be pushed at write time — `claude-status-update`'s
  `bridge_stamp` — not derived by a reader on the remote.
- **The daemon subscribes rather than polls** (#566): `refresh-client -B
  '<name>:%*:<format>'` makes tmux report the format's value for each pane on
  `%subscription-changed` whenever it moves, so a stamp arrives as a stream line
  and the shipper applies the row it was handed without reading the remote at
  all. Available since tmux **3.2**, unused here until #566. Its own `list-panes`
  read survives as a coarse backstop through `rt` (the ordinal-matched
  round-tripper from #283), so that read still runs **on the main loop** and
  nowhere else — `rt` reads the stream, which has one consumer.
- It writes the files under **local** pane ids (`@bridge_pane` holds the
  reverse mapping), and stamps `timestamp` through a clock skew measured once at
  startup: ages drive the fade and the "last active" readout, and two hosts'
  clocks never agree.
- The same format carries each remote pane's `pane_current_command` into
  `@bridge_proc` on the mirror pane, which `tmux-update-icons` prefers over the
  local one — that runs the renderer, so a mirrored window would otherwise draw
  the fallback glyph. Panes with **no** agent stay in the reply for exactly this
  reason. Every consumer that names a mirror pane's command must prefer it:
  `tmux-update-icons` (window icons and `@active_pane_icon`), `usage.go`'s
  agent-pane liveness gate, and `statusline`'s top-right `<icon> <command>` unit
  — which read `pane_current_command` raw and so printed the renderer next to an
  icon already resolved from `@bridge_proc` (#590).
- An **unchanged row is not rewritten**, so the local mark-seen hook's clearing
  of `unseen` survives until the remote's agent genuinely writes again — and the
  `@bridge_proc` stamp isn't re-asserted, which would be a fork per pane per
  second.
- Teardown deletes what it wrote — `claude_prune_stale_state` collects by
  server-start mtime and would keep it until a tmux restart.
- **The dispatcher's pane decorations ride the same row** (#640).
  `decorate_pane` stamps `@crew_role`/`@crew_state`/`@crew_role_color` per pane
  and a `pane-border-format` that reads them, but a border is *chrome*: the
  LOCAL server draws it, from local options, so a mirrored role grid rendered
  every pane identically and there was no way to tell the reviewer from the
  plan-critic. The trio crosses as `@bridge_crew_*` and the global
  `pane-border-format`/`pane-border-style` read those — never the real
  `@crew_*` names, which are the dispatcher's own. They are stamped **before**
  the agent-less return: a parked role pane reports no agent state and still has
  to draw its border. Colour falls back through `@bridge_crew_color` (the
  window's agent tint, already carried for the label) to the theme, which is the
  remote's own precedence.
- **These are the first carried values a LOCAL format renders**, so they are the
  first that may not merely garble: `cleanLabelValue`'s contract stops at
  `#[…]`, leaving `#{…}` and `#(…)` intact, and `#(cmd)` on a border would run
  cmd here. `crewWordRe`/`crewColorRe` exclude `#` outright rather than relying
  on an escape pass a later consumer could forget — see that warning on
  `cleanLabelValue`.
- **Screen-scraped agents (pi, codex, cursor) cross the bridge too** (#635),
  on a second pane option: `agent-detect`'s `statefile.Writer` mirrors its
  verdict into `@agent_screen` (`"<state> <epoch> [name=count ...]"`) the same
  way `claude-status-update` mirrors into `@claude_status` — a NEW option
  rather than overloading `@claude_status`, so the laptop side keeps its
  hook-vs-screen precedence (`read_pane_state`, `collectAgentPanesFrom`) once
  both sources are on one host. `agentStatusFormat` carries `@agent_screen`
  alongside `@claude_status`, and the shipper writes it to
  `screen/<local_pane_id>` under the same clock skew, unchanged-row
  suppression, and teardown deletion the `panes/` file gets — independently,
  since a screen-only pane never gets a `panes/`/`tasks/`/`issues/` file and a
  Claude pane's hook state must not gate a screen verdict or vice versa. The
  #603 sweep hook is what makes this reachable at all: it arms `agent-detect`
  on a host whose only clients are bridges, which is why the note this
  replaces once called screen-scraper state uncarriable.
- Not carried: `interrupted` (derived on the remote from a transcript tail
  this side can't read).

## Remote Window Labels

The same problem one level up: a mirror window's own `@crew_*`, `@issue_*`,
`@pr_*` and `@window_label_*` were stamped by the `after-new-window` hook
against the *launcher's* cwd, so they describe the wrong repo entirely. Reflow
used to blank them and render the bare remote window name (#462). The daemon now
ships the remote window's own label state across instead.

- **The daemon writes `@bridge_*`, never the real option names.**
  `tmux-reflow-windows` stamps `@window_label_*` on every window of the mirror
  session, mirrors included, so a same-name write is a two-writer race the daemon
  loses on every reflow pass.
- **Every render site reads `@bridge_*` directly** rather than reflow's stamped
  copies — the three (reflow's grid, `picker/main.go`, `picker/statusline`) stay
  symmetric and independent, and reflow only runs for a session with a client.
  The colour/state values (`@bridge_crew_color`, `@bridge_pr_number`,
  `@bridge_pr_state`, `@bridge_pr_check_state`, `@bridge_pr_mergeable`,
  `@bridge_pr_review`, `@bridge_pr_auto_merge`) are read *live* at render time
  through a `#{?#{@bridge_win},…}` conditional (`bopt` in
  `tmux-reflow-windows`), so they are never in reflow's `read -r` list.
- **Subscribed, not polled**, like `agentstatus.go` — a window *option* change
  emits no control-stream traffic **of its own**, which is why this shipper and
  its neighbour both used to poll and why `mainLoopTickInterval` was cut to a
  coarse 5s. `refresh-client -B '<name>:@*:<format>'` is the push channel
  (#566), and its `%subscription-changed` lines are ordinary stream traffic: the
  loop still `select`s over the tick, and every line taken off that channel must
  still `claimSeq`, or the ordinal count falls behind and no later round-trip
  recognises its own reply. The remaining `list-windows` read is a backstop,
  reached on its own long floor or the moment the registry generation moves.
- **What a subscription cannot report is a change in the mirror SET.** A window
  created after its value was last reported, and a `retireMirror` rebuild under
  the same remote id, both leave a mirror whose stamp is missing while the remote
  value is unchanged — so nothing fires. `registry.generation` counts both, and a
  shipper whose recorded generation is stale re-reads at once instead of waiting
  out its backstop. Dropping the poll without this is the one way to get a
  permanently bare mirror.
- **A bare mirror is a viewer-build check before it is a shipper bug.** The
  binary to read is the tmux-og wrapper: the file that contains
  `-f /nix/store/…-tmux.conf`. A houston-class host cannot be reached over SSH
  from the source, so the check runs on the viewer, in fish. Resolve `tmux` on
  PATH, extract the baked conf the way `tests/test-display.sh` does (`grep -o`
  of that `-f` argument, then `cut`), and require a non-empty `$conf` before
  `rg`. An empty extract is the failure: this tmux is not the tmux-og wrapper,
  and there is no conf to search.

  ```fish
  set tmux_bin (readlink -f (command -v tmux))
  set conf (grep -o -- '-f /nix/store/[a-z0-9]*-tmux[.]conf' $tmux_bin | head -1 | cut -d' ' -f2)
  if test -z "$conf"
      echo "this tmux is not the tmux-og wrapper: $tmux_bin"
  else
      echo $tmux_bin
      echo $conf
      rg -n '@bridge_crew_name|@bridge_crew_color' $conf
      rg -n -F '#{?#{&&:#{?#{@bridge_win},1,#{@window_has_agent}},#{?#{@bridge_win},#{@bridge_crew_name},#{@crew_name}}},' $conf
  end
  ```

  Empty baked conf means this tmux is not the tmux-og wrapper. A conf with no
  `@bridge_crew_name` means the viewer predates window-label shipping. A conf
  that has `@bridge_crew_name` but lacks the #671 fragment
  `#{?#{&&:#{?#{@bridge_win},1,#{@window_has_agent}},#{?#{@bridge_win},#{@bridge_crew_name},#{@crew_name}}},`
  means the viewer predates the bridge crew-badge bypass. Both present means
  the viewer has the render path.

- **Queued rows are applied by the loop, not by the dispatch that received
  them.** Re-subscribing under an existing name re-reports *every* object — which
  is how a reattach gets back to ground truth in one command — and the loop runs
  a pass per line, so applying eagerly would cost one local fork per shipper, and
  one forced reflow, per window and per pane of that snapshot. The shippers hold
  their rows while more lines are already buffered on the pump (`queuedApplyDue`),
  bounded so a stream that never goes quiet cannot hold them.
- **A notification is edge-detected at 1 Hz**, so two changes inside a second
  collapse to the last sampled value. No worse than the 1s pollers it replaces,
  but it is not an event log.
- **An unchanged row is not rewritten** (the neighbour's rule), but the cache is
  keyed on remote window id *and* the local window it landed on: `retireMirror`
  rebuilds a dead mirror under the same remote id against a fresh local window,
  and a row compare alone would suppress the re-stamp and leave the replacement
  bare. `agentShipper` needs no equivalent because it keys on the local pane id,
  which a rebuild changes anyway.
- **A label change alters no window count**, so reflow's `count:width:height`
  cache would skip it — the shipper forces `tmux-reflow-windows --force` once per
  changed pass, the `@window_bridge_name` precedent. It is the *only* trigger on
  a mirror: `tmux-update-icons`' `@crew_name`/`@crew_seen` comparison never fires
  there, since both stay empty.
- **A remote carrying no label state renders the remote window name**, exactly as
  before — reflow falls back only when the bridge carries neither id nor rest,
  and that fallback alone speaks `@window_bridge_name`'s doubled-`#` dialect.
- Every carried value is sanitized daemon-side (`|`, control bytes and `#[…]`
  markup dropped, enums and colours regex-matched, a leading `-` rejected whole
  since `LocalTmux` execs without a shell) and length-capped. Teardown unsets
  what it wrote.
- **Two cleaning policies, split on what a wrong value costs** (#598). Display
  fields (`@bridge_issue_title`, `@bridge_pr_title`, and the two label segments)
  **truncate** at their cap — a shortened title is still a title. Identity
  fields (`@bridge_issue_provider`, `_issue_id`, `_issue_url`, `_pr_url`,
  `_pr_draft`, `_pr_review`, `_pr_auto_merge`, `_pr_check_progress`, `_branch`,
  `_dir`) **drop whole** via `cleanLabelValueExact`,
  because a truncated URL opens the wrong page, a truncated branch refreshes the
  wrong branch, and a truncated path names a directory that is not the one on
  screen. That cleaner also rejects any value `stripWindowName` *altered*, not
  just an over-cap one: the stripper deletes rather than rejects, so
  `https://host/a#[b]c` would otherwise become `https://host/ac` — a different,
  still-valid-looking URL that passes the regex. Every consumer renders absent
  correctly, so absent beats plausibly-wrong. The drop set is therefore wider
  than "over cap": markup, control bytes and a leading `-` all drop too.
- **Free-form fields lose their pipes on the REMOTE**, wrapped
  `#{s/[|]/ /:@opt}` in the read format. The bracket expression is load-bearing
  — a bare `s/|/ /` is an ERE empty alternation — and it is what allows more
  than one free-form field in the row at all: sanitization runs after the split
  and cannot repair a shift. `@window_label_rest_long` is the one field left
  unwrapped, kept last so its own `|` lands inside it rather than shifting the
  row. `@bridge_dir` is `#{?@worktree,#{@worktree},#{@git_root}}` resolved
  remotely, so no consumer re-implements that fallback.
- Liveness: the codename and label track the remote live; `@pr_*` is only as
  fresh as the remote's own `tmux-pr-enrich` poll. That poll is **server-wide**
  (`list-windows -a`), so a remote with a real client attached to any session
  does refresh a bridged session's windows on its own schedule — but a remote
  whose only client is this bridge renders no status line and so never polls at
  all. Neither case gives an *on-demand* refresh, which is what the
  `enrich-refresh` ctl verb is for (#598).
- **The enrich card reads `@bridge_*` in a mirror, and only those** (#598).
  `picker/enrichcard`'s `resolve()` returns the bridge values with no fallback to
  `@issue_*`/`@pr_*`/`@branch`/`@worktree`/`@git_root` — those are the launcher's
  residue from the `after-new-window` hook and can describe an unrelated repo, so
  falling back to them is the bug, not a safety net. Same semantics as
  `bridgeOpt` (`config/tmux.conf.nix`). `detectBaseBranch` is skipped in a
  mirror: it shells `git -C <dir>` against a path on the *remote*, and the same
  path can exist locally as a different repo. `prefix + i` stays a plain local
  `floatBind` — the card has nothing to run remotely, and `[o]`/`[p]`'s
  `xdg-open` on a headless host would break URL opening — but it is passed
  `--bridge-sock`/`--bridge-pane`/`--bridge-ctl-bin` so `[r]` can route to the
  remote. `[r]` in a mirror sends the `enrich-refresh` verb synchronously and
  flashes the ctl's real outcome; a mirror with no ctl handle reads
  `[r] no bridge`.

