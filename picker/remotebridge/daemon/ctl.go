package daemon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// A local structural keybind inside a @bridge_win window does not act on the
// local mirror — it asks the daemon to run the equivalent command on the remote,
// and the mirror then repaints from the remote's own state. That request arrives
// as a FrameCtl on the same unix socket the renderers already use.
//
// The daemon cannot run the command and then wait for the remote's notification:
// tmux emits a notification caused by a control client's own command INSIDE that
// command's %begin/%end block (cmd-queue.c derives the block's flags from
// CMDQ_STATE_CONTROL), and controlmode.Reader folds a block's body into the
// reply's Data — so our own %layout-change never surfaces as a Line. Instead each
// request registers a reconcile intent that the main loop drains; the reply block
// for our own command is itself the line that wakes the loop back to the drain
// point, so no timer is needed.

// ctlState is everything ctl connections share with the main loop. It has its own
// mutex because mirrorWindow's fields are written by the main loop unlocked
// (registry.mu guards only the map), so a ctl goroutine must never read them —
// the main loop mirrors the pane->window mapping in here instead.
//
// Lock order is ctlState.mu -> sendMu, never the reverse: a ctl handler holds mu
// while it sends so the send and the intent registration are one critical
// section, and send never takes mu.
//
// No ctl handler may block while holding mu either, which a third edge would
// make it do: the main loop takes this same mutex during a control-client
// replacement (repair() -> reconcileWindows -> forgetWindow, daemon.go), so a
// handler waiting on main-loop progress under mu deadlocks. That is why
// #574's resolve/compare/raise runs before submit and outside mu, and why the
// press that discovers a stale viewing identity is nacked rather than queued.
type ctlState struct {
	mu sync.Mutex

	// paneToWin maps a remote pane id to the remote window id holding it — how a
	// request carrying only @bridge_pane resolves to a window.
	paneToWin map[string]string
	// focus is the per-window echo-suppression state (focus.go).
	focus map[string]*focusState

	// Intents coalesce, so a burst of gestures in one window is one reconcile.
	wantWindows bool
	// wantLayout maps a window to the tiled layout a drag already sent it, or
	// "" for any other layout verb.
	wantLayout map[string]string
	// wantReseed names remote WINDOWS whose mirror needs a fresh screen —
	// respawn-pane/-window clear the remote screen, and control mode never
	// carries the clear, so the mirror keeps stale bytes until the pane is
	// re-seeded from the remote. Keyed by window, not pane: respawn-window
	// keeps the window's FIRST pane, which is not necessarily the active pane
	// the ctl request carried, so no pane id is trustworthy at command time.
	wantReseed map[string]bool
}

func newCtlState() *ctlState {
	return &ctlState{
		paneToWin:  map[string]string{},
		focus:      map[string]*focusState{},
		wantLayout: map[string]string{},
		wantReseed: map[string]bool{},
	}
}

// setWindowPanes records a window's remote pane order. The main loop calls it
// wherever it changes mirrorWindow.remotePanes, so the ctl side always has a
// snapshot it may read without touching mirrorWindow.
func (c *ctlState) setWindowPanes(remoteWin string, panes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, w := range c.paneToWin {
		if w == remoteWin {
			delete(c.paneToWin, p)
		}
	}
	for _, p := range panes {
		c.paneToWin[p] = remoteWin
	}
	if _, ok := c.focus[remoteWin]; !ok {
		c.focus[remoteWin] = &focusState{}
	}
}

// forgetWindow drops a closed window's pane mapping, focus state and pending
// intent, so a stale commanded entry cannot outlive its window.
func (c *ctlState) forgetWindow(remoteWin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, w := range c.paneToWin {
		if w == remoteWin {
			delete(c.paneToWin, p)
		}
	}
	delete(c.focus, remoteWin)
	delete(c.wantLayout, remoteWin)
	delete(c.wantReseed, remoteWin)
}

// takeIntents removes and returns the pending reconcile work. layouts maps
// each window with a pending layout reconcile to the sentLayout wantLayout
// held for it — see ctlState.wantLayout.
func (c *ctlState) takeIntents() (windows bool, layouts map[string]string, reseeds []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	windows = c.wantWindows
	c.wantWindows = false
	// settle drains per stream line, %output included: no allocation when idle.
	if len(c.wantLayout) > 0 {
		layouts = c.wantLayout
		c.wantLayout = map[string]string{}
	}
	for w := range c.wantReseed {
		reseeds = append(reseeds, w)
		delete(c.wantReseed, w)
	}
	return windows, layouts, reseeds
}

// ctlRequest is a decoded, validated FrameCtl: the remote commands to run and
// the reconcile they imply. Producing one mutates nothing and sends nothing.
type ctlRequest struct {
	cmds        []string   // control-mode command lines, in order
	local       [][]string // local tmux argv lists, run in order before cmds
	wantWindows bool
	wantLayout  string // remote window id, or "" for none
	// sentLayout is the tiled layout the local window already shows after a
	// drag, "" otherwise; see noteLocalLayout.
	sentLayout string
	// wantReseed names the remote window whose mirror needs a fresh screen
	// after this verb, or "" for none. See ctlState.wantReseed for why it is
	// window-scoped rather than a pane id.
	wantReseed string
	// invalidate names the window whose remote-active-pane belief this verb
	// makes unknowable (the new pane does not exist at command time).
	invalidate string
	// A focus request is filtered by the echo guards rather than run outright.
	focusPane string
	focusSeq  int64
	// needsView carries the verb's flag of the same name through to the
	// handler.
	needsView bool
	// probePane is the remote pane whose verdict stamp the handler must wait
	// on, or "" for a verb that reports nothing back (carouselprobe.go).
	probePane string
}

// verb is one entry of the fixed translation table. The daemon builds every
// remote command from this table and never forwards raw command text, even
// though the socket is 0600 same-user.
type verb struct {
	args int // trailing arguments after the pane id
	// optArgs is how many FURTHER trailing arguments the verb tolerates. Only
	// the tool verb has any: its cwd rides along when the bind can supply one
	// and is absent from an older local config, and a verb that refused the
	// shorter form would break a mirror mid-upgrade.
	optArgs int
	windows bool
	layout  bool
	// reseed marks a verb that clears the remote screen (respawn-*), so the
	// mirror must be re-seeded from the remote or it keeps stale bytes.
	reseed bool
	// moves is true when the verb implicitly changes the remote's current pane.
	moves bool
	// needsView marks a verb whose effect on the remote depends on WHICH
	// terminal this bridge's control client advertises, so it must not run
	// under a client that advertises a terminal the user is no longer looking
	// through (#574). A field here rather than a verb-name compare in
	// daemon.go: the translation table is otherwise the only thing that knows
	// what a verb means, and the handler asks the parsed request a typed
	// question instead.
	needsView bool
	// probe marks a verb whose remote command reports its outcome by stamping
	// carouselVerdictOpt rather than by changing anything the mirror can see,
	// so the daemon has to read that stamp back to say anything about it
	// (#593). A field here for needsView's reason: the table stays the only
	// thing that knows what a verb means.
	probe bool
	// localLayout marks a verb whose one argument is a tiled layout the local
	// window already shows, so the reconcile it schedules must not trust a
	// dedup key recorded before it (mirrorWindow.noteLocalLayout).
	localLayout bool
	build       func(pane, win, sess string, args []string) ([]string, error)
	// buildLocal, when set, returns local tmux argv lists the handler runs
	// before build's remote commands. Called only once build has accepted
	// the same args.
	buildLocal func(args []string) ([][]string, error)
}

// Whitelisted direction flags, so a request cannot smuggle an arbitrary option
// into a remote command line.
var (
	resizeDirs = map[string]string{"U": "-U", "D": "-D", "L": "-L", "R": "-R"}
	swapDirs   = map[string]string{"U": "-U", "D": "-D"}
	rotateDirs = map[string]string{"U": "-U", "D": "-D"}
	// The closed set of tools a bind may launch on the remote: a name reaches a
	// remote shell only by being a key here, so the socket peer cannot smuggle
	// one in.
	remoteTools  = map[string]bool{"prdash": true, "lazygit": true, "yazi": true}
	remoteThemes = map[string]bool{"dark": true, "light": true}
	// remoteToolFloat gives each remoteTools entry its float shape, matching
	// config/tmux.conf.nix's floatShort/floatFull mkFloat shapes byte for byte
	// so a bridged tool opens at the same geometry the purely-local bind uses.
	// Not itself an authorization list — remoteTools stays that; a tool present
	// there but absent here (should never happen, kept in sync by hand) falls
	// back to remoteFloatFull rather than build a flagless new-pane.
	remoteToolFloat = map[string]string{
		"prdash":  remoteFloatShort,
		"yazi":    remoteFloatShort,
		"lazygit": remoteFloatFull,
	}
)

// layoutCommand is one allow-listed entry of the layout verb: the remote
// command's own verb, plus a trailing preset name or flag (empty for the plain
// cycle commands). The pane target is spliced in at build time, so nothing the
// socket peer sends reaches the remote command line as free-form layout text.
type layoutCommand struct {
	verb string
	arg  string
}

// layoutCommands is the closed set of layout gestures a bind may ask for: the
// seven select-layout presets, the two cycle commands, and the -E spread.
var layoutCommands = map[string]layoutCommand{
	"even-horizontal":          {verb: "select-layout", arg: "even-horizontal"},
	"even-vertical":            {verb: "select-layout", arg: "even-vertical"},
	"main-horizontal":          {verb: "select-layout", arg: "main-horizontal"},
	"main-vertical":            {verb: "select-layout", arg: "main-vertical"},
	"tiled":                    {verb: "select-layout", arg: "tiled"},
	"main-horizontal-mirrored": {verb: "select-layout", arg: "main-horizontal-mirrored"},
	"main-vertical-mirrored":   {verb: "select-layout", arg: "main-vertical-mirrored"},
	"next":                     {verb: "next-layout"},
	"previous":                 {verb: "previous-layout"},
	"spread":                   {verb: "select-layout", arg: "-E"},
}

// remoteFloatShort and remoteFloatFull are config/tmux.conf.nix's
// floatShort/floatFull mkFloat shapes, always carrying -A: the local bind's
// version-guarded flagsNoA fallback exists only for a tmux old enough to lack
// new-pane -A, and every remote this daemon opens a ctl connection to is on
// this revision.
//
// -A is a z-order flag — "the floating pane remains visible above a zoomed
// pane" — and NOT attach-if-exists: new-pane has no such mode, so nothing here
// dedupes by itself. The reuse is explicit and keyed on @pane_label (#679).
const (
	remoteFloatShort = "-x 90% -y 85% -X 5% -Y 8% -B heavy -A"
	remoteFloatFull  = "-x 90% -y 90% -X 5% -Y 5% -B heavy -A"
)

// floatRegister is the window option a tool press hands the float's pane id to
// its own focus branch through. It is not a second lookup key: the value is
// written by the branch that reads it, in the same command list, and the branch
// is only reached once floatLookup has already matched — so it is never
// consulted across a press and can never be stale. It exists because a pane
// loop nested inside a run-shell argument is a shell-injection shape this repo
// guards against (tests/conf-shell-quoting.bats); `#{q:<option>}` is the one
// legal way to pass an id to a shell. Per tool, so a read can never name
// another tool's float even in principle.
func floatRegister(tool string) string { return "@og_float_target_" + tool }

// floatLookup is the pane loop behind one tool press: it expands to the pane id
// of the window's float carrying that tool's @pane_label, or to nothing when
// there is none (which is falsey in if-shell -F, so no #{?:} wrapper is
// needed). pane_floating_flag keeps the predicate honest — @pane_label is a
// border title, and a tiled pane wearing it is not the float to reuse.
//
// generator/render/keys.go builds the same expression for the local bind, kept
// in step by hand like the float geometry above: the two modules share no code.
func floatLookup(tool string) string {
	return fmt.Sprintf("#{P:#{?#{&&:#{==:#{@pane_label},%s},#{pane_floating_flag}},#{pane_id},}}", tool)
}

// floatGeomCommand puts remote float pane on inner box c. -X/-Y/-x/-y speak
// the outer box, whose inset is the remote float's own border (tmux reads a
// float's pane-border-lines from its pane options), so the inset is chosen
// remote-side. A pane re-tiled since the drag began is left alone.
func floatGeomCommand(pane string, c controlmode.PaneCell) string {
	none := fmt.Sprintf("move-pane -t %s -X %d -Y %d ; %s",
		pane, c.X, c.Y, floatResizeCmd(pane, c.W, c.H))
	bordered := fmt.Sprintf("move-pane -t %s -X %d -Y %d ; %s",
		pane, c.X-1, c.Y-1, floatResizeCmd(pane, c.W+2, c.H+2))
	inner := fmt.Sprintf("if-shell -t %s -F %s %s %s",
		pane, tmuxQuote("#{==:#{pane-border-lines},none}"), tmuxQuote(none), tmuxQuote(bordered))
	return fmt.Sprintf("if-shell -t %s -F %s %s", pane, tmuxQuote("#{pane_floating_flag}"), tmuxQuote(inner))
}

// tiledArg parses a tile-layout argument, a tiled layout already in remote
// pane ids. TiledLayout rebuilds Raw, so no wire text reaches the remote.
func tiledArg(s string) (controlmode.Layout, error) {
	return controlmode.TiledLayout(s, func(id string) (string, bool) { return id, true })
}

// tileLayoutCommand asks the remote to reshape pane's window to L, guarded so
// the command is a no-op unless the remote's tiled panes are still in the
// order L was built from, the window is still L's size, and it isn't zoomed.
//
// A v1 select-layout assigns tiled panes to cells positionally in the
// window's pane-LIST order, skipping floats (tmux layout_assign_fallback_tiled),
// so the guard must walk that same order: #{P/i:} does (SORT_INDEX), while a
// bare #{P:} sorts by pane id (SORT_CREATION) instead — measured after
// rotate-window on a three-pane window: pane list %2 %1 %3, #{P/i:} %2 %1 %3,
// #{P:} %1 %2 %3. The size guard exists because select-layout resizes the
// window to the string's own size, and the mirror window is fitted to the
// remote's rather than the other way round; the zoom guard because
// select-layout unzooms. A remote that fails any of these is left alone.
func tileLayoutCommand(pane string, L controlmode.Layout) string {
	var order strings.Builder
	for _, id := range RemotePaneOrder(L) {
		order.WriteString(id)
		order.WriteByte(' ')
	}
	cond := fmt.Sprintf(
		"#{&&:#{==:#{P/i:#{?pane_floating_flag,,#{pane_id} }},%s},#{&&:#{==:#{window_width}x#{window_height},%dx%d},#{==:#{window_zoomed_flag},0}}}",
		order.String(), L.W, L.H)
	inner := fmt.Sprintf("select-layout -t %s %s", pane, tmuxQuote(L.Raw))
	return fmt.Sprintf("if-shell -t %s -F %s %s", pane, tmuxQuote(cond), tmuxQuote(inner))
}

var localPaneRe = regexp.MustCompile(`^%[0-9]+$`)

// floatGeomArgs is a validated float-geom request: the local float that was
// dragged, the inner box it was dragged to, and the window it sits in.
type floatGeomArgs struct {
	localPane  string
	raw        controlmode.PaneCell
	winW, winH int
}

// parseFloatGeom validates float-geom's [local-pane x y w h winW winH] args.
func parseFloatGeom(a []string) (floatGeomArgs, error) {
	if !localPaneRe.MatchString(a[0]) {
		return floatGeomArgs{}, fmt.Errorf("float-geom: bad local pane %q", a[0])
	}
	spec := []struct {
		name   string
		lo, hi int
	}{
		{"x", -9999, 9999}, {"y", -9999, 9999},
		{"w", 1, 9999}, {"h", 1, 9999},
		{"winW", 3, 9999}, {"winH", 3, 9999},
	}
	vals := make([]int, len(spec))
	for i, s := range spec {
		n, err := strconv.Atoi(a[i+1])
		if err != nil || n < s.lo || n > s.hi {
			return floatGeomArgs{}, fmt.Errorf("float-geom: bad %s %q", s.name, a[i+1])
		}
		vals[i] = n
	}
	return floatGeomArgs{
		localPane: a[0],
		raw:       controlmode.PaneCell{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]},
		winW:      vals[4],
		winH:      vals[5],
	}, nil
}

var verbs = map[string]verb{
	// Splits carry the pane's cwd, matching the local bindings they replace.
	"split-h": {layout: true, moves: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("split-window -h -t %s -c '#{pane_current_path}'", pane)}, nil
	}},
	"split-v": {layout: true, moves: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("split-window -v -t %s -c '#{pane_current_path}'", pane)}, nil
	}},
	// Killing a pane can empty its window, so it needs the window reconcile too.
	"kill-pane": {layout: true, windows: true, moves: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("kill-pane -t %s", pane)}, nil
	}},
	// respawn-pane -k restarts the remote program in place (same pane id, same
	// active pane), so only the re-seed is owed — not a layout/moves reconcile.
	"respawn-pane": {reseed: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("respawn-pane -k -t %s", pane)}, nil
	}},
	// respawn-window -k destroys every remote pane but the window's FIRST
	// (spawn.c), so the layout reconcile drops the lost renderers and the first
	// pane becomes active (moves) — use the window-scoped re-seed, never a pane id.
	"respawn-window": {layout: true, moves: true, reseed: true, build: func(_, win, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("respawn-window -k -t %s", win)}, nil
	}},
	"resize": {args: 2, layout: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		dir, ok := resizeDirs[a[0]]
		if !ok {
			return nil, fmt.Errorf("resize: bad direction %q", a[0])
		}
		n, err := strconv.Atoi(a[1])
		if err != nil || n < 1 || n > 999 {
			return nil, fmt.Errorf("resize: bad amount %q", a[1])
		}
		return []string{fmt.Sprintf("resize-pane -t %s %s %d", pane, dir, n)}, nil
	}},
	// Zoom has to happen on the remote, because that is where the pane whose
	// size must change lives. A local resize-pane -Z does grow the renderer
	// pane, and it sticks — but the remote pane keeps its size, so the remote
	// program goes on rendering at the old one and the rows the pane gained are
	// dead space (measured: dst 150x39 against src 150x19), until some
	// unrelated remote layout change reconciles the zoom away. The
	// mirror learns the new flag from the layout reconcile, so a zoom made by
	// any other client on the remote follows too (verified): tmux emits
	// %layout-change for a zoom, even though #{window_layout} itself is
	// unchanged by one.
	"zoom": {layout: true, moves: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("resize-pane -Z -t %s", pane)}, nil
	}},
	// No -d: without it the active pane rides with the swap, so the remote keeps
	// the same pane focused (verified). The local reconcile swaps WITH -d, and
	// that pairing is what keeps both sides focused on the same remote pane.
	"swap": {args: 1, layout: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		dir, ok := swapDirs[a[0]]
		if !ok {
			return nil, fmt.Errorf("swap: bad direction %q", a[0])
		}
		return []string{fmt.Sprintf("swap-pane -t %s %s", pane, dir)}, nil
	}},
	// Layout goes to the remote for the same reason zoom does: a local
	// select-layout reshapes only the renderer panes, so the remote panes keep
	// their sizes, the programs in them render at the old ones, and the next
	// remote %layout-change reverts the local shape. The layout name is
	// allow-listed rather than forwarded: the socket peer may only pick a verb
	// and a target, never a free-form layout string.
	//
	// None of these presets, nor next/previous/spread, changes which pane id is
	// active (measured on next-3.9), so no `moves` — the daemon's active-pane
	// belief stays valid and the reconcile it schedules refreshes the geometry.
	"layout": {args: 1, layout: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		c, ok := layoutCommands[a[0]]
		if !ok {
			return nil, fmt.Errorf("layout: bad layout %q", a[0])
		}
		cmd := c.verb + " -t " + pane
		if c.arg != "" {
			cmd += " " + c.arg
		}
		return []string{cmd}, nil
	}},
	// rotate-window DOES change which pane id is active (measured: %2 -> %0 on
	// a three-pane window), so it carries `moves` and the daemon invalidates its
	// active-pane belief for the reconcile to re-learn. Without that the focus
	// echo guard would suppress a later focus command against a stale belief.
	"rotate": {optArgs: 1, layout: true, moves: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		dir := ""
		if len(a) == 1 {
			d, ok := rotateDirs[a[0]]
			if !ok {
				return nil, fmt.Errorf("rotate: bad direction %q", a[0])
			}
			dir = d + " "
		}
		return []string{fmt.Sprintf("rotate-window %s-t %s", dir, pane)}, nil
	}},
	// -t '<sess>:' is a session target with the index unspecified: tmux picks the
	// lowest free index and makes the new window active (verified), matching the
	// local new-window this replaces. Never a bare session name in a
	// target-window slot (CLAUDE.md, "Session Targeting Gotcha").
	"new-window": {windows: true, moves: true, build: func(_, _, sess string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("new-window -t %s: -c '#{pane_current_path}'", tmuxQuote(sess))}, nil
	}},
	"kill-window": {windows: true, moves: true, build: func(_, win, _ string, _ []string) ([]string, error) {
		return []string{fmt.Sprintf("kill-window -t %s", win)}, nil
	}},
	// The new name is re-read from the remote by the window reconcile rather than
	// written locally first: a remote rename can fail, and writing
	// @window_bridge_name optimistically is the local-first mutation the mirror
	// invariant forbids.
	"rename": {args: 1, windows: true, build: func(_, win, _ string, a []string) ([]string, error) {
		// The prompt prefills from @window_bridge_name, so the value handed back is
		// already in that option's escaped dialect — re-encoding it without decoding
		// first is a double-encode. The re-encode is still kept: the remote's own
		// rename-window format-expands its argument, so a user-typed 'a#(x)' must
		// not reach it bare.
		name := sanitizeWindowName(decodeWindowName(a[0]))
		if name == "" {
			return nil, fmt.Errorf("rename: empty name")
		}
		return []string{fmt.Sprintf("rename-window -t %s -- %s", win, tmuxQuote(name))}, nil
	}},
	// The carousel toggle opens its own split on the remote, so this verb only
	// launches it. TMUX_PANE is injected explicitly because run-shell does not
	// export it (the local bind this replaces does the same via #{pane_id}), and
	// AEYE_BRIDGED tells the viewer its frames cross a network, not a disk.
	// Backgrounded (-b) so a slow launch never blocks the remote command queue;
	// the split arrives as a %layout-change like any other structural event.
	//
	// Nothing the remote can say reaches the user directly: the only client
	// attached to it is this daemon's control client, which has no status line
	// for a display-message to land on. So neither a missing binary nor an
	// empty manifest opens anything there — the script stamps a verdict and
	// carouselProbe reads it back into a local message (#593).
	"carousel": {layout: true, moves: true, needsView: true, probe: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
		// show-options (not display-message -F "#{@…}"): run-shell expands #{}
		// before /bin/sh runs. Case arms omit an '' empty alternative — that
		// becomes a dense '\'' stack after double tmuxQuote and is not portable.
		script := carouselResolveScript(pane)
		// run-shell uses tmux's configured default shell (fish on the normal
		// host), while this validation is POSIX shell syntax. Quote each layer so
		// the remote pane id remains data all the way through both parsers.
		cmd := fmt.Sprintf("run-shell -b -t %s %s", pane, tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
		return []string{cmd}, nil
	}},
	// The tool binds (prefix p/g/G/y) open a float locally, and this is the
	// remote leg: it now opens a float on the remote too, at the same shape
	// (remoteToolFloat) the local bind uses, so the mirror can reconcile it
	// into a local float the same way it reconciles any other remote float.
	// The cwd comes from the mirror window's @bridge_dir, passed by the bind:
	// tmux expands a -c format against the CLIENT'S CURRENT pane, not against
	// -t (measured on next-3.8 — new-pane and split-window alike), so asking
	// the remote to expand #{pane_current_path} opens the tool in whichever
	// window the remote happens to be on rather than the one pressed in
	// (#643). A remote window carrying neither @worktree nor @git_root sends
	// nothing and keeps that expansion, which is no worse than it ever was.
	//
	// A bare command name, never the local ${tool}/bin/tool store path, which
	// exists on this host only. A remote missing the tool degrades to a
	// short-lived message pane — unlike carousel, which reports its own
	// failures as a local status message (#593): this verb is asked to open a
	// float either way, so the message rides the float the press already
	// implies rather than needing a verdict read back for it.
	//
	// Never stamps @float_geom on the remote pane: that option is read by the
	// remote's own tmux-float-refit, which would then fight the mirror for
	// authority over this float's geometry on the remote's next resize.
	//
	// The press is a reuse gate, not a bare new-pane (#679): a second press for
	// a tool whose float is already open focuses that float instead of stacking
	// another one at the same geometry. The create branch's @pane_label stamp is
	// what makes the next press's floatLookup find it — this verb used to leave
	// the remote float unlabelled, so there was nothing to look up.
	//
	// Every command in the branch carries its own -t. if-shell -t pins the
	// CONDITION's context but not the branch's (measured: with the current
	// window short-circuited to one holding no float, a bare run-shell in the
	// branch expanded the loop against that window), and the remote's current
	// window is not the window the press came from.
	//
	// `;`, never `\;`, separates the branch's two commands. This string reaches
	// tmux as a command LINE (cmd_parse_from_string, the same parser a config
	// file goes through), where `\;` is a literal semicolon argument that leaves
	// the pane uncreated — measured against a real control-mode client.
	"tool": {args: 1, optArgs: 1, layout: true, moves: true, build: func(pane, win, _ string, a []string) ([]string, error) {
		if !remoteTools[a[0]] {
			return nil, fmt.Errorf("tool: unknown tool %q", a[0])
		}
		tool := a[0]
		flags, ok := remoteToolFloat[tool]
		if !ok {
			flags = remoteFloatFull
		}
		cwd := "'#{pane_current_path}'"
		if len(a) > 1 {
			if dir, ok := remoteToolCwd(a[1]); ok {
				cwd = tmuxQuote(dir)
			}
		}
		script := toolResolveScript(tool)
		// -t <win>, not a bare set -p: the new pane is the window's active one,
		// and this must not depend on which window the control client is on.
		create := fmt.Sprintf("new-pane -t %s -c %s %s %s ; set -p -t %s @pane_label %s",
			pane, cwd, flags, tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)), win, tool)
		loop := floatLookup(tool)
		focus := fmt.Sprintf("set -wF -t %s %s %s ; run-shell -t %s %s",
			pane, floatRegister(tool), tmuxQuote(loop),
			pane, tmuxQuote(fmt.Sprintf("tmux select-pane -t #{q:%s}", floatRegister(tool))))
		cmd := fmt.Sprintf("if-shell -t %s -F %s %s %s",
			pane, tmuxQuote(loop), tmuxQuote(focus), tmuxQuote(create))
		return []string{cmd}, nil
	}},
	// A mirror float's border drag, routed from the local float's inner box
	// and window size (the remote window is converged to the same size). The
	// box is clamped with the reconcile's own rule. The reconcile moves the
	// local float only when the REMOTE float changes, so when the clamp moved
	// the box, buildLocal puts the local float there itself: a flush float
	// dragged past its edge is a remote no-op and would stay off screen.
	// x/y may be negative: a float dragged partly off screen reports a
	// negative pane_left (measured -5). Not `moves`: remote move-pane and
	// resize-pane keep the active pane.
	"float-geom": {args: 7, layout: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		g, err := parseFloatGeom(a)
		if err != nil {
			return nil, err
		}
		return []string{floatGeomCommand(pane, clampInner(g.raw, g.winW, g.winH))}, nil
	}, buildLocal: func(a []string) ([][]string, error) {
		g, err := parseFloatGeom(a)
		if err != nil {
			return nil, err
		}
		inner := clampInner(g.raw, g.winW, g.winH)
		if inner == g.raw {
			return nil, nil
		}
		return [][]string{
			floatResizeArgv(g.localPane, inner, g.winW, g.winH),
			floatMoveArgv(g.localPane, inner, g.winW, g.winH),
		}, nil
	}},
	// A tiled mirror border drag (#823), routed from ctl drag with the local
	// window's tiled layout mapped to remote ids; the string is re-parsed and
	// rebuilt so raw text is never forwarded; floats untouched (v1 string,
	// tmux preserves floating cells for version 1); not `moves` — select-layout
	// keeps the active pane.
	"tile-layout": {args: 1, layout: true, localLayout: true, build: func(pane, _, _ string, a []string) ([]string, error) {
		L, err := tiledArg(a[0])
		if err != nil {
			return nil, err
		}
		return []string{tileLayoutCommand(pane, L)}, nil
	}},
	// A mirror's pane content is bytes the remote's programs coloured from the
	// remote's own theme state, so a local toggle cannot reach it: this asks the
	// remote for the same theme.
	//
	// run-shell, not a split: nothing should appear on screen. With -t, tmux
	// prints a job's stdout and, on a non-zero exit, a "returned N" line in view
	// mode on that pane, and a view-mode overlay on a mirrored pane wedges the
	// mirror — so the body discards theme-toggle's output and always exits 0.
	// A remote without theme-toggle (any headless host — it ships from the
	// desktop profile) is therefore silent per call — Run()'s one-shot
	// themeToggleAvailable probe (daemon.go) is what reports the absence, once
	// per bridge connect rather than once per toggle, since this fire-and-forget
	// verb never learns its own exit status (#545).
	"theme": {args: 1, build: func(pane, _, _ string, a []string) ([]string, error) {
		if !remoteThemes[a[0]] {
			return nil, fmt.Errorf("theme: unknown theme %q", a[0])
		}
		script := themeApplyScript(a[0])
		cmd := fmt.Sprintf("run-shell -b -t %s %s", pane, tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
		return []string{cmd}, nil
	}},
	// The enrich card's [r] in a mirror window: a local refresh cannot reach the
	// values the card displays, since the remote's own tmux-pr-enrich runs
	// against the remote's checkout, not this daemon's. This asks the remote to
	// poll its own window on demand instead of waiting out the poller's own
	// prRefreshSeconds/prCheckRefreshSeconds interval.
	//
	// No windows/layout/moves: the poller opens nothing and moves nothing, and
	// its result comes home as a %subscription-changed label row, never a
	// reconcile. No needsView/probe either — it depends on no terminal identity
	// and reports through that row rather than a stamp read back.
	"enrich-refresh": {build: func(pane, win, _ string, _ []string) ([]string, error) {
		script := enrichRefreshScript(win)
		cmd := fmt.Sprintf("run-shell -b -t %s %s", pane, tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
		return []string{cmd}, nil
	}},
}

// themeApplyScript is the POSIX body run under exec /bin/sh -c. Like the two
// scripts below it must contain zero single-quote characters so double tmuxQuote
// only wraps. theme is a remoteThemes key, so it needs no quoting of its own.
func themeApplyScript(theme string) string {
	return fmt.Sprintf("command -v theme-toggle >/dev/null 2>&1 || exit 0; theme-toggle apply %s >/dev/null 2>&1; exit 0", theme)
}

// themeProbeCmd is the display-message line Run() sends once per bridge
// connect to tell "no theme-toggle here" apart from "applied" (#545).
//
// Deliberately not a run-shell, unlike every other capability probe in this
// file: run-shell without -b shows its output ON the target pane (a view-mode
// overlay), and that pane is the one this daemon is about to mirror — the
// overlay wedges it, so %pause/%continue never fires again and live output
// stops forever. display-message -p -t sess "#(...)" runs the same POSIX body
// as a tmux job and returns its stdout in the command's own reply without
// touching any pane. Tradeoff: a #() job populates asynchronously, so the
// first read of a fresh job string can come back empty before it has run once
// — themeToggleAvailable (themeprobe.go) retries for exactly that.
func themeProbeCmd(sess string) string {
	script := "command -v theme-toggle >/dev/null 2>&1 && echo yes || echo no"
	job := "#(/bin/sh -c " + tmuxQuote(script) + ")"
	return fmt.Sprintf("display-message -p -t %s %s", tmuxQuote(sess), tmuxQuote(job))
}

// remoteToolCwd accepts the directory the tool bind read off the mirror
// window's @bridge_dir, or reports false to leave the cwd alone.
//
// The value is remote-derived and arrives over the ctl socket, and new-pane
// format-expands its -c argument — so a '#' is not a character in a path here,
// it is the start of a format, and '#(...)' would be a command the remote runs.
// A control byte would end the command line the daemon writes to the control
// stream and start another. Both drop the value whole rather than being
// scrubbed out of it: a repaired path names a directory that is not the one on
// screen, which is the class of wrong cleanLabelValueExact already refuses.
func remoteToolCwd(dir string) (string, bool) {
	if !strings.HasPrefix(dir, "/") || len(dir) > maxRemoteToolCwd {
		return "", false
	}
	for _, r := range dir {
		if r == '#' || r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return dir, true
}

// maxRemoteToolCwd is PATH_MAX: a longer value is not a path this remote could
// have been sitting in.
const maxRemoteToolCwd = 4096

// toolResolveScript is the POSIX body run under exec /bin/sh -c: split-window
// runs its command through the remote's default shell (fish on the normal host),
// and this is POSIX. Like carouselResolveScript it must contain zero single-quote
// characters so double tmuxQuote only wraps. tool is a remoteTools key, so it is
// [a-z-] and needs no quoting of its own.
//
// The PATH restore is what makes the bare name resolvable at all. tmux hands a
// new pane its global environ, which carries every store path tmux-og's wrapper
// prepended — but split-window spawns through default-shell, and fish on NixOS
// rebuilds PATH from the login profile instead of inheriting it (measured on the
// remote: 67 entries in, 10 out). A tool that reaches the remote tmux only
// through the wrapper is then invisible; prdash and tmux-gh-dash are exactly
// that, while lazygit and yazi survive only by also being in the user profile.
//
// Prepended, not replaced, and guarded on a non-empty PATH= line: an unset
// variable prints nothing and exits 1, and blindly assigning that leaves the
// pane with no PATH at all — not even the sleep below would resolve, so the
// fallback would flash shut instead of showing its message.
//
// The trim is ${p#*=}, never ${p#PATH=}: split-window does NOT format-expand its
// shell-command (measured, next-3.8 — #P and #{pane_id} arrive byte-identical),
// but run-shell DOES, and there #P expands to the pane index, corrupting that
// spelling to ${p1ATH=}. #* passes through both. Keep this body #*-only so it
// stays correct if it ever moves near a run-shell verb, or if a tmux bump
// extends expansion to split-window.
//
// The BROWSER restore right after the PATH one guards the same seam: fish's
// login-profile rebuild may export its own BROWSER, which would shadow the
// tmux global BROWSER=<og-open path> and silently disable URL forwarding for
// this tool.
func toolResolveScript(tool string) string {
	return fmt.Sprintf(
		"p=$(tmux show-environment -g PATH 2>/dev/null); "+
			"case $p in PATH=?*) PATH=${p#*=}:$PATH; export PATH;; esac; "+
			"b=$(tmux show-environment -g BROWSER 2>/dev/null); "+
			"case $b in BROWSER=?*) BROWSER=${b#*=}; export BROWSER;; esac; "+
			"command -v %s >/dev/null 2>&1 && exec %s; "+
			"echo tmux-og: %s is not on PATH on this host; sleep 5",
		tool, tool, tool)
}

// carouselResolveScript is the POSIX body run under exec /bin/sh -c. It must
// contain zero single-quote characters so double tmuxQuote only wraps.
//
// An empty/absent manifest is checked here rather than left to
// tmux-claude-images' own `[[ ! -s $MANIFEST ]]` branch: that branch's
// `tmux display-message` lands on the control-mode client's nonexistent
// status line and evaporates — so this uses `tmux-claude-images --resolve`
// (prints MODE\tKEY\tMANIFEST, launches nothing) to test the manifest itself.
//
// Every outcome is reported by stamping carouselVerdictOpt on the ctl pane and
// nothing else: the daemon reads it back and shows the local one-line message
// the purely-local bind shows (#593). It used to open a 90%x90% float here
// instead, which mirrors home as a bordered pane that steals focus for five
// seconds to say one sentence.
//
// The stamp lands on the ctl pane, never on $src: $src is the pane whose
// IMAGES these are (@claude_img_src can point elsewhere), while the ctl pane
// is the only id the daemon knows to read back. Cleared at entry so a
// previous press cannot be read as this one's answer; the daemon unsets it
// again on read, which covers a press whose verdict arrived after the probe
// had given up.
//
// manifest=${res####*$tab}, not ${res##*$tab}: run-shell format-expands its
// whole command string before /bin/sh ever sees it, and a run of `#`
// characters is that format language's own escape for a literal `#` — two
// collapse to one (measured, next-3.8: `##` arrives as `#`). Writing the
// POSIX "strip longest match" operator ## therefore requires doubling every
// `#` in it, i.e. four, so the shell that actually runs still sees `##`.
func carouselResolveScript(pane string) string {
	return fmt.Sprintf(
		"%s; "+
			"src=$(tmux show-options -pqv -t %s @claude_img_src); "+
			"case \"$src\" in %%[0-9]*) case \"${src#%%}\" in *[!0-9]*) src=%s;; esac;; *) src=%s;; esac; "+
			// @carousel_bin repoints on a config reload; PATH is frozen at
			// server start, so a bare name would keep resolving to the old
			// generation's script after a nix switch (#554). The -x guard
			// drops a stamp whose store path was garbage-collected.
			"bin=$(tmux show-options -gqv @carousel_bin); "+
			"if [ -n \"$bin\" ] && [ ! -x \"$bin\" ]; then bin=; fi; "+
			"if [ -z \"$bin\" ]; then bin=$(command -v tmux-claude-images 2>/dev/null); fi; "+
			"if [ -z \"$bin\" ]; then %s; exit 0; fi; "+
			"tab=$(printf \"\\t\"); "+
			"res=$(env TMUX_PANE=\"$src\" \"$bin\" --resolve 2>/dev/null); "+
			"manifest=${res####*$tab}; "+
			"if [ -n \"$manifest\" ] && [ -s \"$manifest\" ]; then "+
			// Stamped before the exec, so a press that launched is an answer
			// too: the probe stops on it rather than burning its whole retry
			// budget on the common case.
			"%s; "+
			"exec env TMUX_PANE=\"$src\" AEYE_BRIDGED=1 \"$bin\"; "+
			"fi; "+
			"%s",
		"tmux "+carouselClearCmd(pane),
		pane, pane, pane,
		"tmux "+carouselStampCmd(pane, carouselVerdictNoBin),
		"tmux "+carouselStampCmd(pane, carouselVerdictOK),
		"tmux "+carouselStampCmd(pane, carouselVerdictNoImages),
	)
}

// enrichRefreshScript is the POSIX body run under exec /bin/sh -c. Like its two
// siblings above it must contain zero single-quote characters so double
// tmuxQuote only wraps, and it restores PATH from the global environment for
// toolResolveScript's measured reason — run-shell spawns through the remote's
// default-shell, which on NixOS rebuilds PATH from the login profile, and
// tmux-pr-enrich reaches the remote tmux only through tmux-og's wrapper.
// Parameter expansion is spelled ${p#*=} for that same reason: run-shell
// format-expands the whole string first, where #P would become the pane index.
// That is the body's ONLY literal #, and it survives because #* is not one of
// tmux's format codes; any # that would be read as one has to be doubled.
//
// Both resolved values are guarded before the poller runs, and neither empty is
// a harmless no-op. An empty branch falls THROUGH the poller's single-target
// guard (tmux-pr-enrich.sh:463) into tick mode, where --force skips the age gate
// outright, touches the remote's .last-tick and detaches a whole-server
// run_full_pass (:405) — one keypress enriching every window on that host and
// resetting its tick clock. An empty dir keeps single-target mode but skips the
// conditional cd (:234), so gh pr list --head runs in the remote tmux server's
// cwd; if that happens to sit in a repo, that repo's PR for the branch name is
// written to the remote window's own @pr_* and ships home as @bridge_pr_* — a
// wrong value rather than an absent one. The card's local [r] no-branch footer
// gate does not cover either: it reads the value carried home, on a different
// host, and never gates dir at all.
//
// The target is win, never <sess>:<win>. Config.RemoteSession may contain spaces
// (daemon.go), and quoting it through run-shell's expansion, two tmuxQuote
// layers and sh needs exactly the single quotes this body bans; win is a remote
// window id (@N), a complete target-window on its own.
func enrichRefreshScript(win string) string {
	return fmt.Sprintf(
		"p=$(tmux show-environment -g PATH 2>/dev/null); "+
			"case $p in PATH=?*) PATH=${p#*=}:$PATH; export PATH;; esac; "+
			"b=$(tmux show-options -wqv -t %s @branch); "+
			"d=$(tmux show-options -wqv -t %s @worktree); "+
			"[ -n \"$d\" ] || d=$(tmux show-options -wqv -t %s @git_root); "+
			"[ -n \"$b\" ] && [ -n \"$d\" ] || exit 0; "+
			"exec tmux-pr-enrich --target %s --branch \"$b\" --dir \"$d\" --force",
		win, win, win, win)
}

// parseCtl validates a FrameCtl argv and resolves it against the current
// pane->window mapping. argv is [version, verb, pane, args...]; the pane rides
// on every verb because @bridge_pane is the only id a keybind has, and the
// window is derived from it.
func (c *ctlState) parseCtl(argv []string, sess string) (ctlRequest, error) {
	if len(argv) < 3 {
		return ctlRequest{}, fmt.Errorf("want at least version, verb and pane, got %d fields", len(argv))
	}
	if argv[0] != wire.CtlProtocolVersion {
		return ctlRequest{}, fmt.Errorf("ctl protocol version %q, this daemon speaks %q — reopen the bridge", argv[0], wire.CtlProtocolVersion)
	}
	name, pane, args := argv[1], argv[2], argv[3:]
	// This probe deliberately resolves before pane lookup: a new ctl can tell a
	// live v2 daemon from a pre-carousel v1 daemon without needing a mirrored
	// pane. The latter rejects its v2 version before reaching this branch.
	if name == "ping" {
		if len(args) != 0 {
			return ctlRequest{}, fmt.Errorf("ping wants no arguments")
		}
		return ctlRequest{}, nil
	}

	c.mu.Lock()
	win, known := c.paneToWin[pane]
	c.mu.Unlock()
	if !known {
		return ctlRequest{}, fmt.Errorf("%s: pane %s is not mirrored by this bridge", name, pane)
	}

	if name == "focus" {
		if len(args) != 1 {
			return ctlRequest{}, fmt.Errorf("focus wants one sequence argument")
		}
		seq, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return ctlRequest{}, fmt.Errorf("focus: bad sequence %q", args[0])
		}
		return ctlRequest{focusPane: pane, focusSeq: seq, invalidate: win}, nil
	}

	v, ok := verbs[name]
	if !ok {
		return ctlRequest{}, fmt.Errorf("unknown verb %q", name)
	}
	if len(args) < v.args || len(args) > v.args+v.optArgs {
		return ctlRequest{}, fmt.Errorf("%s wants %d argument(s), got %d", name, v.args, len(args))
	}
	cmds, err := v.build(pane, win, sess, args)
	if err != nil {
		return ctlRequest{}, err
	}
	req := ctlRequest{cmds: cmds, wantWindows: v.windows, needsView: v.needsView}
	if v.buildLocal != nil {
		if req.local, err = v.buildLocal(args); err != nil {
			return ctlRequest{}, err
		}
	}
	if v.probe {
		req.probePane = pane
	}
	if v.localLayout {
		L, err := tiledArg(args[0])
		if err != nil {
			return ctlRequest{}, err
		}
		req.sentLayout = L.Raw
	}
	if v.layout {
		req.wantLayout = win
	}
	if v.reseed {
		req.wantReseed = win
	}
	if v.moves {
		req.invalidate = win
	}
	return req, nil
}

// submit runs a parsed request: it registers the reconcile intent and writes the
// remote command inside one critical section, so any drain that happens after the
// command was sent necessarily happens after the intent was registered.
//
// It reports whether the command was actually written. send is a silent no-op
// once the daemon is tearing down, and a request that loses that race must not be
// acked as accepted — the keybind would report success for a gesture that never
// happened.
func (c *ctlState) submit(req ctlRequest, send func(...string) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if req.focusPane != "" {
		cmd, ok := c.planFocusLocked(req.invalidate, req.focusPane, req.focusSeq)
		if !ok {
			return true // suppressed by the echo guards; nothing to send is success
		}
		return send(cmd)
	}

	if req.wantWindows {
		c.wantWindows = true
	}
	if req.wantLayout != "" {
		// A drag's sentLayout wins over a plain layout verb coalescing into the
		// same window either way round — the local window shows the last one —
		// while "" from a plain verb must never overwrite a pending drag.
		if req.sentLayout != "" {
			c.wantLayout[req.wantLayout] = req.sentLayout
		} else if _, ok := c.wantLayout[req.wantLayout]; !ok {
			c.wantLayout[req.wantLayout] = ""
		}
	}
	if req.wantReseed != "" {
		c.wantReseed[req.wantReseed] = true
	}
	// A verb that implicitly moves the remote's current pane leaves the belief
	// unknowable at command time (the new pane does not exist yet), so invalidate
	// rather than guess: the empty sentinel can never equal a real pane id, so it
	// never suppresses a later focus command. The reconcile the verb schedules
	// re-learns the truth from readLayout.
	if req.invalidate != "" {
		if f := c.focus[req.invalidate]; f != nil {
			f.remoteActivePane = ""
		}
	}
	for _, cmd := range req.cmds {
		if !send(cmd) {
			return false
		}
	}
	return true
}

// pressAgainErr is the nack a gesture gets while the bridge re-dials for a new
// viewing identity (#574). User-facing: the ctl client shows a request failure
// with display-message -d 5000, so this is a short status-line string, not a
// log line — and deliberately not submit's generic "bridge has no live
// connection to the remote", which reads as "your bridge is broken" when the
// truth is "your viewer identity is being updated" (R6).
func pressAgainErr(term string) error {
	return fmt.Errorf("re-dialling for %s — press again", term)
}

// handleCtl is the whole body of Run's acceptConns callback, package-level so
// a test can drive the ordering below without a live Run: the loops that act
// on a raise are closures over Run's locals, and Run has exactly one caller.
//
// The resolve/compare/raise runs before submit, and therefore outside
// ctlState.mu, for the deadlock reason ctlState's lock-order note gives. Both
// raise verdicts that nack come back through one call site, so the discovering
// press and a press landing mid-flight cannot drift apart (R8), and the nack
// is returned when the replacement is RAISED, not when it completes: a message
// arriving after a multi-second dial reads as a timeout rather than an
// instruction.
func handleCtl(cst *ctlState, rep *viewReplacer, probe *carouselProbe, argv []string, sess string, local func(...string) error, send func(...string) bool) error {
	req, err := cst.parseCtl(argv, sess)
	if err != nil {
		return err
	}
	if req.needsView {
		if v, term := rep.raise(); v != raiseNone {
			return pressAgainErr(term)
		}
	}
	// Before the submit: a reconcile the remote command triggers must be the
	// last word on the local float, never overwritten by this apply.
	for _, cmd := range req.local {
		if err := local(cmd...); err != nil {
			return fmt.Errorf("local %s: %w", cmd[0], err)
		}
	}
	if !cst.submit(req, send) {
		return fmt.Errorf("bridge has no live connection to the remote")
	}
	// After the submit, never before: a press whose command was not written
	// has no verdict coming, and arming for it would spend the whole retry
	// budget reading an option nothing is going to stamp.
	if req.probePane != "" {
		probe.arm(req.probePane)
	}
	return nil
}
