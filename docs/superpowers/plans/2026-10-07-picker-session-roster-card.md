# Picker session preview: window roster card (#942)

Goal: for a local session row in session mode, `loadPreviewCmd` renders a window
roster card (header, one aligned line per window, optional blocked-agent detail
line, rule, tail of the active pane) instead of a bare `capture-pane`.

## Files

- `picker/main.go`: extract the `windowsArgv` format into `const windowsFormat`
  (windowsArgv unchanged); add `paneID` to `agentPaneInfo`, set in
  `collectAgentPanesFrom`.
- `picker/session_card.go` (new): `sessionCardArgv`, `parseSessionCard`,
  `attachCardAgents`, `renderSessionCard` (pure), `cardAge`, `loadSessionCard`.
- `picker/tui.go`: `loadPreviewCmd` routes a local session row (session mode,
  not header, `item.session == item.target`) through the card under the same
  `previewGate` ticket; a failed card chain falls back to the plain capture.
- `picker/session_card_test.go` (new): renderer + argv tests.
- `docs/agents/picker.md`: describe the card.

## Steps

1. **Data, one tmux call**: `list-panes -s -t =<sess>: -f notModalFilter -F
   <windowsFormat>|#{window_activity}|#{pane_id}|#{session_path}` `;`
   `display-message -p <sep>` `;` `capture-pane -p -e -t selfCaptureTarget(t)`.
   Split on a line equal to the separator. The first 38 fields go to
   `windowsFromRows` (reuses the bridge substitution, the `currentBranch`
   in-process fallback); extras give activity, pane ids (a paneMap for
   `collectAgentPanesFrom`) and the session path.
2. **Agent state**: `collectAgentPanesFrom` over the trusted status dir with the
   paneMap built from step 1; `aggregateAgentByWindow` + `mergeAgentWindows`.
   Detail line: for a window whose priority state is waiting/denied/error, the
   `tasks/<pane_id>` self-report of the first matching pane, through
   `sanitizeStatusText`.
3. **Render** (pure): columns idx, active marker, name, icons (procs via
   `buildProcIcons`, agent via `appendAgentIcon`; mirror: `@bridge_proc` already
   in procs, no figures), branch (mirror: `bridgeName`), issue id + PR badge
   (`colorPRBadge`), right-aligned age. Widths via `iconCellWidth`/`visibleWidth`;
   flexible columns (name, branch) shrink to the width budget, every line final-
   clamped with `truncateVisibleWidth`. Capture tail: last N lines that fit the
   remaining height, each clamped; `stripStringEscapes` first.
4. **Wire**: preview content is the card with `scrollTop: true`.
5. **Tests**: alignment with wide glyphs, narrow-width clamp (no line wider
   than width), mirror row, blocked detail line, single window, hostile names
   (ESC/CSI/OSC in name and detail), argv has exactly one `list-panes`, one
   `capture-pane`, uses `=` target and `notModalFilter`.
6. **Docs**: `docs/agents/picker.md` section.

## Risks

- A tmux error in any chained command fails the whole list: fall back to the
  plain capture (today's behaviour), never an empty preview.
- Names/detail/capture are untrusted: sanitized before any render; nothing is
  placed in a tmux format.

## Revisions

- Plan-critic (revise): rows are exactly 40 fields; `session_path` is its own chained section; stable detail-pane choice (lowest pane id); width/height captured at cmd build.
- Review gate: per-call nonce separator, `=name:` targets, bounded task read.
