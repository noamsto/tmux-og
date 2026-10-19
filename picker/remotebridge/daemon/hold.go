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
// holdFreezeMax bounds how long a pane may stay held before the pump releases
// it on its own. It is longer than holdWait because it spans hold to seed,
// which includes a reseed's WAN round trip, while holdWait bounds only a local
// barrier. Package vars so tests can shrink them.
var (
	holdWait      = 200 * time.Millisecond
	holdPoll      = 2 * time.Millisecond
	holdFreezeMax = 1 * time.Second
)

// holdHeaderFormat is the per-pane header preceding each capture in a hold
// read. Pipe-delimited: tmux rewrites tabs and newlines for a non-UTF-8 client.
const holdHeaderFormat = "#{@bridge_pane}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{pane_dead}"

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

// holdPollArgs batches one alternate_on and cursor_y probe per local pane.
func holdPollArgs(locals []string) []string {
	var argv []string
	for _, l := range locals {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv, "display-message", "-p", "-t", l, "#{alternate_on}|#{cursor_y}")
	}
	return argv
}

// holdRead is one local pane's state as captured for a hold.
type holdRead struct {
	local    string
	holdable bool
	w, h     int
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
		if len(f) != 5 || f[0] != remoteIDs[i] {
			break
		}
		w, werr := strconv.Atoi(f[2])
		h, herr := strconv.Atoi(f[3])
		if werr != nil || herr != nil || w < 0 || h < 0 || len(lines)-1 < h {
			break
		}
		reads = append(reads, holdRead{
			local:    local,
			holdable: f[1] == "1" && f[4] == "0",
			w:        w,
			h:        h,
			rows:     lines[1 : 1+h],
		})
		lines = lines[1+h:]
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
// The cursor is parked at the top-left: screen_resize_y deletes empty rows
// below the cursor first, so with the cursor on row 0 a shrink only removes
// bottom rows and a regrow adds blanks below. Every row stays at its position
// or goes blank, never shifts.
//
// Every row is placed with its own CUP rather than joined by CRLF: a scroll
// region left set by the application would scroll a joined copy instead of
// painting it in place. Rows are written bottom-up with ESC[2K: a late hold on
// a pane already shorter than h clamps each CUP past the bottom onto the last
// row, and writing upward leaves that row holding its own content.
func holdBytes(rows []string, h int) []byte {
	var b strings.Builder
	b.WriteString("\x1b\\\x1b[?1049h\x1b[" + strconv.Itoa(h) + ";1H")
	for i := h; i >= 1; i-- {
		b.WriteString("\x1b[" + strconv.Itoa(i) + ";1H\x1b[m\x1b[2K")
		if i-1 < len(rows) {
			b.WriteString(rows[i-1])
		}
	}
	b.WriteString("\x1b[1;1H")
	return []byte(b.String())
}

// holdReshaped holds every pane this pass is about to reshape and reseed, and
// returns the sinks it held: each is owed a release once the pass is over.
//
// Any pass may hold: the parked cursor keeps every row at its position through
// any sequence of shrinks and regrows (see holdBytes), so a pane still held
// from an earlier pass, or held again here, never shifts.
//
// Only a pane present on both sides of the pass is a candidate (a removed pane
// is not reshaped, an appended one has nothing on screen yet), and in a zoomed
// window only the zoomed pane, the one pane the pass reseeds. The read rules
// out the rest: a pane with alternate-screen off cannot be held, and a dead
// pane's renderer will never write the hold. A pane still held is read and
// enqueued like any other; its pump drops the second hold in stream order.
func holdReshaped(cfg Config, w *mirrorWindow, L controlmode.Layout, remote, newRemote []string, structural, zoomed bool, remoteActive string, router *Router) []*outputSink {
	if cfg.LocalTmuxOut == nil {
		return nil
	}
	zoomHides := zoomed && indexOf(newRemote, remoteActive) >= 0
	type target struct {
		sink *outputSink
		w, h int
	}
	var ids, locals []string
	var targets []target
	for i, id := range newRemote {
		if indexOf(remote, id) < 0 || (zoomHides && id != remoteActive) {
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
		tw, th := L.Panes[i].W, L.Panes[i].H
		if zoomHides {
			tw, th = L.W, L.H
		}
		ids = append(ids, id)
		locals = append(locals, local)
		targets = append(targets, target{s, tw, th})
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
	// A structural pass holds every candidate, even one already at its target
	// dims: a local kill-pane can give the freed space to a different neighbour
	// than the remote did (tmux next-3.9 gives a closed pane's space to the next
	// cell, stock releases such as 3.6a and 3.7c to the previous), so a survivor
	// whose final size matches its target may still have grown and then shrunk
	// back.
	force := structural
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
		if t.sink.hold(holdBytes(r.rows, r.h)) {
			held = append(held, t.sink)
			heldLocals = append(heldLocals, r.local)
		}
	}
	if len(held) == 0 {
		return nil
	}
	// Reshaping anyway on a miss is safe: a hold that lands late still writes
	// its rows in place at whatever size the pane has reached, and parks the
	// cursor.
	if !awaitAlt(cfg, heldLocals, "1|0") {
		fmt.Fprintf(os.Stderr, "daemon: hold %s: not confirmed in %v\n", strings.Join(heldLocals, ","), holdWait)
	}
	return held
}

// awaitAlt polls until every pane in locals reports
// #{alternate_on}|#{cursor_y} as want, or holdWait passes. Hold bytes reach
// local tmux through each renderer's pty asynchronously, so a reshape issued
// right after enqueueing them would race them. An exec error gives up at once:
// no answer is coming back in time.
func awaitAlt(cfg Config, locals []string, want string) bool {
	args := holdPollArgs(locals)
	deadline := time.Now().Add(holdWait)
	for {
		out, err := cfg.LocalTmuxOut(args...)
		if err != nil {
			return false
		}
		if allAlt(out, len(locals), want) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(holdPoll)
	}
}

// allAlt reports that a holdPollArgs reply answers all n panes with want.
func allAlt(out string, n int, want string) bool {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != n {
		return false
	}
	for _, l := range lines {
		if l != want {
			return false
		}
	}
	return true
}
