#!/usr/bin/env bash
# Invoked by the pane-exited/pane-died hooks with #{q:hook_pane}.
set -euo pipefail

# Children first: this must run for every hook pane, lib or no lib.
PANE_CHAIN="${OG_PANE_CHAIN_BIN:-@pane_chain@}"
[[ -x $PANE_CHAIN ]] && "$PANE_CHAIN" close-children "${1:-}" || true

# Guarded so the RAW script still runs under bats, where @lib_claude@ is not
# substituted: nothing to reap without the lib, so exit rather than crash on an
# undefined function. In the built script the placeholder is always substituted
# (mkScriptWithLibs), so a missing file there fails loudly at source time.
# shellcheck source=/dev/null
if [[ -f "@lib_claude@" ]]; then
	source "@lib_claude@"
else
	exit 0
fi

claude_reap_pane "${1:-}"
