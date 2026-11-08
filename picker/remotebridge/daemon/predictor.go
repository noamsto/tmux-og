package daemon

import (
	"fmt"
	"slices"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// prediction is one typed key drawn ahead of the remote's echo: a char at
// (x, y), or an erase of (x, y) when ch is 0. curX, curY is the cursor the
// remote should leave behind once it has processed the key.
type prediction struct {
	x, y       int
	ch         byte
	curX, curY int
	epoch      int
	at         time.Time
	// fed is the predictor's fedTotal when the key was typed: output "since
	// the key" is fedTotal > fed.
	fed int
}

type cellPos struct{ x, y int }

// predictor draws a mirror pane's typed keys locally before the remote echoes
// them, and repairs them from a lazily built model of the pane. It is owned by
// the sink's pump goroutine; every method returns the bytes the pump writes to
// the renderer as FrameOutput, nil for none.
//
// Invariant: while anything is painted, the local pane shows the model plus
// the painted overlay, so unpaint must run before the model is fed anything
// new or the renderer is written anything else.
type predictor struct {
	shared *predictShared
	model  *predictModel
	w, h   int

	preds      []prediction
	painted    []cellPos
	paintedAny bool

	epoch int
	// confirmed is the highest epoch some prediction of which matched the
	// remote's echo; only those epochs are painted.
	confirmed int
	// blocked refuses new predictions until the outstanding ones resolve.
	blocked bool
	// runEpoch, runX, runY are where this epoch's first prediction began,
	// bounding backspace; runEpoch is -1 until the epoch has one.
	runEpoch, runX, runY int

	// ground describes the renderer's byte stream so far; a paint is only
	// ever spliced in at a sequence boundary.
	ground streamGround

	keyAt, lastKey, lastRequest time.Time
	// fedCost is the time the model feed has taken since the last key; past
	// predictFeedBudget the model is dropped. fedTotal counts every byte fed
	// and is never reset.
	fedCost  time.Duration
	fedTotal int

	timer *time.Timer

	now          func() time.Time
	requestModel func()
	paused       func() bool
	keysLost     func() bool
}

func newPredictor(shared *predictShared, requestModel func(), paused func() bool, keysLost func() bool) *predictor {
	return &predictor{
		shared:       shared,
		confirmed:    -1,
		runEpoch:     -1,
		now:          time.Now,
		requestModel: requestModel,
		paused:       paused,
		keysLost:     keysLost,
	}
}

func appendCUP(b []byte, x, y int) []byte {
	return fmt.Appendf(b, "\x1b[%d;%dH", y+1, x+1)
}

// unpaint restores every painted cell from the model. ESC 8 ESC 7 puts the
// real cursor and pen back while keeping the slot; each cell's style starts
// from a reset because uv.Style.String carries none for a non-zero style.
func (p *predictor) unpaint() []byte {
	if !p.paintedAny {
		return nil
	}
	out := []byte("\x1b8\x1b7")
	for _, c := range p.painted {
		out = appendCUP(out, c.x, c.y)
		cell := p.cellAt(c.x, c.y)
		if cell == nil {
			out = append(out, "\x1b[m "...)
			continue
		}
		if !cell.Style.IsZero() {
			out = append(out, "\x1b[0m"...)
		}
		out = append(out, cell.Style.String()...)
		if cell.Content == "" {
			out = append(out, ' ')
		} else {
			out = append(out, cell.Content...)
		}
	}
	out = append(out, "\x1b8"...)
	p.painted = p.painted[:0]
	p.paintedAny = false
	return out
}

// paint draws the predictions of confirmed epochs dimmed, leaving the local
// cursor after them. It never paints inside a sequence the renderer stream is
// still in, nor while the real cursor sits in the last column: tmux's DECRC
// would not restore a pending wrap there.
func (p *predictor) paint() []byte {
	m := p.model
	if m == nil || !p.paintable() || !p.ground.atGround() {
		return nil
	}
	if cx, _ := m.cursor(); cx >= p.w-1 {
		return nil
	}
	// painted is empty here: every paint follows an unpaint.
	out := []byte("\x1b7")
	var last prediction
	shown := false
	for _, pr := range p.preds {
		if pr.epoch > p.confirmed {
			break
		}
		if c := m.cell(pr.x, pr.y); c == nil || c.Width != 1 {
			p.painted = p.painted[:0]
			return nil
		}
		out = appendCUP(out, pr.x, pr.y)
		out = append(out, "\x1b[0;2m"...)
		if pr.ch == 0 {
			out = append(out, ' ')
		} else {
			out = append(out, pr.ch)
		}
		p.markPainted(pr.x, pr.y)
		last, shown = pr, true
	}
	if !shown {
		return nil
	}
	out = appendCUP(out, last.curX, last.curY)
	p.paintedAny = true
	return out
}

// cellAt is the model's cell, nil without a model or outside the pane.
func (p *predictor) cellAt(x, y int) *uv.Cell {
	if p.model == nil {
		return nil
	}
	return p.model.cell(x, y)
}

func (p *predictor) markPainted(x, y int) {
	for _, c := range p.painted {
		if c.x == x && c.y == y {
			return
		}
	}
	p.painted = append(p.painted, cellPos{x, y})
}

func (p *predictor) paintable() bool {
	m := p.model
	return m != nil && !p.paused() && p.shared.want() &&
		!m.alt() && !m.hidden && !m.insert && !m.origin && !m.slotUsed
}

func (p *predictor) reset() []byte {
	out := p.unpaint()
	p.preds = nil
	p.epoch++
	p.blocked = false
	p.stopTimer()
	return out
}

func (p *predictor) dropModel() {
	p.model.close()
	p.model = nil
}

// onKeys classifies a chunk of typed bytes: printable ASCII and backspace are
// predicted, anything else is a barrier that ends the chunk's predictions.
func (p *predictor) onKeys(b []byte) []byte {
	if p.keysLost() || p.paused() {
		return p.reset()
	}
	t := p.now()
	p.lastKey = t
	p.fedCost = 0
	if p.keyAt.IsZero() {
		p.keyAt = t
	}
	if !p.shared.want() {
		out := p.reset()
		p.dropModel()
		return out
	}
	m := p.model
	if m == nil {
		if p.w > 0 && t.Sub(p.lastRequest) >= predictModelRetry {
			p.requestModel()
			p.lastRequest = t
		}
		return nil
	}
	if !p.paintable() {
		return p.reset()
	}

	added := false
keys:
	for _, c := range b {
		switch {
		case c == 0x7f || c == 0x08:
			if p.blocked {
				continue
			}
			x, y := p.base(m)
			if p.runEpoch != p.epoch || y != p.runY || x-1 < p.runX || len(p.preds) >= predictMaxPending {
				p.blocked = true
				continue
			}
			p.preds = append(p.preds, prediction{x: x - 1, y: y, curX: x - 1, curY: y, epoch: p.epoch, at: t, fed: p.fedTotal})
			added = true
		case c >= 0x20 && c <= 0x7e:
			if p.blocked {
				continue
			}
			x, y := p.base(m)
			if cell := m.cell(x, y); x+1 > p.w-2 || len(p.preds) >= predictMaxPending || cell == nil || cell.Width != 1 {
				p.blocked = true
				continue
			}
			p.preds = append(p.preds, prediction{x: x, y: y, ch: c, curX: x + 1, curY: y, epoch: p.epoch, at: t, fed: p.fedTotal})
			if p.runEpoch != p.epoch {
				p.runEpoch, p.runX, p.runY = p.epoch, x, y
			}
			added = true
		default:
			p.epoch++
			p.blocked = true
			break keys
		}
	}
	if !added {
		return nil
	}
	out := append(p.unpaint(), p.paint()...)
	p.arm()
	return out
}

// base is where the next key lands: the model cursor, or after the last
// outstanding prediction.
func (p *predictor) base(m *predictModel) (x, y int) {
	if len(p.preds) > 0 {
		last := p.preds[len(p.preds)-1]
		return last.curX, last.curY
	}
	return m.cursor()
}

// onOutput wraps a remote output payload in unpaint and paint, feeding it to
// the model in between and settling the outstanding predictions against it.
func (p *predictor) onOutput(b []byte) []byte {
	if !p.keyAt.IsZero() {
		p.shared.sample(p.now().Sub(p.keyAt))
		p.keyAt = time.Time{}
	}
	p.ground.observe(b)
	m := p.model
	if m == nil {
		return b
	}
	pre := p.unpaint()
	snap := p.snapshot(m)
	p.fedTotal += len(b)
	if p.now().Sub(p.lastKey) > predictIdle || !p.feedTimed(m, b) {
		p.reset()
		p.dropModel()
		return joinBytes(pre, b, nil)
	}
	p.resolve(m, snap)
	if !p.paintable() {
		p.preds = nil
	}
	if len(p.preds) == 0 {
		p.blocked = false
	}
	p.arm()
	return joinBytes(pre, b, p.paint())
}

// feedTimed feeds b to the model in slices, adding each slice's time to
// fedCost, and reports false once the budget is spent or a feed fails. vt's
// scroll and erase cost grows with pane area, so time bounds the stall where
// bytes could not. A slice ends just after an ASCII byte: vt's grapheme
// buffer is flushed at the end of every Write, so a cut inside a multi-rune
// cluster would size it differently from local tmux; the buffer is empty
// after an ASCII byte, which makes slicing equal to one Write. A run with no
// ASCII byte after the slice size is fed whole: it can only wrap and scroll.
func (p *predictor) feedTimed(m *predictModel, b []byte) bool {
	for len(b) > 0 {
		if p.fedCost > predictFeedBudget {
			return false
		}
		n := len(b)
		for i := predictFeedSlice - 1; i < len(b); i++ {
			if b[i] < 0x80 {
				n = i + 1
				break
			}
		}
		start := p.now()
		ok := m.feed(b[:n])
		p.fedCost += p.now().Sub(start)
		if !ok {
			return false
		}
		b = b[n:]
	}
	return true
}

// resolve settles predictions against the payload just fed: the run it
// provably completed, else head by head while the cursor is past a char.
func (p *predictor) resolve(m *predictModel, pre predSnap) {
	if p.settlePrefix(m, pre) {
		return
	}
	// A barrier typed after these keys bumped the epoch: their verdict can
	// no longer confirm anything, and a cursor the barrier moved off their
	// row would leave them painted there until expiry.
	if len(p.preds) > 0 && p.preds[0].epoch < p.epoch {
		p.preds = nil
		return
	}
	cx, cy := m.cursor()
	for len(p.preds) > 0 {
		pr := p.preds[0]
		if cy != pr.y || pr.ch == 0 || cx <= pr.x {
			return
		}
		if cellText(m.cell(pr.x, pr.y)) != string(pr.ch) {
			// A later key may have overwritten the cell; only an untouched
			// one shows the remote drew something else.
			if !overwritten(p.preds[1:], pr) {
				p.preds = nil
				p.epoch++
			}
			return
		}
		p.preds = p.preds[1:]
		p.confirmed = max(p.confirmed, pr.epoch)
	}
}

// overwritten reports whether a prediction in later touches pr's cell.
func overwritten(later []prediction, pr prediction) bool {
	return slices.ContainsFunc(later, func(o prediction) bool { return o.x == pr.x && o.y == pr.y })
}

// predSnap is the model at each outstanding prediction's cell, and its
// cursor, before a payload is fed.
type predSnap struct {
	cx, cy int
	cells  []string
}

func (p *predictor) snapshot(m *predictModel) predSnap {
	s := predSnap{cells: make([]string, len(p.preds))}
	s.cx, s.cy = m.cursor()
	for i, pr := range p.preds {
		s.cells[i] = cellText(m.cell(pr.x, pr.y))
	}
	return s
}

// cellText is a cell's content with a blank cell as a space, "" outside the
// pane.
func cellText(c *uv.Cell) string {
	switch {
	case c == nil:
		return ""
	case c.Content == "":
		return " "
	}
	return c.Content
}

// settlePrefix settles the shortest run of predictions the payload provably
// completed: the model cursor is where the run leaves it, every typed char the
// run leaves standing is in its cell, and the payload moved the cursor or
// changed one of those cells. Erased cells are not compared: zsh-autosuggestions
// refills them with grey text. Keys that net to nothing (a char erased again)
// leave the screen as it was before them, so only the shortest run is proven;
// a longer one would settle keys whose echo may still be in flight. A run
// confirms its epoch only if it leaves a typed char standing, so a silent
// prompt never confirms. With no run matching, a run ending at the cursor
// whose standing char the payload replaced, and no later key touches, is a
// misprediction.
func (p *predictor) settlePrefix(m *predictModel, pre predSnap) bool {
	cx, cy := m.cursor()
	moved := cx != pre.cx || cy != pre.cy
	wrong := false
	for k, last := range p.preds {
		if last.curX != cx || last.curY != cy {
			continue
		}
		run := p.preds[:k+1]
		match, changed, typed, proven := true, moved, false, false
		for i, pr := range run {
			if pr.ch == 0 || overwritten(run[i+1:], pr) {
				continue
			}
			got := cellText(m.cell(pr.x, pr.y))
			typed = true
			changed = changed || got != pre.cells[i]
			if got != string(pr.ch) {
				match = false
				proven = proven || !overwritten(p.preds[k+1:], pr)
			}
		}
		switch {
		case !changed:
			continue
		case !match:
			wrong = wrong || proven
			continue
		}
		p.preds = p.preds[k+1:]
		if typed {
			p.confirmed = max(p.confirmed, last.epoch)
		}
		return true
	}
	if wrong {
		p.preds = nil
		p.epoch++
	}
	return wrong
}

func joinBytes(pre, b, post []byte) []byte {
	if len(pre) == 0 && len(post) == 0 {
		return b
	}
	out := make([]byte, 0, len(pre)+len(b)+len(post))
	out = append(out, pre...)
	out = append(out, b...)
	return append(out, post...)
}

// onSeed precedes a real FrameSeed: predictions are dropped, and an existing
// model follows the seed. The pump observes the seed bytes it writes.
func (p *predictor) onSeed(seed []byte) []byte {
	out := p.reset()
	if p.model != nil && !p.model.feed(seed) {
		p.dropModel()
	}
	return out
}

// onResize drops the model on a dims change. Painted cells are not restored:
// local tmux reflows them, and the reseed after every reshape repaints them;
// only the real cursor and pen come back.
func (p *predictor) onResize(w, h int) []byte {
	if w == p.w && h == p.h {
		return nil
	}
	p.w, p.h = w, h
	var out []byte
	if p.paintedAny {
		out = []byte("\x1b8")
	}
	p.painted = p.painted[:0]
	p.paintedAny = false
	p.reset()
	p.dropModel()
	return out
}

// onHold unpaints and drops the model: a hold switches screens, so painted
// predictions are unpainted first and the model, which tracked the main
// screen, is dropped; the next key re-requests one.
func (p *predictor) onHold() []byte {
	out := p.reset()
	if p.model != nil {
		p.dropModel()
	}
	return out
}

// onModelSeed builds the model from a seed that was never written to the
// renderer.
func (p *predictor) onModelSeed(seed []byte, m predictModes) {
	if p.model != nil || p.w == 0 {
		return
	}
	p.model = newPredictModel(p.w, p.h, seed, m)
}

// observe records bytes the pump writes to the renderer on its own.
func (p *predictor) observe(b []byte) { p.ground.observe(b) }

// shutdown restores the pane and closes the model when the sink's channel
// closes.
func (p *predictor) shutdown() []byte {
	out := p.reset()
	p.dropModel()
	return out
}

// release stops the timer and closes the model when the pump exits, after
// shutdown or on a renderer write error; it is safe to repeat.
func (p *predictor) release() {
	p.stopTimer()
	p.dropModel()
}

func (p *predictor) timerC() <-chan time.Time {
	if p == nil || p.timer == nil {
		return nil
	}
	return p.timer.C
}

// onExpire runs when the expiry timer fires; it judges by now() rather than
// the fire time.
func (p *predictor) onExpire() []byte {
	if len(p.preds) == 0 {
		p.stopTimer()
		return nil
	}
	if p.now().Sub(p.preds[0].at) >= p.expiry() {
		// Keys that net to nothing, echoed in one payload, look exactly like
		// keys not yet echoed: nothing to repair and no evidence against the
		// epoch, so the longest such prefix is dropped without a reset. A key
		// typed after it is not expired with it, and is judged on its own
		// clock.
		k := p.drawnPrefix()
		if k < 0 {
			return p.reset()
		}
		out := p.unpaint()
		p.preds = p.preds[k+1:]
		if len(p.preds) == 0 {
			p.blocked = false
			p.stopTimer()
			return out
		}
		out = append(out, p.paint()...)
		p.arm()
		return out
	}
	p.arm()
	return nil
}

// drawnPrefix returns the index of the last prediction of the longest prefix
// that already shows in the model and that output arrived after, or -1. With
// no output since the prefix's last key, a model that draws it is no evidence:
// a no-echo prompt looks the same.
func (p *predictor) drawnPrefix() int {
	for k := range slices.Backward(p.preds) {
		if p.fedTotal > p.preds[k].fed && p.drawsPrefix(k) {
			return k
		}
	}
	return -1
}

// drawsPrefix reports whether preds[:k+1] already show in the model: the
// cursor is where the last leaves it and every char left standing is in its
// cell.
func (p *predictor) drawsPrefix(k int) bool {
	m := p.model
	if m == nil {
		return false
	}
	run := p.preds[:k+1]
	last := run[k]
	if cx, cy := m.cursor(); cx != last.curX || cy != last.curY {
		return false
	}
	for i, pr := range run {
		if pr.ch != 0 && !overwritten(run[i+1:], pr) && cellText(m.cell(pr.x, pr.y)) != string(pr.ch) {
			return false
		}
	}
	return true
}

func (p *predictor) expiry() time.Duration {
	return min(max(4*p.shared.estimate(), predictExpiryMin), predictExpiryMax)
}

func (p *predictor) arm() {
	if len(p.preds) == 0 {
		p.stopTimer()
		return
	}
	d := p.preds[0].at.Add(p.expiry()).Sub(p.now())
	if p.timer == nil {
		p.timer = time.NewTimer(d)
		return
	}
	p.timer.Reset(d)
}

func (p *predictor) stopTimer() {
	if p.timer != nil {
		p.timer.Stop()
	}
}

const (
	sgGround uint8 = iota
	sgEsc
	sgCSI
	sgOSC    // ends at BEL or ST
	sgString // APC, PM, SOS and screen's ESC k title: ends at ST only
	sgStringEsc
	sgDCSEnter // DCS before its final byte, where CAN and SUB still cancel
	sgDCS      // DCS data: ESC then anything but a backslash is more data
	sgDCSEsc
)

// streamGround tracks whether a byte stream ends at a sequence boundary. It
// is a hand-rolled loop because it sees every byte written to a predicting
// pane. Every doubt leans "not ground": it may withhold a paint, never permit
// one mid-sequence.
type streamGround struct {
	st       uint8
	utf8Left uint8
}

func (g *streamGround) atGround() bool { return g.st == sgGround && g.utf8Left == 0 }

func (g *streamGround) observe(b []byte) {
	for _, c := range b {
		g.step(c)
	}
}

func (g *streamGround) step(c byte) {
	switch g.st {
	case sgDCS:
		if c == 0x1b {
			g.st = sgDCSEsc
		}
		return
	case sgDCSEsc:
		if c == '\\' {
			g.st = sgGround
		} else {
			g.st = sgDCS
		}
		return
	}
	if c == 0x18 || c == 0x1a {
		g.st, g.utf8Left = sgGround, 0
		return
	}
	if g.utf8Left > 0 {
		if c >= 0x80 && c <= 0xbf {
			g.utf8Left--
			return
		}
		g.utf8Left = 0
	}
	switch g.st {
	case sgGround:
		switch {
		case c == 0x1b:
			g.st = sgEsc
		case c >= 0xc2 && c <= 0xdf:
			g.utf8Left = 1
		case c >= 0xe0 && c <= 0xef:
			g.utf8Left = 2
		case c >= 0xf0 && c <= 0xf4:
			g.utf8Left = 3
		}
	case sgEsc, sgStringEsc:
		if g.st == sgStringEsc && c == '\\' {
			g.st = sgGround
			return
		}
		g.escByte(c)
	case sgCSI:
		switch {
		case c == 0x1b:
			g.st = sgEsc
		case c >= 0x40 && c <= 0x7e:
			g.st = sgGround
		}
	case sgDCSEnter:
		switch {
		case c == 0x1b:
			g.st = sgEsc
		case c >= 0x40 && c <= 0x7e:
			g.st = sgDCS
		}
	case sgOSC, sgString:
		switch {
		case c == 0x1b:
			g.st = sgStringEsc
		case c == 0x07 && g.st == sgOSC:
			g.st = sgGround
		}
	}
}

// escByte handles the byte after ESC. A C0 control there executes without
// ending the sequence, as in tmux.
func (g *streamGround) escByte(c byte) {
	switch {
	case c == '[':
		g.st = sgCSI
	case c == ']':
		g.st = sgOSC
	case c == 'P':
		g.st = sgDCSEnter
	case c == '_' || c == '^' || c == 'X' || c == 'k':
		g.st = sgString
	case c >= 0x30 && c <= 0x7e:
		g.st = sgGround
	default:
		// ESC, intermediates 0x20-0x2f, C0 controls and high bytes stay in esc.
		g.st = sgEsc
	}
}
