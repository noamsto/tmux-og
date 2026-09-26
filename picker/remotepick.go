package main

import (
	"fmt"
	"os"
	"strings"
)

// emitPayload is the picker's only output in --remote-pick mode: a typed
// key=value block the wrapper (scripts/og-remote-picker.sh's kv_get)
// reads back over the same ssh session. See spec "The emit payload" — the
// value is everything after the first '=', so spaces and tabs round-trip.
type emitPayload struct {
	kind string // "session" or "dir"
	name string
	path string // set only for kind == "dir"
}

// maxEmitFieldLen bounds a value against a pathological row. Transport-only:
// no charset filter, since tmux session names are already near-arbitrary and
// cross today's Remote section as argv unfiltered (picker/remote.go:307-312).
const maxEmitFieldLen = 4096

// validEmitField gates a value on what the transport itself cannot carry. The
// format is one key=value per line, so a newline in a value would forge a
// second key — kv_get takes the last match, which is how a crafted session
// name could turn a session pick into a dir pick.
func validEmitField(v string) bool {
	return v != "" && len(v) <= maxEmitFieldLen &&
		!strings.ContainsAny(v, "\x00\n\r")
}

func (p emitPayload) encode() (string, error) {
	if !validEmitField(p.kind) {
		return "", fmt.Errorf("emit payload: unusable kind %q", p.kind)
	}
	if !validEmitField(p.name) {
		return "", fmt.Errorf("emit payload: unusable session name %q", p.name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "kind=%s\n", p.kind)
	if p.kind == "dir" {
		if !validEmitField(p.path) {
			return "", fmt.Errorf("emit payload: unusable path %q", p.path)
		}
		fmt.Fprintf(&b, "path=%s\n", p.path)
	}
	fmt.Fprintf(&b, "name=%s\n", p.name)
	return b.String(), nil
}

// resolveEmitPick maps a selected row to the payload emit mode hands back to
// the wrapper. Pure — no tmux/zoxide call, unlike activateCurrent's ordinary
// path this mode replaces: selection here must be side-effect-free, since the
// picker runs in an ssh pty with no attached tmux client to switch.
func resolveEmitPick(item listItem) (emitPayload, bool) {
	if item.createPath != "" {
		return emitPayload{kind: "dir", path: item.createPath, name: item.createName}, true
	}
	if item.target != "" {
		return emitPayload{kind: "session", name: item.target}, true
	}
	return emitPayload{}, false
}

// writeEmitPayload encodes p and writes it to path (0600 — matches the
// pre-created file's own mode). Called only on a successful pick; cancel
// leaves the wrapper's pre-created file empty, which it reads as cancel.
func writeEmitPayload(path string, p emitPayload) error {
	data, err := p.encode()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o600)
}

// noSessionRows reports whether items — the session-mode base, which always
// carries at least a header row (buildSessionItems never omits it) — has no
// actual session.
func noSessionRows(items []listItem) bool {
	for _, it := range items {
		if !it.isHeader {
			return false
		}
	}
	return true
}

// remotePickHost resolves the ssh host ^o should float the remote picker for.
// Session-mode Remote rows carry remoteHost; window/wall mirror rows carry
// bridgeHost instead.
func remotePickHost(item listItem, windowMode bool) (string, bool) {
	if item.remoteHost != "" {
		return item.remoteHost, true
	}
	if windowMode && item.bridgeHost != "" {
		return item.bridgeHost, true
	}
	return "", false
}

// shellQuote single-quotes s for the local float pane's shell (fish),
// escaping embedded single quotes only — the Go twin of the house helper for
// that purpose, not a full fish-safe backslash quoter.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remotePickNewPaneArgs builds the `new-pane` argv that floats
// og-remote-pick over host. Unlike the bind-key y/p tool floats
// (generator/render/keys.go's mkFloat/floatBind), this one is modal
// (-O -K -C, #648): it hosts the REMOTE's own interactive picker over ssh, so
// it needs every key including the prefix sequence — the tool floats stay
// non-modal on purpose (you tab away from a running lazygit/btop/yazi and
// leave it running). -K alone doesn't block select-pane away from the float;
// -O does. -C (close on click outside) is the mouse escape for a host stuck
// in the ^o hang (#486, scripts/og-remote-picker.sh's unbounded interactive
// ssh leg) now that -K also swallows prefix + x. No version guard, unlike
// floatNewPaneGuard: this float only ever runs from the local, pinned wrapped
// tmux, same as the already-unguarded -A here.
//
// bin is @remote_pick_bin's store path — reached by option rather than bare
// name, since the tmux server's PATH is frozen until a restart (#336) and a
// fresh script is absent from it until then. host is shell-quoted because
// tmux hands the command string on to the pane's own shell, not to us.
// @float_geom repeats the percentages so tmux-float-refit can reassert them
// when the window resizes (#371) — tmux itself bakes them into cells at
// creation and never revisits them. remain-on-exit off is mkFloat's other
// stamp: a mirror window sets it on (#547) and this float is opened from
// inside one, which left its pane dead on screen (#587).
//
// While this float is open, the bridge daemon's focusLocalPane
// (picker/remotebridge/daemon/reconcile.go) refuses its own select-pane on
// this window like any other pane-switch attempt; it self-corrects on the
// next remote focus change to a different pane or the next local focus
// gesture.
//
// window is the picker's own window id (@N): by id, so the deferred command
// (remotePickScheduleArgs) still finds it after the popup pane is gone.
func remotePickNewPaneArgs(bin, host, window string) []string {
	return []string{
		"new-pane", "-t", window,
		"-x", "90%", "-y", "85%", "-X", "5%", "-Y", "8%", "-B", "heavy", "-A", "-O", "-K", "-C",
		bin + " " + shellQuote(host),
		";",
		"set", "-p", "@pane_label", "remote " + host,
		";",
		"set", "-p", "@float_geom", "90% 85% 5% 8%",
		";",
		"set", "-p", "remain-on-exit", "off",
	}
}

// remotePickScheduleArgs defers remotePickNewPaneArgs past the picker popup's
// teardown. A window holds one modal pane, so a `new-pane -O` issued while the
// popup is still open is refused ("window already has a modal pane", #766);
// whichKeyReplayDelay is the measured floor for the same collision.
func remotePickScheduleArgs(bin, host, window string) []string {
	return []string{"run-shell", "-b", "-d", whichKeyReplayDelay, "-C",
		tmuxCommandLine(remotePickNewPaneArgs(bin, host, window))}
}

// tmuxCommandLine joins argv into one string for tmux's own command parser
// (run-shell -C): every word single-quoted, with `;` left bare so it still
// separates the chained commands.
func tmuxCommandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		if a == ";" {
			words[i] = a
			continue
		}
		words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}

// emitEmptyRow is spec D3's "empty view is never blank" placeholder: an
// unselectable row shown only when emit mode has neither sessions nor zoxide
// dirs to offer.
func emitEmptyRow(tmuxOpts map[string]string) listItem {
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	reset := "\033[0m"
	text := "(no sessions, and no zoxide on this host)"
	return listItem{
		display:    cDim + text + reset,
		plain:      text,
		searchText: text,
	}
}
