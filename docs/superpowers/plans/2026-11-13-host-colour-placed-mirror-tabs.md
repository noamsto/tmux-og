# Colour placed mirror tabs per host (#1102)

## Design choice

Colour is computed **by one Go owner, `picker/hostcolor`**, and resolved per
pass by `tmux-reflow-windows` via one `tmux-picker-generate host-colors` fork
(only when the session holds a placed window; never per window). Reflow stamps
the result as tmux format tokens, so rendering stays theme-correct (`@thm_*`
resolved at draw time, light and dark):

- per placed window: `@window_host_color` = `#{@thm_teal}` (same per-window
  `set -w` exec that stamps `@window_host_disp`: no new fork);
- per session: `@session_host_marker` = pre-styled `⇄ h1 · h2` fragment
  (sorted distinct hosts, each `#[fg=#{@thm_x}]`), unset when none.

Rejected: daemon-stamped colour (touches every create/rebuild/restamp path and
goes stale when `@remote_bridge_hosts` changes until a restamp); a bash copy of
the slot algorithm (violates req 5). A build-time global map cannot cover hosts
outside `@remote_bridge_hosts` (picker falls back to plain hash for those).

Dedicated mirror session: `tmux-statusline` colours its existing host marker in
Go from the same package (`@remote_bridge_hosts` + palette hex flags).

## Steps

1. `picker/hostcolor`: `Palette` names (peach teal pink yellow sky), `ParseHosts`,
   `Name(hosts []string, host string) string` (fnv32a slot + linear probe over
   sorted hosts; unknown → plain hash). Move the algorithm out of `hostColorFunc`;
   `hostColorFunc` maps name→`THM_*` ansi via `hostPalette` (order pinned equal
   to `hostcolor.Palette` by test). `parseRemoteHosts` delegates to `ParseHosts`.
2. `tmux-picker-generate host-colors <@remote_bridge_hosts value> <host>...`
   (dispatched before `applyConfig`) prints `host name` lines.
3. `tmux-reflow-windows`: read `#{@remote_bridge_hosts}` in the existing fast-path
   `display-message`; remember each placed window's host; after the window loop,
   if any, one `host-colors` call; stamp `@window_host_color`; build/stamp (or
   `set -u`) `@session_host_marker` (hosts failing `^[A-Za-z0-9._-]+$` are
   left out of the marker: it is format-expanded). ENTRY: idx and host badge use
   `#{E:@window_host_color}` (fallback `@thm_peach`); width unchanged.
   `FOLD_FMT` appends the marker after the session name.
4. `config/tmux.conf.tmpl` + `.reference.nix` (byte-identical): single-line
   `status-format[1]` uses the same per-window colour; `status-format[0]` passes
   `--thm-teal/pink/yellow/sky`. `host-badge-conf-assertions` updated.
5. `tmux-statusline`: new volatile fields `#{E:@session_host_marker}` and
   `#{@remote_bridge_hosts}`; render marker right after the session pill; a
   placed active window then skips its own host segment (marker covers it);
   dedicated host marker coloured via `hostcolor.Name`.
6. Tests: Go (`hostcolor`; picker test that status-bar `Name` equals
   `hostColorFunc` for a set incl. colliding hosts; `host-colors` cmd;
   statusline render); bats on a scratch tmux server (two placed windows, two
   hosts → tab colours + marker; none → no marker; fold carries marker).
7. Docs: `status-bar.md`, `bridge-daemon.md` (`@bridge_host` entry),
   `performance.md` reflow row (measured fork cost), `scripts.md` if rows change.

## Risks to verify before relying on them

- `#{E:@opt}` expands a stored `#{@thm_x}` inside conditionals/`#[...]` in
  `status-format` and in `display-message -p` (statusline field), `#[` retained.
- Fork cost of `tmux-picker-generate host-colors` (measure; req 6).

## Revision 1 (plan-critic findings, verified)

- Binary reference: reflow calls `@picker_generate@` (already in `iconSubstFrom`
  for `tmux-reflow-windows`). Failure of the call (empty/non-zero output, or a
  placeholder left unresolved in a suite lacking the binary) is non-fatal: no
  `@window_host_color` is stamped (renders peach, today's look) and
  `@session_host_marker` is stamped uncoloured (`@thm_subtext_0`) so existing
  placed-window tests in `tests/reflow-fanout.bats` keep passing.
- Flake wiring: the new bats file gets its own check derivation (like
  `status-fold-tests`) with `nativeBuildInputs` + the plain picker package
  (not `pickerChecked`); `setup` seds `@picker_generate@` to its
  `tmux-picker-generate`. The existing reflow suites need no binary.
- Bats cannot see line 0 on a config-less server: it asserts the stamped
  `@session_host_marker`, `display-message -p '#{E:@session_host_marker}'`
  expanding to hex with `#[` retained, per-window `@window_host_color`, and the
  rendered `status-format[1]` via `display-message`/expansion; the real
  `tmux-statusline` render is covered by a Go test.
- Flags `--thm-teal/pink/yellow/sky` land in the same commit as their
  definition in statusline (Go `flag` exits on unknown flags).
- New volatile fields are appended after `@og_dispatchers`; `main_test.go`'s
  `len(volatileFields)` pin and `fetchVolatile` indices are updated.
- Marker option renamed `@session_host_marker` (reflow-owned; `@bridge_*` stays
  daemon-owned).
- Staleness: colours/marker recompute on a full reflow pass only; a changed
  `@remote_bridge_hosts` shows after the next structural event or forced reflow
  (the home-manager activation already reflows every session; verified/doc'd).
- `host-colors` takes the raw option value as argv and honours no env override;
  the Go equivalence test feeds `hostColorFunc` the same value via its opts map.
- Reflow: `@remote_bridge_hosts` is read in the fast-path `display-message`
  before `@bridge_nudge` (nudge stays last); the `host-colors` call sits between
  the collect loop and the stamp loop; `@window_host_color` joins the existing
  per-window `set -w` argv. `host-badge-conf-assertions` greps the exact new
  string; tmpl and reference.nix stay byte-identical.

## Revision 2 (plan-critic re-review, verified)

- Existing tests asserting the old statusline output are rewritten with Step 5:
  `tests/remote-m2-integration.bats` "into: the statusline names the remote host
  on a placed window only" (~7859; helper `into_statusline` ~7752 and the
  dedicated call ~4031 gain `--thm-teal/pink/yellow/sky`), and
  `picker/statusline/main_test.go` bridge fixtures (~153-187, ~437-495: pass the
  full palette, expect `hostcolor.Name`'s slot). New contract: on every window of
  a session with placed mirrors line 0 shows the marker after the session pill;
  the placed window prints no separate host segment; no placed windows → no
  marker. Grep tests/ and picker/statusline for `thm-peach|thmPeach|fab387`.
- IDX colouring is gated on `#{@window_host_color}` (non-placed tabs keep BASE,
  no peach fallback for them); the stamp loop writes an empty `@window_host_color`
  on non-placed windows so a window that loses `@bridge_placed` is cleared.
- New bats setup exports `XDG_DATA_HOME` (wrapped-tmux-suite-isolation) and sets
  all five `@thm_*` palette tokens.
- `@session_host_marker` is written by a direct argv `tmux set` / `set -u`, not
  through the `tmux source -` batch.
