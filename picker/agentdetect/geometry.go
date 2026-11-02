package main

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"time"
)

type geom struct{ cols, rows int }

// geometryRound runs one read-then-wait: it reports the pane's current
// geometry when the read parses as one, and returns once the wait wakes or
// fails. printed says whether report was called.
type geometryRound func(ctx context.Context, report func(geom)) (printed bool, err error)

// Consecutive rounds that fail without printing a geometry before the watcher
// falls back to polling. More than one, so a single transient failure (server
// restarting, refused connection) does not pin a watcher to polling for life.
const geometryRejectLimit = 3

// geometryWaitArgs reads the pane's size and then blocks until that pane
// resizes. Both commands are one command list, run back to back on the server
// thread, so no resize can land between the read and the wait. wait-for's
// flags must precede the event name (after it they are a parse error), and the
// event payload keys are bare format names (#{pane}, not #{hook_pane}).
func geometryWaitArgs(paneID string) []string {
	pane := "%" + paneID
	return []string{
		"display", "-p", "-t", pane, "#{pane_width} #{pane_height}",
		";",
		"wait-for", "-E", "-F", "#{==:#{pane}," + pane + "}", "pane-resized",
	}
}

// runGeometryRound runs geometryWaitArgs as one tmux client. A cancelled ctx
// kills the client, which closes its stdout and removes its waiter on the
// server. The display line uses parsePaneInfo's rule: display-message exits 0
// against a dead pane with empty fields, and that is not a geometry.
func runGeometryRound(paneID string) geometryRound {
	return func(ctx context.Context, report func(geom)) (bool, error) {
		cmd := exec.CommandContext(ctx, "tmux", geometryWaitArgs(paneID)...) //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return false, err
		}
		if err := cmd.Start(); err != nil {
			return false, err
		}
		r := bufio.NewReader(stdout)
		line, _ := r.ReadString('\n')
		c, rows, _, printed := parsePaneInfo(line)
		if printed {
			report(geom{c, rows})
		}
		_, _ = io.Copy(io.Discard, r)
		return printed, cmd.Wait()
	}
}

// watchGeometry sends the pane's geometry to out on start and after every
// resize, waiting settle after each round so a drag's per-step events
// coalesce: steps inside the delay are not lost, the next read sees the size
// they left. It returns false when ctx is cancelled, and true once
// geometryRejectLimit consecutive rounds failed without printing a geometry.
//
// That rejection is the version gate: a resident server predating
// `wait-for -E` (#407) rejects the command list at parse time, so the live
// server's own parser is probed, never `tmux -V`.
//
// Residuals: a watcher killed with SIGKILL leaves its waiter blocked until
// that pane next resizes or the server exits. And a resident server built
// between upstream d29aa121 (`wait-for -E` added) and 0e1f6ed8
// (`cmd_wait_for_client_lost`) keeps a killed waiter's wait item and client
// struct until that pane next resizes or the server restarts; the parser
// cannot tell those builds apart (all report next-3.x), so it is not gated.
func watchGeometry(ctx context.Context, round geometryRound, out chan<- geom, settle time.Duration) (rejected bool) {
	report := func(g geom) {
		select {
		case out <- g:
		case <-ctx.Done():
		}
	}
	timer := time.NewTimer(settle)
	timer.Stop()
	defer timer.Stop()

	rejects := 0
	for {
		printed, err := round(ctx, report)
		if ctx.Err() != nil {
			return false
		}
		switch {
		case printed:
			rejects = 0
		case err != nil:
			rejects++
			if rejects >= geometryRejectLimit {
				return true
			}
		}
		timer.Reset(settle)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
}
