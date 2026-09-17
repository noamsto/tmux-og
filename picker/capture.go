package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// captureBGReset ends every captured row so a pane's own background cannot
// bleed into the tile padding cells around it (same reason as loadPreviewCmd).
const captureBGReset = "\033[49m"

// captureTimeout bounds one batch. The wall's clock fires every 500ms, so an
// unbounded call against a hung tmux socket would pile subprocesses up without
// limit; the bridge's graphics fetches bound themselves the same way.
const captureTimeout = 2 * time.Second

// captureSeq makes each batch's marker unique within a process. Atomic because
// captureTargets runs on bubbletea's command goroutines, not the update loop.
var captureSeq atomic.Uint64

type captureRunner func(args ...string) ([]byte, error)

// defaultCaptureRunner is the real tmux invocation captureTargets and sendKeys
// fall back to when a caller passes no runner.
func defaultCaptureRunner(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", args...).Output()
}

// captureErr names the target whose capture aborted the batch.
type captureErr struct {
	Target string
	Err    error
}

func (e *captureErr) Error() string {
	return fmt.Sprintf("capture-pane -t %s: %v", e.Target, e.Err)
}

func (e *captureErr) Unwrap() error { return e.Err }

// captureTargets returns each target's visible pane content keyed by target,
// from one batched tmux call: `capture-pane … ; display-message -p <marker>`
// per target, so every capture is terminated by a marker line.
//
// tmux aborts the whole batch at the first bad target (exit 1, stdout holding
// everything up to the last marker it managed to print, nothing after). One
// terminated section per completed capture is what makes the count of parsed
// sections the index of the offender — callers keep the good tiles refreshing
// and drop that target from the next batch. Only a "can't find …" failure is
// attributed that way (see captureTargetGone); anything else is transient.
//
// The marker carries this process's pid and a per-call counter and is passed in
// argv, so a pane that prints a marker-shaped line cannot forge the real one.
func captureTargets(targets []string, run captureRunner) (map[string]string, error) {
	out := make(map[string]string, len(targets))
	if len(targets) == 0 {
		return out, nil
	}
	if run == nil {
		run = defaultCaptureRunner
	}

	// One capture per distinct target; the deduped set is the requested key
	// set, so keying the map off it covers every requested target.
	distinct := make([]string, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		distinct = append(distinct, t)
	}

	marker := fmt.Sprintf("@@og-wall-%d-%d@@", os.Getpid(), captureSeq.Add(1))
	args := make([]string, 0, len(distinct)*9)
	for _, t := range distinct {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, "capture-pane", "-p", "-e", "-t", t, ";", "display-message", "-p", marker)
	}

	stdout, runErr := run(args...)
	parts := splitCaptures(string(stdout), marker)
	for i, p := range parts {
		if i >= len(distinct) {
			break
		}
		out[distinct[i]] = p
	}
	if runErr != nil {
		if len(parts) < len(distinct) && captureTargetGone(runErr) {
			return out, &captureErr{Target: distinct[len(parts)], Err: runErr}
		}
		// Unattributed, so nothing is blacklisted and the next tick retries.
		return out, fmt.Errorf("capture-pane batch: %w", runErr)
	}
	return out, nil
}

// captureTargetGone reports whether the failure is tmux saying the target does
// not exist — the only failure a single target can be blamed for. exec.Output()
// puts tmux's stderr on the ExitError, and it reads "can't find session: …" /
// "can't find window: …" / "can't find pane: …". Everything else is transient or
// systemic (the server busy during a config reload, the captureTimeout killing
// the process, an exec-level error): blaming a target for one of those would
// freeze a healthy tile at (gone) until its pane actually closes.
func captureTargetGone(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return strings.Contains(string(exitErr.Stderr), "can't find ")
}

// splitCaptures cuts stdout into one section per marker-terminated capture.
// A full-line match only: a pane row merely containing the marker is content.
// Anything trailing the last marker is an unfinished capture, so it is dropped.
func splitCaptures(stdout, marker string) []string {
	var parts []string
	var rows []string
	for _, line := range strings.Split(stdout, "\n") {
		if line == marker {
			parts = append(parts, joinCaptureRows(trimBlankRows(rows)))
			rows = rows[:0]
			continue
		}
		rows = append(rows, stripStringEscapes(line))
	}
	return parts
}

// stripStringEscapes removes OSC (ESC ]) and DCS (ESC P) strings from s, keeping
// CSI sequences — an SGR color surviving is the whole point of `capture-pane -e`.
//
// tmux replays a pane's OSC 8 hyperlinks verbatim, which would make tile text a
// clickable attacker-chosen link, and truncateVisibleWidth understands only CSI:
// it counts an OSC's bytes as visible cells and can cut one in half, leaving
// hyperlink mode open past that tile. Stripping here keeps that crop's escape
// parser untouched.
//
// A string ends at BEL or ST (ESC \) and, because this runs on both single rows
// and whole multi-row captures, at a newline — so an unterminated sequence can
// never eat the rows after it.
func stripStringEscapes(s string) string {
	if !strings.Contains(s, "\033]") && !strings.Contains(s, "\033P") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\033' || i+1 >= len(s) || (s[i+1] != ']' && s[i+1] != 'P') {
			out.WriteByte(s[i])
			continue
		}
		i += 2
		for i < len(s) && s[i] != '\a' && s[i] != '\n' {
			if s[i] == '\033' && i+1 < len(s) && s[i+1] == '\\' {
				i++ // the ST's backslash goes with its ESC
				break
			}
			i++
		}
		if i < len(s) && s[i] == '\n' {
			i-- // the newline is content; the loop's i++ lands back on it
		}
	}
	return out.String()
}

// trimBlankRows drops the trailing empty rows tmux pads a capture with — an
// idle pane is mostly them, and they'd render as dead space in a tile.
func trimBlankRows(rows []string) []string {
	end := len(rows)
	for end > 0 && strings.TrimSpace(stripANSI(rows[end-1])) == "" {
		end--
	}
	return rows[:end]
}

func joinCaptureRows(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	return strings.Join(rows, captureBGReset+"\n") + captureBGReset
}

// --- Keystroke relay (#316 stage 4) ---

// relayKeyArgs is the tmux send-keys argv (after "-t <target>") for key, or
// ok=false when key is outside the relay's deliberately small scope: printable
// characters, Enter, Escape and Backspace. Arrows, C-* and function keys are
// never relayed — a mapping table for the full key space is never complete,
// and a half-working relay is worse than none; ↵ already jumps to the real
// pane, which is full fidelity by definition, for anything richer.
//
// Escape maps here for the table's own completeness, but in practice a
// focused esc always unfocuses (see applyWallEsc in tui.go) before this
// mapping is ever consulted for it — a literal Escape can never reach the
// pane through the wall.
func relayKeyArgs(key string) (args []string, ok bool) {
	switch key {
	case "enter":
		return []string{"Enter"}, true
	case "esc":
		return []string{"Escape"}, true
	case "backspace":
		return []string{"BSpace"}, true
	}
	if text, ok := printableKeyText(key); ok {
		// -l -- sends the byte literally; without it a single-character key like
		// "0" or ";" would be read as a key name instead of typed text.
		return []string{"-l", "--", text}, true
	}
	return nil, false
}

// sendKeys relays one keystroke to target via tmux send-keys, with the exec
// function injectable for tests — the same seam captureTargets uses.
func sendKeys(target string, args []string, run captureRunner) error {
	if run == nil {
		run = defaultCaptureRunner
	}
	full := append([]string{"send-keys", "-t", target}, args...)
	_, err := run(full...)
	return err
}
