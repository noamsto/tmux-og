package daemon

import (
	"fmt"
	"os"
	"strings"
)

// phaseSuffix names the file og-remote-loading polls beside the socket. The
// launcher writes the first line (it owns the stretch before this process
// exists) and this daemon writes the rest, up to the respawn-pane that
// replaces the loading pane with the first mirror's renderer.
const phaseSuffix = ".phase"

// phasePath is empty when the daemon has no socket (the --test-local harness),
// which turns every phase write into a no-op rather than a file in the cwd.
func phasePath(cfg Config) string {
	if cfg.SockPath == "" {
		return ""
	}
	return cfg.SockPath + phaseSuffix
}

// setPhase replaces the loading pane's caption. Best-effort: the pane it feeds
// is cosmetic and usually already gone by the time a later phase is written, so
// a failed write is never worth failing setup over. Newlines are collapsed —
// the reader takes one line.
func setPhase(cfg Config, format string, args ...any) {
	path := phasePath(cfg)
	if path == "" {
		return
	}
	line := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")
	_ = os.WriteFile(path, []byte(line+"\n"), 0o600)
}

// clearPhase drops the file on teardown, so a launcher that reuses this socket
// path cannot show the previous bridge's last caption before writing its own.
func clearPhase(cfg Config) {
	if path := phasePath(cfg); path != "" {
		_ = os.Remove(path)
	}
}
