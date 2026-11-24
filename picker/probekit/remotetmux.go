package probekit

// RemoteTmuxBin resolves the remote's tmux without a shell assignment: PATH
// first, then the nix per-user profile a non-interactive ssh does not see.
const RemoteTmuxBin = `$(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux)`

// RemoteTmuxCmd runs one tmux argument string under the same TMUX_TMPDIR /
// binary resolution as og-remote-open. Both legs name the PARENT of the
// socket dir — tmux appends tmux-<uid> to $TMUX_TMPDIR itself — so
// /run/user/<uid> on Linux and /tmp on macOS, which has no $XDG_RUNTIME_DIR
// and keeps its server at tmux's own /tmp/tmux-<uid> default. It tries Linux
// first and a wrong guess costs a stat.
// Both legs are silenced and OR'd, so a missing server yields empty stdout
// rather than an error a caller would read as unreachable. Everything it emits
// must stay fish-safe: no `var=value` assignments — fish login shells reject
// them, and the picker would mark a reachable host unreachable.
func RemoteTmuxCmd(args string) string {
	leg := func(tmpdir string) string {
		return `env TMUX_TMPDIR=` + tmpdir + ` ` + RemoteTmuxBin + ` ` + args + ` 2>/dev/null`
	}
	return leg(`/run/user/$(id -u)`) + ` || ` + leg(`/tmp`)
}
