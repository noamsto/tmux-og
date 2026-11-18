package daemon

import (
	"fmt"
	"os"
	"strings"
	"syscall"
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
	_ = writeNoFollow(path, []byte(line+"\n"), 0o600)
}

// writeNoFollow truncates-and-writes path, refusing to follow a symlink planted
// there so the write cannot land on the link's target.
func writeNoFollow(path string, data []byte, perm os.FileMode) error {
	// No O_TRUNC: truncating before the checks would empty a planted hard link's
	// target. O_NONBLOCK keeps a planted FIFO from blocking the open.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm) //nolint:gosec // G302/G304: socket-derived path, owner-only perm
	if err != nil {
		return err
	}
	if !isPrivateRegular(f) {
		_ = f.Close()
		return fmt.Errorf("refusing %s: not a private regular file", path)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// isPrivateRegular reports whether f is a regular file we own with no other
// hard link.
func isPrivateRegular(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && uint64(st.Nlink) == 1 //nolint:gosec // G115: euid is non-negative
}

// clearPhase drops the file on teardown, so a launcher that reuses this socket
// path cannot show the previous bridge's last caption before writing its own.
func clearPhase(cfg Config) {
	if path := phasePath(cfg); path != "" {
		_ = os.Remove(path)
	}
}
