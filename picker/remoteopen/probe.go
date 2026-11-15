package remoteopen

import "strings"

type probeResult struct{ OS, UID, Tmux, Tmpdir, Sess, Win, Want string }

const probeHead = `: og-probe;
os=$(uname -s)
uid=$(id -u)
tmux_bin=$(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux)`

const probeTmpdirDefault = `
case "$os" in
	Darwin) tmpdir="/tmp" ;;
	*) tmpdir="/run/user/$uid" ;;
esac`

const probeSessCanon = `
if env TMUX_TMPDIR="$tmpdir" "$tmux_bin" has-session -t "=$sess" 2>/dev/null; then
	:
else
	sess_canon=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "$sess" -F '#{session_name}' 2>/dev/null | head -1)
	[ -n "$sess_canon" ] && sess="$sess_canon"
fi`

const probeSessLatest = `
sess=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-sessions -F '#{session_name}' | head -1)`

const probeWant = `
win=""
want_ok=""
if [ -n "$sess" ]; then
	win=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "=$sess" -F '#{window_index} #{window_active}' 2>/dev/null | awk '$2==1{print $1; exit}')
	want_ok=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "=$sess" -F '#{window_id} #{window_index}' 2>/dev/null | awk -v w="$want" '$1==w||$2==w{print $2; exit}')
fi`

const probeWinPlaced = `
win=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "=$sess" -F '#{window_index} #{window_active}' 2>/dev/null | awk '$2==1{print $1; exit}')`

const probeWinActive = `
win=""
if [ -n "$sess" ]; then
	win=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "$sess" -F '#{window_index} #{window_active}' | awk '$2==1{print $1; exit}')
fi`

const probePrintf = `
printf 'os=%s\nuid=%s\ntmux=%s\ntmpdir=%s\nsess=%s\nwin=%s\nwant=%s\n' "$os" "$uid" "$tmux_bin" "$tmpdir" "$sess" "$win" "$want_ok"`

// buildProbeScript returns the remote one-round-trip probe, without the
// trailing newline the herestring adds.
func buildProbeScript(o Opts) string {
	var b strings.Builder
	b.WriteString(probeHead)

	if o.Tmpdir != "" {
		b.WriteString("\ntmpdir_lit=" + shellQuote(o.Tmpdir) + "\ntmpdir=\"$tmpdir_lit\"")
	} else {
		b.WriteString(probeTmpdirDefault)
	}

	if o.Sess != "" {
		b.WriteString("\nsess_lit=" + shellQuote(o.Sess) + "\nsess=\"$sess_lit\"")
		if o.NewDir == "" && o.Restore == "" && !o.WantMode && !o.Placed {
			b.WriteString(probeSessCanon)
		}
	} else {
		b.WriteString(probeSessLatest)
	}

	switch {
	case o.WantMode:
		b.WriteString("\nwant_lit=" + shellQuote(o.Win) + "\nwant=\"$want_lit\"")
		b.WriteString(probeWant)
	case o.Placed:
		b.WriteString(probeWinPlaced)
	case o.Win != "":
		b.WriteString("\nwin_lit=" + shellQuote(o.Win) + "\nwin=\"$win_lit\"")
	default:
		b.WriteString(probeWinActive)
	}

	b.WriteString(probePrintf)
	return b.String()
}

// parseProbe reads the probe output: last occurrence wins, unknown lines
// (shell greetings) are ignored, values stay verbatim.
func parseProbe(out string) probeResult {
	var r probeResult
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "os="):
			r.OS = line[len("os="):]
		case strings.HasPrefix(line, "uid="):
			r.UID = line[len("uid="):]
		case strings.HasPrefix(line, "tmux="):
			r.Tmux = line[len("tmux="):]
		case strings.HasPrefix(line, "tmpdir="):
			r.Tmpdir = line[len("tmpdir="):]
		case strings.HasPrefix(line, "sess="):
			r.Sess = line[len("sess="):]
		case strings.HasPrefix(line, "win="):
			r.Win = line[len("win="):]
		case strings.HasPrefix(line, "want="):
			r.Want = line[len("want="):]
		}
	}
	return r
}
