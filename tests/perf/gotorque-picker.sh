#!/usr/bin/env bash
# Run a gotorque campaign (https://github.com/asaf-shitrit/gotorque, MIT) on
# the picker's startup path (#874). Nothing here vendors gotorque: build it
# yourself (Go 1.26+, git, GNU patch) and point GOTORQUE at it.
#
# Usage: GOTORQUE=/path/to/gotorque tests/perf/gotorque-picker.sh [stub|live|print]
#   stub   `--adk-stub`: deterministic stub agents, proves the manifest and
#          pipeline work (default)
#   live   `--adk`: model-driven; needs OPENROUTER_API_KEY in the environment
#   print  only print the optimize command
#
# gotorque copies its target out of a Go module root, so the picker module is
# exported into a scratch git repo of its own (this repo's root has no go.mod).
# The workload is `picker --dump-first-frame` against the scratch tmux server
# from picker-open-latency.sh (no agent panes: their staleness would change
# the frame mid-campaign), so tmux's socket rides in via the manifest's env
# passthrough. Everything is torn down on exit.
# Linux only (GNU tar --transform, the fixture script).
set -euo pipefail

ORIG_HOME="$HOME"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-stub}"
GOTORQUE="${GOTORQUE:-gotorque}"
MANIFEST="$REPO_ROOT/tests/perf/gotorque/picker-manifest.json"

W="$(mktemp -d /tmp/og-gotorque.XXXX)"
fixture_pid=""
cleanup() {
	[ -z "$fixture_pid" ] || kill "$fixture_pid" 2>/dev/null || true
	rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$W/repo"
(cd "$REPO_ROOT" && git ls-files -co --exclude-standard -z -- picker |
	tar --null -T - --transform 's,^picker/,,' -cf -) | tar -x -C "$W/repo"
git -C "$W/repo" init -q
git -C "$W/repo" add -A
git -C "$W/repo" -c user.email=t@t -c user.name=t commit -q -m picker

FIXTURE_AGENTS=0 FIXTURE_ENV_FILE="$W/fixture.env" "$REPO_ROOT/tests/perf/picker-open-latency.sh" fixture >/dev/null 2>"$W/fixture.log" &
fixture_pid=$!
for _ in $(seq 1 100); do
	[ -s "$W/fixture.env" ] && break
	kill -0 "$fixture_pid" 2>/dev/null || break
	sleep 0.2
done
[ -s "$W/fixture.env" ] || {
	echo "fixture did not come up:" >&2
	cat "$W/fixture.log" >&2
	exit 1
}
# shellcheck disable=SC1091  # written by the fixture above
source "$W/fixture.env"
# The fixture moved HOME into its scratch dir; gotorque keeps the real one, and
# the picker workload gets the fixture's through the manifest's passthrough.
export FIXTURE_HOME="$HOME"
export HOME="$ORIG_HOME"

agent_flag="--adk-stub"
[ "$MODE" = live ] && agent_flag="--adk"
cmd=("$GOTORQUE" optimize --repo "$W/repo" --manifest "$MANIFEST" "$agent_flag")
echo "+ ${cmd[*]}"
[ "$MODE" = print ] && exit 0
"${cmd[@]}"
echo "campaign output is under gotorque's own state dir; report it with: $GOTORQUE report <campaign-dir>"
