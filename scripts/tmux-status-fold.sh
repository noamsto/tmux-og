#!/usr/bin/env bash
# tmux-status-fold: hand the prefix + Z fold to tmux-reflow-windows.
# Bound to `prefix + Z`, which flips @og_status_folded in a single `set -F` and
# then runs this backgrounded with the session name and client width, the same
# arguments the reflow hooks pass. The flag is the intent; tmux-reflow-windows
# (--force) is the one place that knows what a folded or unfolded bar looks
# like, so fold and unfold are one instant switch. Rapid presses converge on the
# last flag: reflow reads the live flag under its own lock.

SESSION=${1:-}
WIDTH=${2:-}
REFLOW=${OG_FOLD_REFLOW:-@reflow@}

[[ -n $SESSION ]] || exit 0
# Scratch sessions run their own hints bar (see tmux-reflow-windows).
case "$SESSION" in
scratch-*) exit 0 ;;
esac

"$REFLOW" --force "$SESSION" "$WIDTH" >/dev/null 2>&1
