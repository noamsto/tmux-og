package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// The carousel and dwindle-toggle verbs' outcome has to travel from the remote
// back to this daemon before the user can be told anything: the remote's only client is
// this daemon's control client, which renders no status line, so a
// display-message issued there evaporates (the same reason agentstatus.go
// pushes state at write time instead of polling for it). carouselResolveScript
// and the dwindle-toggle command stamp their verdict on the ctl pane as
// carouselVerdictOpt, and this seam is what reads it back and turns it into the
// one-line notifyLocal message the purely-local prefix+I bind shows (#593).
const carouselVerdictOpt = "@og_carousel"

// The closed set of verdicts. Whitelisted rather than sanitized: the value is
// remote-derived, and notifyLocal's escaping is the second line of defence,
// not the first.
const (
	carouselVerdictOK       = "ok"
	carouselVerdictNoImages = "noimages"
	carouselVerdictNoBin    = "nobin"

	carouselVerdictDwindleOn   = "dwindle-on"
	carouselVerdictDwindleOff  = "dwindle-off"
	carouselVerdictDwindleGrid = "dwindle-grid"
)

// carouselProbeInterval and carouselProbeAttempts bound the wait for a
// verdict. The carousel stamp rides a run-shell -b on the remote, so it lands a few
// forks after the press and the first read routinely finds nothing — an empty
// read is expected, not evidence the press failed. The budget covers a slow
// remote without leaving a keypress polling forever; past it the press is
// treated as launched, which is what the far likelier outcome is.
const (
	carouselProbeInterval = 250 * time.Millisecond
	carouselProbeAttempts = 8
)

// carouselProbe is the seam that carries a carousel verdict from a ctl handler
// to the main loop.
//
// A package-level type for the reason viewReplacer is one: the two sides — the
// acceptConns handler and runConn's select — are closures over Run's locals,
// and Run has exactly one caller, so the whole decision lives here where a Go
// test can drive it.
type carouselProbe struct {
	mu sync.Mutex
	// pending is remote ctl pane id -> reads left.
	pending map[string]int
	// armed says a wake-up is already scheduled, so a second press inside one
	// interval does not push the first press's read further out.
	armed bool
	timer *time.Timer
}

// newCarouselProbe builds the seam with its timer stopped. The timer is
// created once per Run and only ever Reset: its channel is what runConn
// selects on, and a per-press channel would be a handle the loop cannot see
// (the reason loopTick is built once too).
func newCarouselProbe() *carouselProbe {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &carouselProbe{pending: map[string]int{}, timer: t}
}

// C is the arm runConn selects on, so a verdict is read a quarter-second after
// the press rather than at the next mainLoopTickInterval — a keypress the user
// is waiting on, and a press with no images produces no stream traffic of its
// own to wake the loop with.
func (p *carouselProbe) C() <-chan time.Time { return p.timer.C }

// arm records a press. Called from the ctl handler after the remote command
// was actually written, so a press the daemon never sent is never waited on.
func (p *carouselProbe) arm(pane string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[pane] = carouselProbeAttempts
	if !p.armed {
		p.armed = true
		p.timer.Reset(carouselProbeInterval)
	}
}

// due returns the panes to read this pass, spending one attempt each and
// dropping the presses whose budget is gone.
func (p *carouselProbe) due() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	panes := make([]string, 0, len(p.pending))
	for pane, left := range p.pending {
		if left <= 1 {
			delete(p.pending, pane)
		} else {
			p.pending[pane] = left - 1
		}
		panes = append(panes, pane)
	}
	return panes
}

func (p *carouselProbe) forget(pane string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, pane)
}

// rearm schedules the next pass while any press is still unanswered.
func (p *carouselProbe) rearm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = len(p.pending) > 0
	if p.armed {
		p.timer.Reset(carouselProbeInterval)
	}
}

// poll reads every pending pane's verdict once and reports the ones that
// answered. Runs on the main-loop goroutine, the only place a round-trip may
// run; one() per pane, sequentially, so no reply block is read while another
// batch is in flight.
func (p *carouselProbe) poll(cfg Config, rt roundTrip, send func(...string) bool) {
	for _, pane := range p.due() {
		verdict, ok := readCarouselVerdict(rt, pane)
		if !ok {
			// The stream is gone or the pane is: either way there is no
			// answer coming, and a retry would only draw another %error.
			p.forget(pane)
			continue
		}
		if verdict == "" {
			continue // not stamped yet; the attempt is already spent
		}
		p.forget(pane)
		// Unset even for a verdict we say nothing about: the script clears the
		// option at entry, but a stamp that lands after this probe gave up
		// would otherwise be read as the NEXT press's answer.
		send(carouselClearCmd(pane))
		if msg := carouselVerdictMessage(verdict, cfg.RemoteHost); msg != "" {
			notifyLocal(cfg, msg)
		}
	}
	p.rearm()
}

// readCarouselVerdict reads one pane's stamp. ok is false when the read itself
// failed, which is distinct from a pane that simply has not stamped yet.
func readCarouselVerdict(rt roundTrip, pane string) (verdict string, ok bool) {
	l, ok := one(rt, carouselVerdictCmd(pane))
	if !ok || l.Kind == controlmode.Error {
		return "", false
	}
	return strings.TrimSpace(string(l.Data)), true
}

// carouselVerdictMessage is the local status-line text for a verdict, or "" for
// one with nothing to say. An unknown value says nothing either: the option is
// remote-derived, and only the verdicts our own commands write are ours.
func carouselVerdictMessage(verdict, host string) string {
	switch verdict {
	case carouselVerdictNoImages:
		return fmt.Sprintf("no images yet for this pane on %s", host)
	case carouselVerdictNoBin:
		return fmt.Sprintf("tmux-claude-images is not on PATH on %s", host)
	case carouselVerdictDwindleOn:
		return "dwindle auto-balance: on (remote window)"
	case carouselVerdictDwindleOff:
		return "dwindle auto-balance: off (remote window)"
	case carouselVerdictDwindleGrid:
		return "dwindle: grid window - tmux-grid-refit owns this layout (remote window)"
	}
	return ""
}

func carouselVerdictCmd(pane string) string {
	return fmt.Sprintf("show-options -pqv -t %s %s", pane, carouselVerdictOpt)
}

// carouselStampCmd and carouselClearCmd are bare tmux command lines — the
// control-mode dialect, which is what send() speaks. carouselResolveScript
// runs them through the remote's own tmux binary instead, so it prefixes each
// with `tmux ` itself rather than the two dialects being kept in step by hand.
//
// That script is wrapped by tmuxQuote twice, so neither may grow a single
// quote. Neither needs one: a pane id, the option name and the verdicts are
// all constants of this package.
func carouselStampCmd(pane, verdict string) string {
	return fmt.Sprintf("set-option -p -t %s %s %s", pane, carouselVerdictOpt, verdict)
}

func carouselClearCmd(pane string) string {
	return fmt.Sprintf("set-option -pu -t %s %s", pane, carouselVerdictOpt)
}
