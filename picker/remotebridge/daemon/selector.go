package daemon

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

var (
	selectorIDRe  = regexp.MustCompile(`^@(0|[1-9][0-9]*)$`)
	selectorOptRe = regexp.MustCompile(`^@[A-Za-z_][A-Za-z0-9_-]{0,63}=[A-Za-z0-9._/@+:-]{1,255}$`)
)

// WindowSelector names the remote windows an --into daemon mirrors: either one
// window id ("@12") or every window whose option equals a value
// ("@crew_branch=feat/x"). The accepted charset holds no tmux format or shell
// metacharacter, so it is spliced into a remote format unquoted.
type WindowSelector struct {
	raw   string
	id    string // "@N" for the id form
	opt   string // "@name" for the option form
	value string
}

// ParseWindowSelector validates s and returns the selector it spells.
func ParseWindowSelector(s string) (WindowSelector, error) {
	if selectorIDRe.MatchString(s) {
		return WindowSelector{raw: s, id: s}, nil
	}
	if selectorOptRe.MatchString(s) {
		opt, value, _ := strings.Cut(s, "=")
		return WindowSelector{raw: s, opt: opt, value: value}, nil
	}
	return WindowSelector{}, fmt.Errorf("window selector must be @<id> or @<option>=<value> ([A-Za-z0-9._/@+:-]), got %q", s)
}

func (s WindowSelector) String() string { return s.raw }

func (s WindowSelector) IsZero() bool { return s.raw == "" }

// flag is the remote format that evaluates the selector to 0 or 1. Evaluating it
// remotely gives one snapshot per read, and only the id and a 0/1 cross the
// bridge, never the option's value.
func (s WindowSelector) flag() string {
	if s.id != "" {
		return "#{==:#{window_id}," + s.id + "}"
	}
	return "#{==:#{" + s.opt + "}," + s.value + "}"
}

// listFormat is windowListFormat with the selector flag after window_active;
// the name stays last because it is the only free-form field.
func (s WindowSelector) listFormat() string {
	return "'#{window_index} #{window_id} #{window_active} " + s.flag() + " #{window_name}'"
}

// subscribes reports whether a change of the selected set can be announced by a
// subscription. An id selector's set only changes when the window is closed,
// which the structural notifications already report.
func (s WindowSelector) subscribes() bool { return s.opt != "" }

// selState is whether a listed window matches the selector.
type selState int

const (
	selUnknown selState = iota
	selNo
	selYes
)

// parseSelectedWindowList turns a list-windows reply body (listFormat) into the
// ordered remote windows, dropping blank rows and rows too short to carry a flag.
// A row whose flag is neither 0 nor 1 is kept as selUnknown: positive evidence
// only, so a kept id is never closed as missing.
func parseSelectedWindowList(body string) []remoteWindow {
	var wins []remoteWindow
	for row := range strings.SplitSeq(body, "\n") {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		idx, rest, ok := strings.Cut(row, " ")
		if !ok {
			continue
		}
		id, rest, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		active, rest, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		flag, name, _ := strings.Cut(rest, " ")
		sel := selUnknown
		switch flag {
		case "1":
			sel = selYes
		case "0":
			sel = selNo
		}
		wins = append(wins, remoteWindow{index: idx, id: id, active: active == "1", name: name, sel: sel})
	}
	return wins
}

// windowFilter is the daemon's selection state: the selector, and when the
// selected set was last read. A nil *windowFilter is the unfiltered daemon and
// every method is a no-op for it.
type windowFilter struct {
	sel        WindowSelector
	dirty      bool // an og_select notification arrived since the last reconcile
	subscribed bool // the remote accepted the og_select subscription
	last       time.Time
}

// newWindowFilter returns nil for a zero selector. last is seeded so the first
// backstop read is a full floor away; the startup reconcile resets it anyway.
func newWindowFilter(sel WindowSelector) *windowFilter {
	if sel.IsZero() {
		return nil
	}
	return &windowFilter{sel: sel, last: time.Now()}
}

func (f *windowFilter) listCmd(remoteSession string) string {
	format := windowListFormat
	if f != nil {
		format = f.sel.listFormat()
	}
	return fmt.Sprintf("list-windows -t %s -F %s", tmuxQuote(remoteSession), format)
}

func (f *windowFilter) parse(body string) []remoteWindow {
	if f == nil {
		return parseWindowList(body)
	}
	return parseSelectedWindowList(body)
}

// observe notes a change of the selected set reported by the og_select subscription.
func (f *windowFilter) observe(l controlmode.Line) {
	if _, ok := subscriptionValue(l, selectSubName); ok {
		f.note()
	}
}

func (f *windowFilter) note() {
	if f != nil {
		f.dirty = true
	}
}

// due reports whether the selected set should be re-read: at once after a
// notification, else at the poll floor. An id selector never is, since only a
// close changes it.
func (f *windowFilter) due(now time.Time) bool {
	if f == nil || !f.sel.subscribes() {
		return false
	}
	return f.dirty || now.Sub(f.last) >= pollFloor(f.subscribed, mainLoopTickInterval, windowLabelBackstopInterval)
}

func (f *windowFilter) reconciled(now time.Time) {
	if f == nil {
		return
	}
	f.dirty = false
	f.last = now
}

// selectedWindows keeps the rows showing the selector's flag as 1.
func selectedWindows(wins []remoteWindow) []remoteWindow {
	var sel []remoteWindow
	for _, rw := range wins {
		if rw.sel == selYes {
			sel = append(sel, rw)
		}
	}
	return sel
}
