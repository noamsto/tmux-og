package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// holdWait bounds the barrier before a reshape; holdPoll spaces its probes.
// Package vars so tests can shrink them.
var (
	holdWait = 200 * time.Millisecond
	holdPoll = 2 * time.Millisecond
)

// holdHeaderFormat is the per-pane header preceding each capture in a hold
// read. Pipe-delimited: tmux rewrites tabs and newlines for a non-UTF-8 client.
// pane_dead and window_height trail so the geometry fields keep their positions.
const holdHeaderFormat = "#{@bridge_pane}|#{alternate_on}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{cursor_x}|#{cursor_y}|#{pane_dead}|#{window_height}"

// holdReadArgs batches, for every local pane, a header line and the pane's
// styled visible rows into one tmux invocation, so a reshape pays one fork
// however many panes it holds.
func holdReadArgs(locals []string) []string {
	var argv []string
	for _, l := range locals {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv,
			"display-message", "-p", "-t", l, holdHeaderFormat, ";",
			"capture-pane", "-e", "-p", "-t", l)
	}
	return argv
}

// holdPollArgs batches one alternate_on probe per local pane.
func holdPollArgs(locals []string) []string {
	var argv []string
	for _, l := range locals {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv, "display-message", "-p", "-t", l, "#{alternate_on}")
	}
	return argv
}

// holdRead is one local pane's state as captured for a hold.
type holdRead struct {
	local    string
	holdable bool
	w, h     int
	cx, cy   int
	winH     int
	rows     []string
}

// parseHoldRead decodes a holdReadArgs reply. locals[i] is the local pane
// mirroring remoteIDs[i]. Parsing is positional — each pane's capture is
// exactly its height in rows — so the first malformed or mismatched header, or
// a short capture, ends it: nothing after that point can be re-aligned. The
// panes parsed before it are returned.
func parseHoldRead(out string, locals, remoteIDs []string) []holdRead {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	var reads []holdRead
	for i, local := range locals {
		if i >= len(remoteIDs) || len(lines) == 0 {
			break
		}
		f := strings.Split(lines[0], "|")
		if len(f) != 9 || f[0] != remoteIDs[i] {
			break
		}
		// Fields 3-6 are the pane geometry, field 8 the window height.
		var n [5]int
		ok := true
		for j := range n {
			k := 3 + j
			if j == 4 {
				k = 8
			}
			v, err := strconv.Atoi(f[k])
			if err != nil || v < 0 {
				ok = false
				break
			}
			n[j] = v
		}
		if !ok || len(lines)-1 < n[1] {
			break
		}
		reads = append(reads, holdRead{
			local:    local,
			holdable: f[1] == "0" && f[2] == "1" && f[7] == "0",
			w:        n[0],
			h:        n[1],
			cx:       n[2],
			cy:       n[3],
			winH:     n[4],
			rows:     lines[1 : 1+n[1]],
		})
		lines = lines[1+n[1]:]
	}
	return reads
}

// holdBytes builds the output that parks a pane on the alternate screen
// showing a copy of rows. Local tmux never reflows a pane on the alternate
// screen, so the old content stays put while the pane grows.
//
// Output frames are split at arbitrary bytes, so the previous frame may have
// left an OSC/DCS/APC string or a CSI open; the leading ST terminates the
// string and aborts the sequence, keeping the 1049h from being swallowed.
//
// Every row is placed with its own CUP rather than joined by CRLF: a scroll
// region left set by the application would scroll a joined copy instead of
// painting it in place.
func holdBytes(rows []string, cx, cy int) []byte {
	var b strings.Builder
	b.WriteString("\x1b\\\x1b[?1049h")
	for i, r := range rows {
		b.WriteString("\x1b[" + strconv.Itoa(i+1) + ";1H")
		b.WriteString(r)
	}
	b.WriteString("\x1b[" + strconv.Itoa(cy+1) + ";" + strconv.Itoa(cx+1) + "H")
	return []byte(b.String())
}

// holdReshaped holds every pane this pass is about to reshape and reseed, and
// returns the sinks it held: each is owed a release once the pass is over.
//
// It holds only on passes where no held pane's height can drop. The alternate
// screen has no history: a height shrink deletes rows from the top when the
// cursor is low, and a regrow only adds blanks at the bottom, so the content
// ends up shifted until the seed. On the main screen a shrink pushes rows into
// history and the regrow pulls them back exactly. So the pass is left alone
// (no read, no hold) when it appends a pane (split-window shrinks one, and a
// later swap or select-layout can regrow it), swaps panes (a pane passes
// through another's cell), is zoomed or applied zoomed (a zoom transition or a
// reshape under zoom shrinks the zoomed pane to its cell and regrows it), or
// when the window itself is shorter locally than L.H (resize-window shrinks it
// before select-layout).
//
// Only a pane present on both sides of the pass is a candidate: a removed pane
// is not reshaped, an appended one has nothing on screen yet. The read rules
// out the rest: a pane already on the alternate screen never reflows, one with
// alternate-screen off cannot be held, and a dead pane's renderer will never
// write the hold.
//
// A close-only structural pass holds every candidate, since its final dims can
// equal the target while the kill still shrinks and regrows the survivor and the
// regrow reflows.
func holdReshaped(cfg Config, w *mirrorWindow, L controlmode.Layout, remote, newRemote []string, ops paneOps, zoomed bool, router *Router) []*outputSink {
	if cfg.LocalTmuxOut == nil || len(ops.Append) > 0 || len(ops.Swaps) > 0 || zoomed || w.appliedZoom {
		return nil
	}
	type target struct {
		sink *outputSink
		w, h int
	}
	var ids, locals []string
	var targets []target
	for i, id := range newRemote {
		if indexOf(remote, id) < 0 {
			continue
		}
		s := router.sink(id)
		if s == nil || s.isPaused() {
			continue
		}
		local, ok := localPaneFor(w, remote, id)
		if !ok {
			continue
		}
		ids = append(ids, id)
		locals = append(locals, local)
		targets = append(targets, target{s, L.Panes[i].W, L.Panes[i].H})
	}
	if len(locals) == 0 {
		return nil
	}
	out, err := cfg.LocalTmuxOut(holdReadArgs(locals)...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: hold read: %v\n", err)
		return nil
	}
	reads := parseHoldRead(out, locals, ids)
	for _, r := range reads {
		if r.winH > L.H {
			return nil
		}
	}
	force := len(ops.Remove) > 0
	var held []*outputSink
	var heldLocals []string
	for i, t := range targets {
		if i >= len(reads) {
			break
		}
		r := reads[i]
		if !r.holdable || (!force && r.w == t.w && r.h == t.h) {
			continue
		}
		if t.sink.hold(holdBytes(r.rows, r.cx, r.cy)) {
			held = append(held, t.sink)
			heldLocals = append(heldLocals, r.local)
		}
	}
	if len(held) == 0 {
		return nil
	}
	// Reshaping anyway on a miss is safe: a hold that lands late still shows
	// the old copy, now at the new size, and the seed releases it.
	if !awaitHeld(cfg, heldLocals) {
		fmt.Fprintf(os.Stderr, "daemon: hold %s: not confirmed in %v\n", strings.Join(heldLocals, ","), holdWait)
	}
	return held
}

// awaitHeld polls until every pane in locals reports the alternate screen, or
// holdWait passes. The hold bytes reach local tmux through each renderer's pty
// asynchronously, so a reshape issued right after enqueueing them would race
// them. An exec error gives up at once: no answer is coming back in time.
func awaitHeld(cfg Config, locals []string) bool {
	args := holdPollArgs(locals)
	deadline := time.Now().Add(holdWait)
	for {
		out, err := cfg.LocalTmuxOut(args...)
		if err != nil {
			return false
		}
		if allAltOn(out, len(locals)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(holdPoll)
	}
}

// allAltOn reports that a holdPollArgs reply answers all n panes with 1.
func allAltOn(out string, n int) bool {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != n {
		return false
	}
	for _, l := range lines {
		if l != "1" {
			return false
		}
	}
	return true
}
