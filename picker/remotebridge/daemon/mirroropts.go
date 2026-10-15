package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// mirrorOpts routes the daemon's per-mirror option writes. A dedicated mirror
// session holds them as session options; a placed mirror shares its session
// with local windows, so each value goes on every window this daemon owns
// instead. The values are remembered so a window created later is stamped with
// the current state rather than a blank. A nil *mirrorOpts is a bare test
// Config and writes session-scoped in either mode; each method inlines that
// check so nilaway can see the guard.
type mirrorOpts struct {
	mu   sync.Mutex
	vals map[string]string
}

func newMirrorOpts() *mirrorOpts { return &mirrorOpts{vals: map[string]string{}} }

// set writes opt=val.
func (o *mirrorOpts) set(cfg Config, opt, val string) {
	if o == nil || !cfg.Placed {
		_ = cfg.LocalTmux("set-option", "-t", cfg.LocalSess, opt, val)
		return
	}
	o.mu.Lock()
	o.vals[opt] = val
	o.mu.Unlock()
	o.writeOwn(cfg, opt, val, false)
}

// unset removes opt.
func (o *mirrorOpts) unset(cfg Config, opt string) {
	if o == nil || !cfg.Placed {
		_ = cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, opt)
		return
	}
	o.mu.Lock()
	delete(o.vals, opt)
	o.mu.Unlock()
	o.writeOwn(cfg, opt, "", true)
}

// applyTo stamps every recorded value on a window that did not exist when it
// was written. Placed only: a dedicated mirror's values are session-scoped and
// reach every window of it already.
func (o *mirrorOpts) applyTo(cfg Config, localWin string) {
	if o == nil || !cfg.Placed {
		return
	}
	o.mu.Lock()
	opts := make([]string, 0, len(o.vals))
	for opt := range o.vals {
		opts = append(opts, opt)
	}
	slices.Sort(opts)
	var args []string
	for _, opt := range opts {
		args = appendWindowSet(args, localWin, opt, o.vals[opt], false)
	}
	o.mu.Unlock()
	if len(args) > 0 {
		_ = cfg.LocalTmux(args...)
	}
}

// writeOwn writes (or unsets) opt on every window carrying
// this daemon's socket in one tmux call. The listing is skipped over, not
// guessed at, when it fails: the recorded value reaches new windows through
// applyTo, and the next write reaches the rest.
func (o *mirrorOpts) writeOwn(cfg Config, opt, val string, unset bool) {
	own, ok := ownWindows(cfg)
	if !ok {
		return
	}
	var args []string
	for _, id := range own {
		args = appendWindowSet(args, id, opt, val, unset)
	}
	if len(args) > 0 {
		_ = cfg.LocalTmux(args...)
	}
}

// appendWindowSet appends one window-scoped set-option to a ";"-joined argv.
func appendWindowSet(args []string, win, opt, val string, unset bool) []string {
	if len(args) > 0 {
		args = append(args, ";")
	}
	args = append(args, "set-option", "-w")
	if unset {
		return append(args, "-u", "-t", win, opt)
	}
	return append(args, "-t", win, opt, val)
}

// nudgePathFor is the file the host session's @bridge_nudge option points at.
// A dedicated mirror owns its session, so the file hangs off its socket. Every
// daemon placed in one host session must touch and watch the same file, so it
// is named by the local server and session id instead.
func nudgePathFor(cfg Config) string {
	if !cfg.Placed {
		return cfg.SockPath + resizeNudgeSuffix
	}
	return filepath.Join(filepath.Dir(cfg.SockPath),
		"og-nudge-"+cfg.local.serverPID+"-"+strings.TrimPrefix(cfg.LocalTarget, "$")+resizeNudgeSuffix)
}

// unregisterPlacedNudge unsets the shared @bridge_nudge only when this daemon
// is the last one placed in the host session: another daemon's windows still
// need the option their resize watcher stats. A listing that fails leaves it
// set — a stale option costs a stat, a missing one silently drops every other
// daemon's resize nudges to the slow fallback.
func unregisterPlacedNudge(cfg Config) {
	wins, ok := hostWindows(cfg)
	if !ok {
		return
	}
	for _, w := range wins {
		if w.placed && w.sock != cfg.SockPath {
			return
		}
	}
	_ = cfg.LocalTmux("set-option", "-u", "-t", cfg.target(), bridgeNudgeOption)
}
