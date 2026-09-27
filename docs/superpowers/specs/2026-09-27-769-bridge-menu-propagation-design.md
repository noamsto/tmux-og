# Bridge: tmux menu actions on a mirror window act on the remote (#769)

## Problem

A `@bridge_win` mirror window is daemon-owned: its panes run the local renderer,
and every structural change must happen on the remote first and reach the mirror
through the daemon's reconcile. The keybinds already honour this — `prefix + &`,
`x`, `,`, `|`, `_`, `c`, `z`, `{`, `}` are `if-shell -F '<gate>' { run-shell
"<ctl> <verb> #{q:@bridge_pane}" } { <local> }` (`config/tmux.conf.tmpl`), and the
daemon translates each verb through the fixed table in
`picker/remotebridge/daemon/ctl.go`.

tmux's **menus** bypass that. They are tmux's own default bindings (the config
defines none) and run `kill-window`, `kill-pane`, `rename-window`, `split-window`,
`new-window`, `swap-pane`, `resize-pane -Z`, `respawn-*`, `select-pane -m`,
`break-pane`/`join-pane`, `rename-session`, `detach-client` directly on the local
mirror. "Kill" from a menu kills the local mirror window only; the remote window
survives and the daemon fights the loss.

## Inventory (pinned tmux `next-3.9`, measured with `list-keys` on a scratch server)

The config defines no `display-menu` of its own; every menu below is tmux's
default binding, byte-identical between `-f /dev/null` and the built config.

| Binding | Menu | Reachable here? |
|---|---|---|
| `prefix <`, `MouseDown3Status`, `M-MouseDown3Status` | window menu | yes |
| `prefix >`, `MouseDown3Pane` (else-branch of its `mouse_any_flag` test), `M-MouseDown3Pane` | pane menu | yes |
| `MouseDown3StatusLeft`, `M-MouseDown3StatusLeft` | session menu (`run-shell -C "display-menu …"`) | yes |
| `MouseDown3Empty`, `M-MouseDown3Empty` | "empty space" menu (New Pane / New Window) | yes (empty window area) |
| `MouseDown1Control9` | "Kill pane?" confirm menu | **no** — fires only on a `range=control\|9` in `pane-border-format`; this config's format (`tmux-apply-theme-colors.sh`, `config/tmux.conf.tmpl:544`) carries no control range |
| `move` table `,` / `.` | float Move / Move & Resize menus | **no** — nothing binds `switch-client -T move` (default or config) |

Measured quirk: on next-3.9 a right-click on a **non-current** window tab opens
no menu at all, on a stock `-f /dev/null` server too — only the current window's
tab does. The design below still evaluates the gate against the event's `-t =`
target, so it stays right if upstream changes that.

## Classification

Rule: an item that changes a **remote object** (window/pane lifecycle, name,
layout) goes through the existing ctl verb when one exists; an item that only
touches local state (copy mode, paste buffers, other local sessions, local
presentation order) stays as is; an item that would change the mirror's
structure locally and has no faithful ctl verb is **hidden** on a mirror.

### Window menu (`prefix <`, `MouseDown3Status`, `M-MouseDown3Status`)

| Item | Stock command | On a mirror |
|---|---|---|
| Swap Left / Swap Right | `swap-window -t :-1` / `:+1` | **local** — reorders local tabs only; the daemon addresses mirrors by id and never reads the local index (`bridge-daemon.md`, "addressed by tmux window ID") |
| Swap Marked | `swap-window` | **hidden** — the marked pane can sit in another session, and swapping a mirror window across sessions breaks the one-local-session ↔ one-remote-session invariant |
| Kill | `kill-window` | **ctl `kill-window`** (no confirm, like the stock item; the `&` keybind's confirm stays the keybind's) |
| Respawn | `respawn-window -k` | **hidden** — locally it destroys every renderer but the first (`spawn.c` respawn path), a desync the daemon does not detect; a remote respawn needs a re-seed the ctl path cannot request today (follow-up issue) |
| Mark / Unmark | `select-pane -m` | **hidden** — only feeds Swap Marked / `join-pane`, both structural |
| Rename | `command-prompt -F -I "#W" { rename-window … }` | **ctl `rename`**, the exact prompt the `,` keybind uses (prefill `@window_bridge_name`, name passed as `%1` → `#{qs:1}`) |
| New After / New At End | `new-window -a` / `new-window` | **ctl `new-window`**, one item "New Window" (`w`) — the daemon appends every mirror at `{end}`, so "after" cannot be honoured locally |

### Pane menu (`prefix >`, `MouseDown3Pane`, `M-MouseDown3Pane`)

| Item | Stock command | On a mirror |
|---|---|---|
| Go To Top/Bottom, Line Numbers, Refresh, Search For / Copy word, line, hyperlink | copy-mode `send-keys -X …`, `set-buffer` | **local** — the renderer pane's own copy mode |
| Paste, Type word / hyperlink | `paste-buffer`, `send-keys -l` | **local** — pane input, which the renderer already forwards to the remote |
| Move, Move & Resize, Tile, Float | `move-pane -P`, `resize-pane -x/-y`, `join-pane`, `break-pane -W` | **hidden** — float geometry and tiling of a mirror are reconciled from the remote; no ctl verb |
| Horizontal Split / Vertical Split | `split-window -h` / `-v` | **ctl `split-h` / `split-v`** (keybind parity: the verb carries the remote cwd) |
| Swap Up / Swap Down | `swap-pane -U` / `-D` | **ctl `swap U` / `swap D`** |
| Swap Marked | `swap-pane` | **hidden** (as the window item) |
| Kill | `kill-pane` | **ctl `kill-pane`** (no confirm, stock parity; the `x` keybind keeps its remote-worded confirm) |
| Respawn | `respawn-pane -k` | **local, relabelled "Reconnect" (`R`)** — locally it redials the renderer (#547: argv carries the sock and remote pane id, the daemon re-adopts and re-seeds), the one UI gesture that un-wedges a stuck renderer. The stock label reads as "restart the program", which it is not on a mirror, hence the rename; a remote respawn is the follow-up issue |
| Mark / Unmark | `select-pane -m` | **hidden** |
| Zoom / Unzoom | `resize-pane -Z` | **ctl `zoom`** (a local zoom leaves the remote pane at its old size — `bridge-daemon.md`) |

Each kept item keeps its stock visibility condition (e.g. splits only on a tiled
pane, Swap Up/Down only with >1 pane).

`MouseDown3Pane` keeps its stock guard on a mirror too: when the pane's program
asked for the mouse (`mouse_any_flag`), the click goes to it (`send-keys -M`),
exactly as stock; otherwise the mirror pane menu opens.

### Session menu (`MouseDown3StatusLeft`, `M-MouseDown3StatusLeft`)

| Item | Stock command | On a mirror session |
|---|---|---|
| Switch To `<session>` (≤6) | `switch-client -t=<id>` | **local** |
| Renumber | `move-window -r` | **local** — presentation only (`renumber-windows` is already on) |
| Rename | `command-prompt … rename-session` | **hidden** — a renamed mirror session reads as gone and tears the mirror down (#680) |
| Detach | `detach-client` | **`og-remote-detach`**, as `prefix + d` routes it in a mirror |
| New Session | `new-session` | **local** |
| New Window | `new-window` | **ctl `new-window`** — a local `new-window` would plant a non-mirror window in the mirror session |

### Empty-space menu (`MouseDown3Empty`, `M-MouseDown3Empty`)

| Item | Stock command | On a mirror |
|---|---|---|
| New Pane | `new-pane ; join-pane` | **hidden** — a local pane inside a mirror window |
| New Window | `new-window` | **ctl `new-window`** |

**No new ctl verb is needed**: every propagating item maps to a verb the
keybinds already use (`kill-window`, `kill-pane`, `rename`, `split-h`,
`split-v`, `swap`, `zoom`, `new-window`), so `ctl.go` does not change.

## Design

Two shapes were weighed:

- **A — whole-binding gate (chosen).** Each of the ten reachable bindings is
  re-bound to `if-shell -F [-t =] '<gate>' { <mirror command> } "<stock
  command, verbatim>"`. The non-mirror branch is tmux's own default text, so
  "behaves exactly as before" is a byte comparison, not an item-by-item argument.
- **B — per-item gate inside one menu.** Every item's command becomes
  `if-shell -F '<gate>' {…} {stock}` and hidden items get `#{?…}` labels.
  Rejected: every stock item is rewritten, so non-mirror parity can only be
  argued item by item, and the menu text grows for every window, mirror or not.

Both copy tmux's default menu text; A copies it verbatim and can prove it.

### Expansion layers (why every mirror command is `#`-escaped)

A menu item's command is format-expanded **when the menu is built**
(`menu.c:143-151`, `format_single_from_state` against the menu's `-t` target),
then that result is **parsed again** when the item is chosen (`menu.c:553-555`,
with the menu target as current state). A `run-shell` inside it expands its own
argument a third time at run. So a format written plainly in an item command is
resolved at build time and its value is spliced into command text that tmux
parses again — `#{qs:1}` resolves to an empty word before the prompt ever runs,
and a remote window name in `-I` would be parsed as tmux syntax.

Rule: **every format in a mirror item command that is meant for run time is
escaped once per layer above it**:

| Menu | Layers above the item's own `run-shell`/`command-prompt` | Written as |
|---|---|---|
| window, pane, empty-space menus | display-menu build | `##{…}` |
| session menu (inside `run-shell -C "display-menu …"`) | `run-shell -C` expansion, then display-menu build | `####{…}` |

This covers the whole ctl invocation (`bridgeCtl`'s `#{q:client_name}` and
`#{q:@bridge_sock}`, the verb's `#{q:@bridge_pane}`), the rename prompt's
`-I '#{@window_bridge_name}'` and `#{qs:1}`, and `og-remote-detach`'s
`#{qs:session_name}`. After build expansion the item command is byte-identical
to the keybind's command, so it inherits the keybind's measured quoting
(`rename-bind-integration.bats`: a `#()` in the remote name never executes, the
name round-trips verbatim). The generator applies the escaping with one helper
per layer, never by hand. Items kept from stock (copy mode, paste, labels) keep
stock text and stock build-time expansion (`#{q:mouse_word}` is only knowable
then), exactly as they behave today.

`command-prompt` waits by default, so its callback runs with the menu item's
state (`cmd-command-prompt.c`: `cmdq_get_command(cmdlist, cmdq_get_state(item))`)
and the item's run-time formats resolve against the menu's target.

### Old resident servers (#407)

A config is parsed in full before any of it runs, and a `{ … }` block is built —
every command looked up and its flags parsed — at that point
(`cmd-parse.y:846`). The stock menus carry next-only commands (`new-pane`,
`move-pane -P`, `break-pane -W`), so an older resident server sourcing this
config on `prefix r` or activation reload would reject the whole file. Two
layers stop that:

- **`%if "#{==:#{version},<stock version>}"` … `%endif`** around the whole menu
  block. `%if` drops a false block's commands before they are built
  (`cmd-parse.y:317-324`), so a server of any other version never sees the text
  and **keeps its own stock menus unchanged** — mirror menus just stay
  unpropagated there until that server restarts on the new binary. The stock
  version (`next-3.9`) is a constant beside the stock text it describes.
- **The stock branch is a string, not a brace block.** `if-shell`'s string
  branches are parsed only when they run, so a same-version server built from an
  older commit that lacks one of those commands fails that one menu when it is
  opened, never the config load. The mirror branch stays a brace block: it uses
  only long-standing commands (`display-menu`, `run-shell`, `command-prompt`,
  `send-keys`, `paste-buffer`, `set-buffer`, `copy-mode`, `if-shell`,
  `select-pane`, `respawn-pane`, `switch-client`, `move-window`, `new-session`).

### Details

- **One gate, one ctl entry point.** The gate is the existing `bridgeGate`
  (`#{&&:#{@bridge_win},#{@bridge_pane}}`) and every propagating item is
  `run-shell "<bridgeCtl> <verb> …"` — the keybinds' own strings
  (`generator/render/keys.go`), escaped per layer. No second forwarding path.
- **Target.** Mouse bindings gate with `if-shell -F -t =` (the event target:
  the clicked window's active pane, the clicked pane, the clicked session's
  current pane); `prefix <`/`>` gate on the current pane like every bridged
  keybind. Menus are opened with the stock `-t`/`-x`/`-y` flags, so build and
  run-time expansion use the same target the gate did.
- **Session menu.** The mirror variant keeps the stock `run-shell -C
  "display-menu …"` shape (the Switch-To loop needs the `#{S:}` expansion).
- **Stock text as data.** `generator/render/stockmenus.txt` (embedded with
  `go:embed`) holds the pinned tmux's own `list-keys` lines for the ten
  bindings, verbatim. `generator/render/menus.go` strips each line's `bind-key -T
  <table> <key>` prefix, double-quotes the rest for the string branch (escaping
  `\`, `"`, `$` — tmux's own `args_escape` set for a double-quoted word), and
  pairs it with the curated mirror command. One item list per menu is shared by
  that menu's keyboard and mouse bindings. The template renders the block through
  one `{{.MenuBinds}}` field; `config/tmux.conf.reference.nix` carries the same
  literal text (the two-file rule in its header).
- **Upstream drift.** The new flake check diffs `stockmenus.txt` against the raw
  pinned tmux's `-f /dev/null` `list-keys` for those ten keys, and its
  `#{version}` against the stock-version constant. A `flake.lock` bump that
  changes a default menu fails with the diff — on purpose: a new upstream item
  must be classified for mirrors before it ships.
- **`conf-shell-quoting.bats`** treats `run-shell -C`'s argument as a shell
  string today. With `-C` it is a tmux command, so the scanner recurses into it
  as a command line instead (still scanning any `run-shell` nested in it).
  Likewise an `if-shell -F` string branch is a tmux command and is recursed into.

## Invariants

- On a non-mirror window/pane/session every overridden binding runs tmux's stock
  command unchanged, and on a server whose `#{version}` differs from the stock
  version no binding is overridden at all.
- On a mirror, no menu item runs a local structural command (`kill-window`,
  `kill-pane`, `split-window`, `new-window`, `new-pane`, `rename-window`,
  `rename-session`, `swap-pane`, `resize-pane -Z`, `respawn-window`,
  `select-pane -m`, `break-pane`, `join-pane`, `move-pane`, `swap-window`
  without a relative target, `detach-client`). The one local `respawn-pane -k`
  is the relabelled Reconnect.
- The commands the mirror variant **adds** never splice a remote-derived value
  into command text: after menu build expansion each is byte-identical to the
  corresponding keybind command, and a value reaches a shell only through
  `#{q:}`/`#{qs:}`. Items kept from stock keep stock's build-time expansion
  (`Search For` expands `#{q:mouse_word}` at build, on a mirror as on any pane).
- `prefix <` and `prefix >` keep their stock notes (`Display window menu`,
  `Display pane menu`), so `prefix ?` and which-key read as before.

## Acceptance evidence

- Behavioural bats (`tests/menu-bind-integration.bats`, the attached-client +
  recording-stub harness of `rename-bind-integration.bats`):
  - on a mirror, `prefix <` → Kill / Rename / New Window and `prefix >` → Kill /
    Horizontal and Vertical Split / Swap Up and Down / Zoom each emit exactly the
    expected ctl argv, and the local window and pane counts do not change;
  - a right-click pane menu (injected SGR mouse event) → Kill emits `kill-pane`;
    a right-click on the current window tab → Kill emits `kill-window`;
  - rename from the menu with a remote name containing `'`, `;`, `#`, `}`, a
    space and a `#(touch …)`: the prompt shows it, the wire argv carries it
    verbatim, and the sentinel is never created;
  - the hidden items (Respawn on the window menu, Mark, Swap Marked, Float/Tile,
    Move, New Pane, session Rename) are absent from the drawn mirror menus, and
    Reconnect is present on the pane menu;
  - on a plain window `prefix <` → Kill kills the local window and sends no
    frame, and the pane menu shows the stock items;
  - the `%if` block sourced into a raw `-f /dev/null` pinned server overrides
    the ten bindings; the same block with the version literal changed leaves
    `list-keys` identical to the raw defaults.
- Stock-parity tripwire as above, in the same check.
- Go unit tests (`generator/render/menus_test.go`): the rendered block sits
  inside the `%if`; each stock branch unquotes to its `stockmenus.txt` line; no
  mirror branch contains a denylisted local structural command; every
  propagating item is `run-shell "<escaped bridgeCtl> <verb> …"` with a verb the
  daemon's table has; the escaping helpers round-trip.
- End-to-end scratch repro for the PR body: the daemon's `--test-local` mode
  (`--src-socket`/`--dst-socket`) mirrors a scratch "remote" server into a
  scratch local server running the built config; menu Kill window / Kill pane /
  Rename are then observed on the SRC server. If that cannot be driven, the PR
  says so and rests on the bats frames plus the daemon's existing verb tests.
- The classification tables land in `docs/agents/bridge-daemon.md`; its #547
  Respawn sentence is updated (the pane menus offer it as Reconnect on a mirror;
  the window menu hides it).

## Out of scope

- A remote `respawn-pane`/`respawn-window` verb (needs a re-seed intent in the
  daemon) — follow-up issue.
- `MouseDown1Control7/8/9` and the `move` table: unreachable in this config.
- `pumpInput` / dead-pane handling (#748).
