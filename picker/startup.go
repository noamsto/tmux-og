package main

import (
	"os/exec"
	"strings"
)

// startupSep separates the outputs of the commands in collectTmux's one tmux
// invocation.
const startupSep = "@@og-picker-sep@@"

// tmuxData is everything a picker frame reads from tmux.
type tmuxData struct {
	opts map[string]string // only when the read included show -g
	snap panesSnapshot
	// Window mode only: its own pane rows (windowsArgv) and session activity.
	windowRows []string
	activity   map[string]int64
}

// collectTmuxArgv chains a frame's tmux reads into one command list, so the
// picker pays one fork and one server round-trip for them instead of one each.
// The first paint also wants the global options; the 1s refresh does not.
func collectTmuxArgv(windowMode, withOpts bool) []string {
	var argv []string
	if withOpts {
		argv = append(showOptionsArgv(), ";", "display-message", "-p", startupSep, ";")
	}
	argv = append(argv, panesSnapshotArgv()...)
	if windowMode {
		argv = append(argv, ";", "display-message", "-p", startupSep, ";")
		argv = append(argv, windowsArgv()...)
		argv = append(argv, ";", "display-message", "-p", startupSep, ";")
		argv = append(argv, sessionActivityArgv()...)
	}
	return argv
}

// collectTmux runs collectTmuxArgv. ok is false when tmux failed the list (a
// command in it errored), and the caller falls back to the separate reads,
// which degrade per call rather than as a whole.
func collectTmux(windowMode, withOpts bool) (tmuxData, bool) {
	out, err := exec.Command("tmux", collectTmuxArgv(windowMode, withOpts)...).Output()
	if err != nil {
		return tmuxData{}, false
	}
	return parseTmuxData(string(out), windowMode, withOpts)
}

func parseTmuxData(out string, windowMode, withOpts bool) (tmuxData, bool) {
	want := 1
	if withOpts {
		want++
	}
	if windowMode {
		want += 2
	}
	// A line equal to the separator, not a substring: an option value is free
	// text and could carry it mid-line.
	parts := make([]string, 0, want)
	var cur []string
	for _, line := range strings.Split(out, "\n") {
		if line == startupSep {
			parts = append(parts, strings.Join(cur, "\n"))
			cur = cur[:0]
			continue
		}
		cur = append(cur, line)
	}
	parts = append(parts, strings.Join(cur, "\n"))
	if len(parts) != want {
		return tmuxData{}, false
	}
	var d tmuxData
	if withOpts {
		d.opts = parseTmuxOpts(parts[0])
		parts = parts[1:]
	}
	d.snap = parsePanesSnapshot(parts[0])
	if windowMode {
		d.windowRows = strings.Split(strings.TrimSpace(parts[1]), "\n")
		d.activity = parseSessionActivity(parts[2])
	}
	return d, true
}
