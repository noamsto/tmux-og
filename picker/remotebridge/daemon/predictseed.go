package daemon

import (
	"fmt"
	"os"

	"github.com/noamsto/tmux-og/picker/remotebridge/render"
)

// seedPredictModels answers every predictor that asked for a model seed: per
// pane a cursor read, a modes read and a capture, all written before any
// reply is read, the PaneSeeds pattern.
//
// The capture is each pane's last reply, and its model seed is enqueued the
// moment that reply is parsed, before the next pane's replies come off the
// stream. roundTrip routes %output into sinks while it walks past reply
// blocks, so with any reply after the capture, output landing between them
// would reach the renderer before the model existed and never reach the
// model. Reading the modes before the capture is the skew cursorCmd already
// accepts.
//
// Called from the main loop beside reseedReshaped, the only place a
// round-trip may run.
func seedPredictModels(router *Router, rt roundTrip) {
	due := router.predictModelPanes()
	ids := make([]string, 0, len(due))
	sinks := make([]*outputSink, 0, len(due))
	for _, paneID := range due {
		if s := router.sink(paneID); s != nil {
			ids = append(ids, paneID)
			sinks = append(sinks, s)
		}
	}
	if len(ids) == 0 {
		return
	}
	cmds := make([]string, 0, 3*len(ids))
	for _, id := range ids {
		cmds = append(cmds, cursorCmd(id), predictModesCmd(id), captureCmd(id))
	}
	next := rt(cmds...)
	for i, id := range ids {
		curLine, curOK := next()
		modesLine, modesOK := next()
		capLine, capOK := next()
		captured, isErr := parseCapture(capLine, capOK)
		if isErr {
			if capLine.Err != nil {
				fmt.Fprintf(os.Stderr, "daemon: predict model seed for %s: %v\n", id, capLine.Err)
			} else {
				fmt.Fprintf(os.Stderr, "daemon: predict model seed for %s: capture-pane failed\n", id)
			}
			continue
		}
		// A remote that cannot report the modes gets no model, silently.
		m, ok := parsePredictModes(modesLine, modesOK)
		if !ok {
			continue
		}
		cx, cy, alt, appck, mouse, tmodes := parseCursor(curLine, curOK)
		m.cx, m.cy = cx, cy
		sinks[i].predictSeed(render.Seed(replaceLF(captured), cx, cy, alt, appck, mouse, tmodes), m)
	}
}
