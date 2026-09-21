package statefile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agentScreenOption is a NEW pane option, distinct from @claude_status: the
// laptop side keeps hook-vs-screen precedence (lib-claude's read_pane_state,
// the picker's collectAgentPanesFrom) and both sources must stay
// distinguishable once they cross the bridge.
const agentScreenOption = "@agent_screen"

// TmuxRunner runs a tmux command; the production default execs the real
// binary, tests inject a fake to assert the stamp/unstamp without touching a
// live server.
type TmuxRunner func(args ...string) error

func runTmux(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

type Writer struct {
	dir, paneID, last string
	server            string
	tmux              TmuxRunner
}

func New(dir, paneID string) *Writer { return &Writer{dir: dir, paneID: paneID, tmux: runTmux} }

// NewWithTmuxRunner is New with an injected TmuxRunner, for callers outside
// this package that need a hermetic Writer in tests — New's default runs the
// real tmux binary.
func NewWithTmuxRunner(dir, paneID string, tmux TmuxRunner) *Writer {
	return &Writer{dir: dir, paneID: paneID, tmux: tmux}
}

// WithServer stamps every state file with the writing tmux server's pid, the
// ownership key claude_prune_stale_state protects a live server's screen state
// by. An empty pid leaves the file unstamped.
func (w *Writer) WithServer(pid string) *Writer {
	w.server = pid
	return w
}

// Update records a newly observed agent state and its counted flags. An empty
// state is manifest matching's positive verdict that no rule matched the
// current screen, i.e. the agent is gone — not "nothing to report" — so it
// delegates to Clear.
//
// The write is skipped only when state *and* flags are both unchanged: a
// background shell finishing moves no state, and a pane that kept reporting
// the stale count would never lose the badge.
func (w *Writer) Update(state string, flags map[string]int, now time.Time) (bool, error) {
	if state == "" {
		return w.Clear()
	}
	key := stateKey(state, flags)
	if key == w.last {
		return false, nil
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return false, err
	}
	tmp := w.path() + ".tmp"
	content := fmt.Sprintf("state=%s\ntimestamp=%d\n%s%s", state, now.Unix(), w.serverLine(), flagLines(flags))
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, w.path()); err != nil {
		return false, err
	}
	w.last = key
	w.stampOption(stampValue(state, flags, now))
	return true, nil
}

func (w *Writer) serverLine() string {
	if w.server == "" {
		return ""
	}
	return "server=" + w.server + "\n"
}

// stampValue renders the tmux-mirrored form of a state: "<state> <epoch>"
// plus each flag as "name=count", space-separated so the option stays one
// line — the same key=value shape flagLines uses on disk, collapsed onto one
// row. State names come from the fixed manifest vocabulary and flags are
// counts, so nothing here can carry the '|' the daemon's row format delimits
// on.
func stampValue(state string, flags map[string]int, now time.Time) string {
	parts := []string{state, strconv.FormatInt(now.Unix(), 10)}
	for _, n := range sortedFlagNames(flags) {
		parts = append(parts, fmt.Sprintf("%s=%d", n, flags[n]))
	}
	return strings.Join(parts, " ")
}

// stampOption mirrors state onto the pane option the remote-bridge daemon's
// agent shipper subscribes to. Best effort, like claude-status-update.sh's
// bridge_stamp: a pane that no longer exists, or no tmux at all, must not
// fail the on-disk write this accompanies.
func (w *Writer) stampOption(value string) {
	if w.tmux == nil {
		return
	}
	_ = w.tmux("set-option", "-p", "-q", "-t", "%"+w.paneID, agentScreenOption, value)
}

// unstampOption removes the mirrored pane option, the Clear counterpart of
// stampOption.
func (w *Writer) unstampOption() {
	if w.tmux == nil {
		return
	}
	_ = w.tmux("set-option", "-p", "-q", "-u", "-t", "%"+w.paneID, agentScreenOption)
}

// sortedFlagNames returns flags' keys sorted, so both flagLines and
// stampValue render an unchanged set byte-identically.
func sortedFlagNames(flags map[string]int) []string {
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// flagLines renders flags as sorted key=value lines, so an unchanged set
// always produces byte-identical content and stateKey stays a valid identity.
func flagLines(flags map[string]int) string {
	if len(flags) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range sortedFlagNames(flags) {
		fmt.Fprintf(&b, "%s=%d\n", n, flags[n])
	}
	return b.String()
}

func stateKey(state string, flags map[string]int) string {
	return state + "\x00" + flagLines(flags)
}

// Clear removes the state file and forgets the last written state, so a
// later Update always writes even if the next real state matches whatever
// was just cleared. It always goes to disk rather than trusting w.last as a
// proxy for "is there a file": a freshly constructed Writer starts with
// w.last == "", even when an earlier Writer instance for the same pane left
// a file behind. Idempotent: a missing file is success, not an error.
func (w *Writer) Clear() (bool, error) {
	switch err := os.Remove(w.path()); {
	case err == nil:
		w.last = ""
		w.unstampOption()
		return true, nil
	case os.IsNotExist(err):
		w.last = ""
		return false, nil
	default:
		return false, err
	}
}

func (w *Writer) path() string { return filepath.Join(w.dir, w.paneID) }
