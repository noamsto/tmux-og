# prefix + H toggles crew dash (#1186)

Goal: `prefix + H` dismisses a visible dash (float or tiled) and still opens/restores otherwise. `--detached` never dismisses.

## Local script (`scripts/tmux-crew-dash.sh`)

Order after the mirror branch, one decision per press:
1. Visible float labelled `crew-dash` (existing scan, focused or not): not detached -> `tmux-float-group hide crew <pane>` (needs `$FLOAT_GROUP` executable, else `notify` and exit); detached -> focus only (today).
2. Not detached: tiled dash pane -> `tmux kill-pane -t <id>`. Detection per non-floating pane of the window (`list-panes -F '#{pane_id}|#{pane_floating_flag}|#{pane_tty}'`, index order, first match wins): `ps -ww -t "${tty#/dev/}" -o stat=,args=`, keep rows whose stat contains `+` (foreground group), match args against `^(.*/)?\.?crew(-wrapped)?[[:space:]]+dash([[:space:]]|$)`. `crew await`/`crew status` never match; `pane_current_command` is not used. `ps -t <tty> -o stat=,args=` is valid on procps and macOS (the repo already uses `ps -o args= -p`).
3. Stashed dash -> restore (unchanged). 4. Else create (unchanged).
Detached skips step 2 and the hide in step 1.

## Mirror window

The ctl `tool` verb gets an optional third arg `toggle` (only the launcher sends it, and only when not `--detached`). With it, the existing reuse gate's focus branch becomes `kill-pane -t <registered float id>` for a visible float (the remote may lack `tmux-float-group` on a non-interactive PATH, so it is dismissed, not stashed); without it, behaviour is unchanged. A tiled `crew dash` on the remote is not detected (documented limit). Launcher argv: `tool <pane> crew-dash <dir> toggle`.

## Tests
- `tests/crew-dash-launch.bats`: crew stub runs `exec -a "crew $*" sleep` so tiled panes show `crew dash`/`crew await` in ps. Add: hide visible float focused and unfocused (restore afterward via second press), tiled `crew dash` closed, tiled `crew await` ignored (a new float opens), `--detached` does not hide/close; mirror argv gains `toggle` (detached lacks it).
- `picker/remotebridge/daemon/ctl_test.go`: tool verb with `toggle` kills the float, without it focuses; arg validation rejects other third values.

## Docs
`floats.md` (H/G bullet), `scripts.md` (tmux-crew-dash row), `bridge-daemon.md` (tool-verb row).

Gate: shellcheck, `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Revisions after plan-critic (first pass: revise)
- `crew-dash-launch-tests` (flake.nix) gets `pkgs.procps` (+ `pkgs.ps` on darwin, as the other ps checks); the bats adds a precondition asserting the stub shows `crew dash` in ps, so tiled tests cannot pass vacuously. Add a full-path argv0 case (`exec -a /x/bin/.crew-wrapped dash`).
- Verb: `optArgs` 1 -> 2, `a[2]` must equal `toggle` else error; update the `optArgs` comment. Version skew: an older resident daemon rejects the 4-arg form, shown by the existing notify; documented.
- Mirror deviation (assumption, recorded in PR body, owner may veto): remote float is killed rather than stashed (the remote reuse gate cannot see a stashed pane, so hide would need a restore branch and duplicate the local script), and a remote tiled dash is not detected.
- Bind note becomes `Toggle crew dash` (tmpl + reference.nix).
- `float-group hide crew <pressed pane>`: the pane resolves the window; hides every visible crew float there.
- Mirror ctl test extends `toolVerbDoesNotStackFloats`: press then press with `toggle` (float unfocused) -> zero floats.
