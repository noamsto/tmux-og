package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// newCtlStateWith returns a state that already mirrors one window's panes, as
// the main loop would have recorded it.
func newCtlStateWith(win string, panes ...string) *ctlState {
	c := newCtlState()
	c.setWindowPanes(win, panes)
	return c
}

// wantFloatGeom builds the exact command floatGeomCommand builds, from the
// plain x/y/w/h a float-geom argv carries rather than by calling the
// production helper, so the test pins the wire shape independently of it.
func wantFloatGeom(pane string, x, y, w, h int) string {
	none := fmt.Sprintf("move-pane -t %s -X %d -Y %d ; %s", pane, x, y, wantFloatResize(pane, w, h))
	bordered := fmt.Sprintf("move-pane -t %s -X %d -Y %d ; %s", pane, x-1, y-1, wantFloatResize(pane, w+2, h+2))
	inner := fmt.Sprintf("if-shell -t %s -F %s %s %s",
		pane, tmuxQuote("#{==:#{pane-border-lines},none}"), tmuxQuote(none), tmuxQuote(bordered))
	return fmt.Sprintf("if-shell -t %s -F %s %s", pane, tmuxQuote("#{pane_floating_flag}"), tmuxQuote(inner))
}

// wantFloatResize builds floatResizeCmd's text by hand, for the same reason.
func wantFloatResize(pane string, w, h int) string {
	return fmt.Sprintf("if-shell -t %s -F %s %s %s", pane, tmuxQuote(wantResizeTrigger),
		tmuxQuote(fmt.Sprintf("resize-pane -t %s -x %d -y %d", pane, w, h-1)),
		tmuxQuote(fmt.Sprintf("resize-pane -t %s -x %d -y %d", pane, w, h)))
}

// wantTileLayout builds tile-layout's exact if-shell command from the plain
// order/w/h/raw a tiledArg result carries, rather than by calling
// tileLayoutCommand, so the test pins the wire shape independently of it.
func wantTileLayout(pane string, order []string, w, h int, raw string) string {
	var ids strings.Builder
	for _, id := range order {
		ids.WriteString(id)
		ids.WriteByte(' ')
	}
	cond := fmt.Sprintf("#{&&:#{==:#{P/i:#{?pane_floating_flag,,#{pane_id} }},%s},#{&&:#{==:#{window_width}x#{window_height},%dx%d},#{==:#{window_zoomed_flag},0}}}",
		ids.String(), w, h)
	inner := fmt.Sprintf("select-layout -t %s %s", pane, tmuxQuote(raw))
	return fmt.Sprintf("if-shell -t %s -F %s %s", pane, tmuxQuote(cond), tmuxQuote(inner))
}

// wantLocalFloatResize is floatResizeArgv's argv for an outer w x h box.
func wantLocalFloatResize(pane string, w, h int) []string {
	return []string{"if-shell", "-t", pane, "-F", "#{pane_floating_flag}", wantFloatResize(pane, w, h)}
}

func TestParseCtlVerbTranslation(t *testing.T) {
	const sess = "my proj"
	tests := []struct {
		name    string
		argv    []string
		want    []string
		local   [][]string
		windows bool
		layout  string
		reseed  string
	}{
		{
			name:   "split -h carries the pane cwd and targets the pane by id",
			argv:   []string{wire.CtlProtocolVersion, "split-h", "%3"},
			want:   []string{"split-window -h -t %3 -c '#{pane_current_path}'"},
			layout: "@1",
		},
		{
			name:   "split -v",
			argv:   []string{wire.CtlProtocolVersion, "split-v", "%3"},
			want:   []string{"split-window -v -t %3 -c '#{pane_current_path}'"},
			layout: "@1",
		},
		{
			// Killing a pane can empty its window, so it needs both reconciles.
			name:    "kill-pane wants both reconciles",
			argv:    []string{wire.CtlProtocolVersion, "kill-pane", "%3"},
			want:    []string{"kill-pane -t %3"},
			windows: true,
			layout:  "@1",
		},
		{
			name:   "resize maps the direction and amount",
			argv:   []string{wire.CtlProtocolVersion, "resize", "%3", "U", "5"},
			want:   []string{"resize-pane -t %3 -U 5"},
			layout: "@1",
		},
		{
			// Zoom is the remote's, not the local renderer pane's; the mirror picks
			// the flag up from the layout reconcile this schedules.
			name:   "zoom toggles on the remote pane",
			argv:   []string{wire.CtlProtocolVersion, "zoom", "%3"},
			want:   []string{"resize-pane -Z -t %3"},
			layout: "@1",
		},
		{
			// No -d: the remote keeps the same pane active, which is what lets the
			// local reconcile's -d swap agree with it.
			name:   "swap sends no -d",
			argv:   []string{wire.CtlProtocolVersion, "swap", "%3", "U"},
			want:   []string{"swap-pane -t %3 -U"},
			layout: "@1",
		},
		{
			// A session target with the index unspecified, quoted because the name
			// has a space — never a bare name in a target-window slot.
			name:    "new-window targets the session, quoted",
			argv:    []string{wire.CtlProtocolVersion, "new-window", "%3"},
			want:    []string{"new-window -t 'my proj': -c '#{pane_current_path}'"},
			windows: true,
		},
		{
			name:    "kill-window resolves the pane's window",
			argv:    []string{wire.CtlProtocolVersion, "kill-window", "%3"},
			want:    []string{"kill-window -t @1"},
			windows: true,
		},
		{
			name:    "rename quotes the new name",
			argv:    []string{wire.CtlProtocolVersion, "rename", "%3", "my new name"},
			want:    []string{"rename-window -t @1 -- 'my new name'"},
			windows: true,
		},
		{
			// A name that would break the reflow delimiter or a command line is
			// sanitized before it reaches the remote.
			name:    "rename strips a pipe and control characters",
			argv:    []string{wire.CtlProtocolVersion, "rename", "%3", "a|b\nc"},
			want:    []string{"rename-window -t @1 -- 'abc'"},
			windows: true,
		},
		{
			name:    "rename escapes an embedded quote",
			argv:    []string{wire.CtlProtocolVersion, "rename", "%3", "it's"},
			want:    []string{`rename-window -t @1 -- 'it'\''s'`},
			windows: true,
		},
		{
			// The poller runs against the pane's own window (@1), never
			// <sess>:<win> — sess here has a space, which this body cannot
			// quote. No reconcile intent: nothing opens on the remote, and the
			// result comes home as a %subscription-changed label row.
			name: "enrich-refresh targets the pane's window, not the session",
			argv: []string{wire.CtlProtocolVersion, "enrich-refresh", "%3"},
			want: []string{fmt.Sprintf("run-shell -b -t %%3 %s",
				tmuxQuote("exec /bin/sh -c "+tmuxQuote(enrichRefreshScript("@1"))))},
		},
		{
			// The seven preset names, allow-listed on the daemon side so the
			// socket peer can never name an arbitrary layout.
			name:   "layout even-horizontal is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "even-horizontal"},
			want:   []string{"select-layout -t %3 even-horizontal"},
			layout: "@1",
		},
		{
			name:   "layout even-vertical is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "even-vertical"},
			want:   []string{"select-layout -t %3 even-vertical"},
			layout: "@1",
		},
		{
			name:   "layout main-horizontal is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "main-horizontal"},
			want:   []string{"select-layout -t %3 main-horizontal"},
			layout: "@1",
		},
		{
			name:   "layout main-vertical is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "main-vertical"},
			want:   []string{"select-layout -t %3 main-vertical"},
			layout: "@1",
		},
		{
			name:   "layout tiled is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "tiled"},
			want:   []string{"select-layout -t %3 tiled"},
			layout: "@1",
		},
		{
			name:   "layout main-horizontal-mirrored is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "main-horizontal-mirrored"},
			want:   []string{"select-layout -t %3 main-horizontal-mirrored"},
			layout: "@1",
		},
		{
			name:   "layout main-vertical-mirrored is a preset select-layout",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "main-vertical-mirrored"},
			want:   []string{"select-layout -t %3 main-vertical-mirrored"},
			layout: "@1",
		},
		{
			// Space's stock next-layout has no layout name and preserves the
			// active pane, so it is a distinct command behind the same verb.
			name:   "layout next is the next-layout command",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "next"},
			want:   []string{"next-layout -t %3"},
			layout: "@1",
		},
		{
			name:   "layout previous is the previous-layout command",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "previous"},
			want:   []string{"previous-layout -t %3"},
			layout: "@1",
		},
		{
			// E's stock spread. select-layout -E takes no layout name.
			name:   "layout spread is select-layout -E",
			argv:   []string{wire.CtlProtocolVersion, "layout", "%3", "spread"},
			want:   []string{"select-layout -t %3 -E"},
			layout: "@1",
		},
		{
			// rotate-window with no direction flag: the stock C-o command.
			name:   "rotate defaults to the stock rotate-window",
			argv:   []string{wire.CtlProtocolVersion, "rotate", "%3"},
			want:   []string{"rotate-window -t %3"},
			layout: "@1",
		},
		{
			name:   "rotate U maps to -U",
			argv:   []string{wire.CtlProtocolVersion, "rotate", "%3", "U"},
			want:   []string{"rotate-window -U -t %3"},
			layout: "@1",
		},
		{
			// M-o's stock rotate-window -D.
			name:   "rotate D maps to -D",
			argv:   []string{wire.CtlProtocolVersion, "rotate", "%3", "D"},
			want:   []string{"rotate-window -D -t %3"},
			layout: "@1",
		},
		{
			name:   "respawn-pane restarts the remote pane and asks for a re-seed",
			argv:   []string{wire.CtlProtocolVersion, "respawn-pane", "%3"},
			want:   []string{"respawn-pane -k -t %3"},
			reseed: "@1",
		},
		{
			// respawn-window destroys every remote pane but the FIRST, so it
			// re-seeds the window, reconciles the layout, and moves the active pane.
			name:   "respawn-window restarts the remote window with a layout reconcile and a re-seed",
			argv:   []string{wire.CtlProtocolVersion, "respawn-window", "%3"},
			want:   []string{"respawn-window -k -t @1"},
			layout: "@1",
			reseed: "@1",
		},
		{
			// An in-window box: clampInner round-trips it, so the branches carry
			// the request's own x/y/w/h (none) and their +/-1 border variants
			// (bordered), and the local float is already where it belongs.
			name:   "float-geom builds the nested if-shell from the clamped inner box",
			argv:   []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11", "6", "38", "10", "100", "30"},
			want:   []string{`if-shell -t %3 -F '#{pane_floating_flag}' 'if-shell -t %3 -F '\''#{==:#{pane-border-lines},none}'\'' '\''move-pane -t %3 -X 11 -Y 6 ; if-shell -t %3 -F '\''\'\'''\''#{||:#{&&:#{==:#{pane-border-status},top},#{==:#{pane_top},1}},#{&&:#{==:#{pane-border-status},bottom},#{==:#{e|+:#{pane_top},#{pane_height}},#{e|-:#{window_height},1}}}}'\''\'\'''\'' '\''\'\'''\''resize-pane -t %3 -x 38 -y 9'\''\'\'''\'' '\''\'\'''\''resize-pane -t %3 -x 38 -y 10'\''\'\'''\'''\'' '\''move-pane -t %3 -X 10 -Y 5 ; if-shell -t %3 -F '\''\'\'''\''#{||:#{&&:#{==:#{pane-border-status},top},#{==:#{pane_top},1}},#{&&:#{==:#{pane-border-status},bottom},#{==:#{e|+:#{pane_top},#{pane_height}},#{e|-:#{window_height},1}}}}'\''\'\'''\'' '\''\'\'''\''resize-pane -t %3 -x 40 -y 11'\''\'\'''\'' '\''\'\'''\''resize-pane -t %3 -x 40 -y 12'\''\'\'''\'''\'''`},
			layout: "@1",
		},
		{
			// Dragged off the left edge: clampInner slides the box back to X=1,
			// and both branches reach the clamped box, not the raw negative one.
			name: "float-geom clamps a negative offset before building the command",
			argv: []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "-5", "6", "45", "10", "100", "30"},
			want: []string{wantFloatGeom("%3", 1, 6, 45, 10)},
			local: [][]string{
				wantLocalFloatResize("%9", 47, 12),
				{"move-pane", "-t", "%9", "-X", "0", "-Y", "5"},
			},
			layout: "@1",
		},
		{
			// A float flush against the left edge, dragged further left: the
			// clamped box is where the remote float already is, so the remote
			// command changes nothing and no reconcile follows. The verb itself
			// must put the local float on the clamped box.
			name: "float-geom puts the local float on the clamped box when the clamp moved it",
			argv: []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "-7", "6", "38", "10", "100", "30"},
			want: []string{wantFloatGeom("%3", 1, 6, 38, 10)},
			local: [][]string{
				wantLocalFloatResize("%9", 40, 12),
				{"move-pane", "-t", "%9", "-X", "0", "-Y", "5"},
			},
			layout: "@1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtlStateWith("@1", "%2", "%3")
			req, err := c.parseCtl(tc.argv, sess)
			if err != nil {
				t.Fatalf("parseCtl: %v", err)
			}
			if !reflect.DeepEqual(req.cmds, tc.want) {
				t.Errorf("cmds = %q, want %q", req.cmds, tc.want)
			}
			if !reflect.DeepEqual(req.local, tc.local) {
				t.Errorf("local = %q, want %q", req.local, tc.local)
			}
			if req.wantWindows != tc.windows {
				t.Errorf("wantWindows = %v, want %v", req.wantWindows, tc.windows)
			}
			if req.wantLayout != tc.layout {
				t.Errorf("wantLayout = %q, want %q", req.wantLayout, tc.layout)
			}
			if req.wantReseed != tc.reseed {
				t.Errorf("wantReseed = %q, want %q", req.wantReseed, tc.reseed)
			}
		})
	}
}

func TestParseCtlRejects(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"unknown verb", []string{wire.CtlProtocolVersion, "detach-client", "%3"}, "unknown verb"},
		{"unmirrored pane", []string{wire.CtlProtocolVersion, "split-h", "%99"}, "not mirrored"},
		{"bad resize direction", []string{wire.CtlProtocolVersion, "resize", "%3", "X", "5"}, "bad direction"},
		{"bad resize amount", []string{wire.CtlProtocolVersion, "resize", "%3", "U", "abc"}, "bad amount"},
		{"resize amount out of range", []string{wire.CtlProtocolVersion, "resize", "%3", "U", "0"}, "bad amount"},
		{"bad swap direction", []string{wire.CtlProtocolVersion, "swap", "%3", "L"}, "bad direction"},
		{"layout rejects an unknown name", []string{wire.CtlProtocolVersion, "layout", "%3", "bogus"}, "bad layout"},
		{"layout rejects a free-form layout string", []string{wire.CtlProtocolVersion, "layout", "%3", "even-horizontal; kill-server"}, "bad layout"},
		{"rotate rejects an unknown direction", []string{wire.CtlProtocolVersion, "rotate", "%3", "X"}, "bad direction"},
		{"wrong arity", []string{wire.CtlProtocolVersion, "resize", "%3", "U"}, "wants 2 argument"},
		{"empty rename", []string{wire.CtlProtocolVersion, "rename", "%3", "|||"}, "empty name"},
		{"truncated frame", []string{wire.CtlProtocolVersion, "split-h"}, "at least version"},
		{"enrich-refresh takes no arguments", []string{wire.CtlProtocolVersion, "enrich-refresh", "%3", "@2"}, "wants 0 argument"},
		{"float-geom wrong arity", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11", "6", "38", "10"}, "wants 7 argument"},
		{"float-geom rejects the form without a local pane", []string{wire.CtlProtocolVersion, "float-geom", "%3", "11", "6", "38", "10", "100", "30"}, "wants 7 argument"},
		{"float-geom rejects a bare local pane number", []string{wire.CtlProtocolVersion, "float-geom", "%3", "9", "11", "6", "38", "10", "100", "30"}, "bad local pane"},
		{"float-geom rejects a local pane injection attempt", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9;x", "11", "6", "38", "10", "100", "30"}, "bad local pane"},
		{"float-geom rejects a non-integer", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "1x", "6", "38", "10", "100", "30"}, "bad x"},
		{"float-geom rejects a zero width", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11", "6", "0", "10", "100", "30"}, "bad w"},
		{"float-geom rejects a window narrower than a bordered box", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11", "6", "38", "10", "2", "30"}, "bad winW"},
		{"float-geom rejects an out-of-range offset", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "10000", "6", "38", "10", "100", "30"}, "bad x"},
		{"float-geom rejects an injection attempt", []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11;kill-server", "6", "38", "10", "100", "30"}, "bad x"},
		// A config reload can hand a new ctl to an old daemon; the mismatch must
		// be a message, not a silently-ignored gesture.
		{"version skew", []string{"1", "split-h", "%3"}, "reopen the bridge"},
		{"tile-layout wants its one layout argument", []string{wire.CtlProtocolVersion, "tile-layout", "%3"}, "wants 1 argument"},
		{"tile-layout rejects a second argument", []string{wire.CtlProtocolVersion, "tile-layout", "%3", "a", "b"}, "wants 1 argument"},
		{"tile-layout rejects unparsable text", []string{wire.CtlProtocolVersion, "tile-layout", "%3", "garbage"}, "layout:"},
		{"tile-layout rejects a v1 leaf with an empty id", []string{wire.CtlProtocolVersion, "tile-layout", "%3", "csum,100x30,0,0,"}, "no valid remote id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtlStateWith("@1", "%2", "%3")
			_, err := c.parseCtl(tc.argv, "rem")
			if err == nil {
				t.Fatalf("parseCtl(%q) succeeded, want error containing %q", tc.argv, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// rotate-window rotates which pane id is active (measured on next-3.9: %2 ->
// %0), so its verb must invalidate the daemon's active-pane belief; the layout
// presets and next/previous/spread preserve it and must not. A missed
// invalidation would leave the focus echo guard suppressing a needed focus.
func TestParseCtlRotateInvalidatesActiveBelief(t *testing.T) {
	c := newCtlStateWith("@1", "%2", "%3")

	for _, arg := range [][]string{{}, {"U"}, {"D"}} {
		argv := append([]string{wire.CtlProtocolVersion, "rotate", "%3"}, arg...)
		req, err := c.parseCtl(argv, "rem")
		if err != nil {
			t.Fatalf("parseCtl(%q): %v", argv, err)
		}
		if req.invalidate != "@1" {
			t.Errorf("rotate invalidate = %q, want @1", req.invalidate)
		}
		if req.wantLayout != "@1" {
			t.Errorf("rotate wantLayout = %q, want @1", req.wantLayout)
		}
	}

	for _, name := range []string{"even-horizontal", "main-vertical", "next", "previous", "spread"} {
		req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "layout", "%3", name}, "rem")
		if err != nil {
			t.Fatalf("parseCtl layout %s: %v", name, err)
		}
		if req.invalidate != "" {
			t.Errorf("layout %s invalidate = %q, want none", name, req.invalidate)
		}
		if req.wantLayout != "@1" {
			t.Errorf("layout %s wantLayout = %q, want @1", name, req.wantLayout)
		}
	}
}

// respawn-pane keeps the same pane id and active pane, so it must NOT
// invalidate the focus belief; respawn-window destroys every pane but the
// window's first and re-activates it, so it must.
func TestParseCtlRespawnInvalidatesActiveBelief(t *testing.T) {
	c := newCtlStateWith("@1", "%2", "%3")

	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "respawn-pane", "%3"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl respawn-pane: %v", err)
	}
	if req.invalidate != "" {
		t.Errorf("respawn-pane invalidate = %q, want none", req.invalidate)
	}
	if req.wantLayout != "" {
		t.Errorf("respawn-pane wantLayout = %q, want none", req.wantLayout)
	}

	req, err = c.parseCtl([]string{wire.CtlProtocolVersion, "respawn-window", "%3"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl respawn-window: %v", err)
	}
	if req.invalidate != "@1" {
		t.Errorf("respawn-window invalidate = %q, want @1", req.invalidate)
	}
	if req.wantWindows {
		t.Error("respawn-window wants a window reconcile, want none (the window survives)")
	}
}

// float-geom only reshapes an existing float: it must not invalidate the
// active-pane belief (not `moves`), gate on the viewing client (not
// `needsView`), or wait on a probe stamp (not `probe`).
func TestParseCtlFloatGeomFlags(t *testing.T) {
	c := newCtlStateWith("@1", "%2", "%3")
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "11", "6", "38", "10", "100", "30"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl float-geom: %v", err)
	}
	if req.invalidate != "" {
		t.Errorf("float-geom invalidate = %q, want none", req.invalidate)
	}
	if req.needsView {
		t.Error("float-geom needsView = true, want false")
	}
	if req.probePane != "" {
		t.Errorf("float-geom probePane = %q, want none", req.probePane)
	}
}

// A v1 tiled layout string, checksum and all, is re-parsed and rebuilt from
// scratch: the fresh checksum below was hand-verified against layoutChecksum
// (tile-layout must never trust the wire body's own checksum, hostile or
// stale as it may be).
const tileLayoutWantRaw = "eb98,100x30,0,0{50x30,0,0,3,49x30,51,0,4}"

func TestParseCtlTileLayout(t *testing.T) {
	c := newCtlStateWith("@1", "%3", "%4")
	// A wrong checksum prefix on the wire: parseCtl must recompute it rather
	// than trust or forward the one sent.
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "tile-layout", "%3", "0000,100x30,0,0{50x30,0,0,3,49x30,51,0,4}"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl tile-layout: %v", err)
	}
	want := []string{wantTileLayout("%3", []string{"%3", "%4"}, 100, 30, tileLayoutWantRaw)}
	if !reflect.DeepEqual(req.cmds, want) {
		t.Errorf("cmds = %q, want %q", req.cmds, want)
	}
	if req.wantLayout != "@1" {
		t.Errorf("wantLayout = %q, want @1", req.wantLayout)
	}
	if req.sentLayout != tileLayoutWantRaw {
		t.Errorf("sentLayout = %q, want %q", req.sentLayout, tileLayoutWantRaw)
	}
	if req.invalidate != "" {
		t.Errorf("invalidate = %q, want none", req.invalidate)
	}
}

// A v2 layout carrying a float leaf must prune to the same tiled command as
// the v1 shape above: the daemon rebuilds Raw from the parsed tree, so the
// wire format the ctl client happened to send never shapes the result.
func TestParseCtlTileLayoutV2PrunesFloats(t *testing.T) {
	c := newCtlStateWith("@1", "%3", "%4")
	v2 := `{"V":2,"L":{"t":"h","w":100,"h":30,"x":0,"y":0,"c":[` +
		`{"t":"p","w":50,"h":30,"x":0,"y":0,"I":"%3"},` +
		`{"t":"p","w":49,"h":30,"x":51,"y":0,"I":"%4"},` +
		`{"t":"p","w":10,"h":10,"x":5,"y":5,"I":"%5","z":1}]}}`

	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "tile-layout", "%3", v2}, "rem")
	if err != nil {
		t.Fatalf("parseCtl tile-layout v2: %v", err)
	}
	want := []string{wantTileLayout("%3", []string{"%3", "%4"}, 100, 30, tileLayoutWantRaw)}
	if !reflect.DeepEqual(req.cmds, want) {
		t.Errorf("cmds = %q, want %q", req.cmds, want)
	}
}

// A hostile checksum prefix is discarded along with the rest of the raw
// text: Raw is always rebuilt from the parsed tree, never echoed.
func TestParseCtlTileLayoutRejectsHostileChecksum(t *testing.T) {
	c := newCtlStateWith("@1", "%3", "%4")
	hostile := "x'; kill-server,100x30,0,0{50x30,0,0,3,49x30,51,0,4}"

	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "tile-layout", "%3", hostile}, "rem")
	if err != nil {
		t.Fatalf("parseCtl tile-layout: %v", err)
	}
	for _, cmd := range req.cmds {
		if strings.Contains(cmd, "kill-server") {
			t.Fatalf("cmds = %q, hostile text reached the command", req.cmds)
		}
	}
}

func TestTileLayoutIntentCoalescing(t *testing.T) {
	const raw1 = "csum,50x30,0,0,2"
	const raw2 = "csum,50x30,0,0,3"

	tileReq := func(c *ctlState, raw string) ctlRequest {
		req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "tile-layout", "%2", raw}, "rem")
		if err != nil {
			t.Fatalf("parseCtl tile-layout: %v", err)
		}
		return req
	}
	layoutReq := func(c *ctlState) ctlRequest {
		req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "layout", "%2", "tiled"}, "rem")
		if err != nil {
			t.Fatalf("parseCtl layout: %v", err)
		}
		return req
	}
	send := func(...string) bool { return true }

	t.Run("tile-layout then layout coalesces to the drag's sentLayout", func(t *testing.T) {
		c := newCtlStateWith("@1", "%2", "%3")
		req := tileReq(c, raw1)
		S := req.sentLayout
		c.submit(req, send)
		c.submit(layoutReq(c), send)

		_, layouts, _ := c.takeIntents()
		if want := (map[string]string{"@1": S}); !reflect.DeepEqual(layouts, want) {
			t.Errorf("layouts = %v, want %v", layouts, want)
		}
	})

	t.Run("layout then tile-layout coalesces to the drag's sentLayout", func(t *testing.T) {
		c := newCtlStateWith("@1", "%2", "%3")
		c.submit(layoutReq(c), send)
		req := tileReq(c, raw1)
		S := req.sentLayout
		c.submit(req, send)

		_, layouts, _ := c.takeIntents()
		if want := (map[string]string{"@1": S}); !reflect.DeepEqual(layouts, want) {
			t.Errorf("layouts = %v, want %v", layouts, want)
		}
	})

	t.Run("two drags: the later replaces the earlier", func(t *testing.T) {
		c := newCtlStateWith("@1", "%2", "%3")
		c.submit(tileReq(c, raw1), send)
		req2 := tileReq(c, raw2)
		S2 := req2.sentLayout
		c.submit(req2, send)

		_, layouts, _ := c.takeIntents()
		if want := (map[string]string{"@1": S2}); !reflect.DeepEqual(layouts, want) {
			t.Errorf("layouts = %v, want %v", layouts, want)
		}
	})

	t.Run("layout alone carries no sentLayout", func(t *testing.T) {
		c := newCtlStateWith("@1", "%2", "%3")
		c.submit(layoutReq(c), send)

		_, layouts, _ := c.takeIntents()
		if want := (map[string]string{"@1": ""}); !reflect.DeepEqual(layouts, want) {
			t.Errorf("layouts = %v, want %v", layouts, want)
		}
	})

	t.Run("forgetWindow drops a pending drag", func(t *testing.T) {
		c := newCtlStateWith("@1", "%2", "%3")
		c.submit(tileReq(c, raw1), send)
		c.forgetWindow("@1")

		if _, layouts, _ := c.takeIntents(); len(layouts) != 0 {
			t.Errorf("layouts = %v, want none after forgetWindow", layouts)
		}
	})
}

func TestParseCtlPingProbesCompatibilityBeforePaneLookup(t *testing.T) {
	c := newCtlState()
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "ping", "placeholder"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl ping: %v", err)
	}
	if len(req.cmds) != 0 || req.wantWindows || req.wantLayout != "" || req.invalidate != "" {
		t.Errorf("ping request = %+v, want no side effects", req)
	}

	_, err = c.parseCtl([]string{"1", "ping", "placeholder"}, "rem")
	if err == nil || !strings.Contains(err.Error(), "reopen the bridge") {
		t.Fatalf("v1 daemon compatibility rejection = %v, want version mismatch", err)
	}
}

// TestPingSubmitAcksWithNoLiveConnection: parseCtl returns a request with no
// commands for ping, so submit's loop never calls send and acks even against
// a send that always refuses — which is what connHolder.send does with an
// empty slot mid-outage. Without that, og-remote-open's dedup would read a
// disconnected bridge as dead and stack a second daemon on the same socket.
func TestPingSubmitAcksWithNoLiveConnection(t *testing.T) {
	c := newCtlState()
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "ping", "placeholder"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl ping: %v", err)
	}
	if !c.submit(req, func(...string) bool { return false }) {
		t.Error("submit reported ping unwritten with no live connection, want ack")
	}
}

// The daemon must build every remote command from the verb table, never forward
// text a caller supplied.
func TestParseCtlNeverForwardsRawCommandText(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "rename", "%3", "x; kill-server"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl: %v", err)
	}
	if len(req.cmds) != 1 || !strings.HasPrefix(req.cmds[0], "rename-window -t @1 -- ") {
		t.Fatalf("cmds = %q, want a single quoted rename-window", req.cmds)
	}
	if strings.Count(req.cmds[0], "\n") != 0 {
		t.Errorf("command must stay one line: %q", req.cmds[0])
	}
}

// submit must register the intent and send inside one critical section, so a
// drain that observes the sent command can never miss the intent.
func TestSubmitRegistersIntentBeforeSending(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "split-h", "%3"}, "rem")
	if err != nil {
		t.Fatalf("parseCtl: %v", err)
	}

	sawIntent := false
	ok := c.submit(req, func(...string) bool {
		// Read the field directly: takeIntents would deadlock on the held mutex,
		// which is itself the property under test.
		_, sawIntent = c.wantLayout["@1"]
		return true
	})
	if !ok {
		t.Fatal("submit reported not written")
	}
	if !sawIntent {
		t.Error("intent was not registered before the command was sent")
	}
}

// A request that loses the race with teardown must not be acked as accepted.
func TestSubmitReportsUnwritten(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	req, _ := c.parseCtl([]string{wire.CtlProtocolVersion, "split-h", "%3"}, "rem")
	if c.submit(req, func(...string) bool { return false }) {
		t.Error("submit reported written when send refused")
	}
}

func TestTakeIntentsCoalescesAndDrains(t *testing.T) {
	c := newCtlStateWith("@1", "%2", "%3")
	c.setWindowPanes("@2", []string{"%9"})
	for _, argv := range [][]string{
		{wire.CtlProtocolVersion, "split-h", "%2"},
		{wire.CtlProtocolVersion, "split-v", "%3"}, // same window: coalesces
		{wire.CtlProtocolVersion, "split-h", "%9"}, // different window
		{wire.CtlProtocolVersion, "new-window", "%2"},
		{wire.CtlProtocolVersion, "respawn-pane", "%2"},   // reseed @1
		{wire.CtlProtocolVersion, "respawn-window", "%3"}, // reseed + layout @1, coalesces
		{wire.CtlProtocolVersion, "respawn-pane", "%9"},   // reseed @2
	} {
		req, err := c.parseCtl(argv, "rem")
		if err != nil {
			t.Fatalf("parseCtl(%q): %v", argv, err)
		}
		c.submit(req, func(...string) bool { return true })
	}

	windows, layouts, reseeds := c.takeIntents()
	if !windows {
		t.Error("new-window should have registered the window reconcile")
	}
	if len(layouts) != 2 {
		t.Errorf("layouts = %v, want 2 distinct windows", layouts)
	}
	// @1 and @2 each got respawns; @1 coalesces its respawn-pane and
	// respawn-window into one entry, so exactly the two respawned windows
	// remain.
	if len(reseeds) != 2 {
		t.Errorf("reseeds = %v, want 2 distinct windows", reseeds)
	}

	windows, layouts, reseeds = c.takeIntents()
	if windows || len(layouts) != 0 || len(reseeds) != 0 {
		t.Errorf("second take should be empty, got %v %v %v", windows, layouts, reseeds)
	}
}

// A closed window's focus state and pending intent must not outlive it.
func TestForgetWindowDropsState(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	req, _ := c.parseCtl([]string{wire.CtlProtocolVersion, "split-h", "%3"}, "rem")
	c.submit(req, func(...string) bool { return true })

	c.forgetWindow("@1")

	if _, layouts, reseeds := c.takeIntents(); len(layouts) != 0 || len(reseeds) != 0 {
		t.Errorf("layouts = %v reseeds = %v, want none after forgetWindow", layouts, reseeds)
	}
	if _, err := c.parseCtl([]string{wire.CtlProtocolVersion, "split-h", "%3"}, "rem"); err == nil {
		t.Error("a pane of a forgotten window must no longer resolve")
	}
}

func TestCtlArgvRoundTrip(t *testing.T) {
	tests := [][]string{
		{wire.CtlProtocolVersion, "split-h", "%3"},
		{wire.CtlProtocolVersion, "rename", "%3", "a name with spaces"},
		{wire.CtlProtocolVersion, "rename", "%3", ""}, // an empty argument must survive as one field
		{wire.CtlProtocolVersion, "rename", "%3", "quote's and |pipe|"},
	}
	for _, argv := range tests {
		if got := wire.DecodeArgv(wire.EncodeArgv(argv)); !reflect.DeepEqual(got, argv) {
			t.Errorf("round-trip %q -> %q", argv, got)
		}
	}
	if got := wire.DecodeArgv(nil); got != nil {
		t.Errorf("empty payload decoded to %q, want nil", got)
	}
}

func TestCarouselVerbBuildsRemoteToggle(t *testing.T) {
	v, ok := verbs["carousel"]
	if !ok {
		t.Fatal("no carousel verb")
	}
	cmds, err := v.build("%5", "@2", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want one command, got %v", cmds)
	}
	script := carouselResolveScript("%5")
	if strings.Contains(script, "'") {
		t.Fatalf("resolve script must have zero single quotes: %q", script)
	}
	wantCmd := fmt.Sprintf("run-shell -b -t %%5 %s", tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
	if cmds[0] != wantCmd {
		t.Fatalf("command\n got %q\nwant %q", cmds[0], wantCmd)
	}
	for _, want := range []string{
		"show-options -pqv",
		"@claude_img_src",
		"TMUX_PANE=\"$src\"",
		"AEYE_BRIDGED=1",
		"tmux-claude-images",
		"command -v",
		carouselStampCmd("%5", carouselVerdictNoImages),
		carouselStampCmd("%5", carouselVerdictNoBin),
		carouselStampCmd("%5", carouselVerdictOK),
		carouselClearCmd("%5"),
	} {
		if !strings.Contains(cmds[0], want) {
			t.Fatalf("command %q missing %q", cmds[0], want)
		}
	}
	// new-pane and a float shape are what #593 removed: every outcome is a
	// verdict stamp the daemon reads back, so nothing opens on the remote.
	for _, ban := range []string{"display-message", "#{@claude_img_src}", "''|*", "split-window", "@float_geom", "new-pane", remoteFloatFull} {
		if strings.Contains(cmds[0], ban) {
			t.Fatalf("command %q must not contain %q", cmds[0], ban)
		}
	}
	if !v.probe {
		t.Fatal("every outcome is a verdict stamp the daemon must read back: needs probe")
	}
	if !v.moves || !v.layout {
		t.Fatal("the toggle opens a float that takes focus: needs moves+layout")
	}
}

func TestCarouselSrcValidation(t *testing.T) {
	const pane = "%1"
	// Mirror the case arms in carouselResolveScript (lookup stubbed via $src).
	validate := `case "$src" in %[0-9]*) case "${src#%}" in *[!0-9]*) src=` + pane + `;; esac;; *) src=` + pane + `;; esac; printf %s "$src"`
	tests := []struct {
		src  string
		want string
	}{
		{"%0", "%0"},
		{"", pane},
		{"%1", "%1"},
		{"junk", pane},
		{"%12", "%12"},
		{"%0x", pane},
	}
	for _, tc := range tests {
		cmd := exec.Command("/bin/sh", "-c", validate)
		cmd.Env = append(os.Environ(), "src="+tc.src)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("src=%q: %v", tc.src, err)
		}
		if got := string(out); got != tc.want {
			t.Errorf("src=%q: got %q, want %q", tc.src, got, tc.want)
		}
	}
}

// TestCarouselResolveScriptManifestCheck runs the ACTUAL "carousel" verb
// command through a real, private tmux server via run-shell — not a bare
// /bin/sh -c of carouselResolveScript's return value. That distinction is
// load-bearing: run-shell format-expands its whole argument before /bin/sh
// ever sees it, collapsing a run of literal '#' characters pairwise (`####`
// -> `##`, measured against the pinned tmux build), so a script executed
// directly never exercises that collapse and would silently miss a broken
// manifest-field extraction underneath it.
func TestCarouselResolveScriptManifestCheck(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tests := []struct {
		name        string
		hasManifest bool
	}{
		{"empty manifest stamps a verdict, opens nothing", false},
		{"present manifest launches the carousel, opens nothing", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stubDir := filepath.Join(dir, "bin")
			if err := os.Mkdir(stubDir, 0o755); err != nil {
				t.Fatal(err)
			}
			launchLog := filepath.Join(dir, "launch.log")
			manifest := filepath.Join(dir, "manifest.jsonl")
			if tc.hasManifest {
				if err := os.WriteFile(manifest, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeStub(t, filepath.Join(stubDir, "tmux-claude-images"), `#!/bin/sh
if [ "$1" = --resolve ]; then
	printf 'tmux\tkey\t`+manifest+`\n'
	exit 0
fi
echo launched >>"`+launchLog+`"
`)

			tmux := startIsolatedTmux(t, "PATH="+stubDir+":"+os.Getenv("PATH"))

			paneOut, err := tmux("display-message", "-p", "-t", "w", "#{pane_id}").Output()
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			pane := strings.TrimSpace(string(paneOut))

			cmds, err := verbs["carousel"].build(pane, "@0", "w", nil)
			if err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(dir, "cmd.conf")
			if err := os.WriteFile(conf, []byte(cmds[0]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// source-file drives the same command grammar (and the same
			// format-expansion pass) a control-mode client's command would.
			if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
				t.Fatalf("source-file: %v\n%s", err, out)
			}

			// run-shell -b is asynchronous; poll for its effect (the stub's
			// launch marker, or the verdict stamp).
			wantVerdict := carouselVerdictNoImages
			if tc.hasManifest {
				wantVerdict = carouselVerdictOK
			}
			deadline := time.Now().Add(3 * time.Second)
			var (
				launched bool
				verdict  string
			)
			for time.Now().Before(deadline) {
				if b, _ := os.ReadFile(launchLog); len(b) > 0 {
					launched = true
				}
				out, err := tmux("show-options", "-pqv", "-t", pane, carouselVerdictOpt).Output()
				if err == nil {
					verdict = strings.TrimSpace(string(out))
				}
				if verdict == wantVerdict && launched == tc.hasManifest {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}

			if verdict != wantVerdict {
				t.Errorf("%s = %q, want %q", carouselVerdictOpt, verdict, wantVerdict)
			}
			if launched != tc.hasManifest {
				t.Errorf("tmux-claude-images exec'd = %v, want %v", launched, tc.hasManifest)
			}
			// The verdict IS the whole report now: a fallback pane would be
			// the 90%x90% float #593 removed, mirrored home to say one
			// sentence.
			out, err := tmux("list-panes", "-t", "w").Output()
			if err != nil {
				t.Fatalf("list-panes: %v", err)
			}
			if n := len(strings.Split(strings.TrimSpace(string(out)), "\n")); n > 1 {
				t.Errorf("window has %d panes, want 1 — a fallback pane was opened:\n%s", n, out)
			}
		})
	}
}

// A remote with no toggle at all reports nobin. Its own branch because it is
// the one outcome that returns before the manifest is ever resolved (#593),
// and because the message it produces names a rebuild the user has to do.
func TestCarouselResolveScriptStampsNoBin(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	// A PATH holding tmux and nothing else: the script needs tmux to stamp
	// with, and must not find a tmux-claude-images this host happens to have
	// installed.
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmuxPath, filepath.Join(binDir, "tmux")); err != nil {
		t.Fatal(err)
	}

	tmux := startIsolatedTmux(t, "PATH="+binDir)
	paneOut, err := tmux("display-message", "-p", "-t", "w", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	pane := strings.TrimSpace(string(paneOut))

	cmds, err := verbs["carousel"].build(pane, "@0", "w", nil)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "cmd.conf")
	if err := os.WriteFile(conf, []byte(cmds[0]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
		t.Fatalf("source-file: %v\n%s", err, out)
	}

	var verdict string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := tmux("show-options", "-pqv", "-t", pane, carouselVerdictOpt).Output()
		if err == nil {
			verdict = strings.TrimSpace(string(out))
		}
		if verdict != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if verdict != carouselVerdictNoBin {
		t.Errorf("%s = %q, want %q", carouselVerdictOpt, verdict, carouselVerdictNoBin)
	}
	out, err := tmux("list-panes", "-t", "w").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(out)), "\n")); n > 1 {
		t.Errorf("window has %d panes, want 1 — a fallback pane was opened:\n%s", n, out)
	}
}

// TestCarouselResolveScriptPrefersCarouselBin pins the #554 fix: the verb
// resolves the toggle through @carousel_bin (repointed by every config
// reload) rather than the server environment's frozen PATH, and falls back
// to PATH when the option is unset (older remote) or its store path is gone
// (garbage-collected generation).
func TestCarouselResolveScriptPrefersCarouselBin(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tests := []struct {
		name      string
		optionVal string // "set" = live stub path, "gc" = nonexistent path, "" = unset
		wantLog   string
	}{
		{"option wins over PATH", "set", "option"},
		{"GC'd option path falls back to PATH", "gc", "path"},
		{"unset option falls back to PATH", "", "path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "manifest.jsonl")
			if err := os.WriteFile(manifest, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			stubBody := func(log string) string {
				return `#!/bin/sh
if [ "$1" = --resolve ]; then
	printf 'tmux\tkey\t` + manifest + `\n'
	exit 0
fi
echo launched >>"` + log + `"
`
			}
			pathLog := filepath.Join(dir, "path.log")
			optLog := filepath.Join(dir, "opt.log")
			pathDir := filepath.Join(dir, "pathbin")
			optDir := filepath.Join(dir, "optbin")
			for _, d := range []string{pathDir, optDir} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeStub(t, filepath.Join(pathDir, "tmux-claude-images"), stubBody(pathLog))
			optStub := filepath.Join(optDir, "tmux-claude-images")
			writeStub(t, optStub, stubBody(optLog))

			tmux := startIsolatedTmux(t, "PATH="+pathDir+":"+os.Getenv("PATH"))
			switch tc.optionVal {
			case "set":
				if out, err := tmux("set-option", "-g", "@carousel_bin", optStub).CombinedOutput(); err != nil {
					t.Fatalf("set-option: %v\n%s", err, out)
				}
			case "gc":
				if out, err := tmux("set-option", "-g", "@carousel_bin", filepath.Join(dir, "gone")).CombinedOutput(); err != nil {
					t.Fatalf("set-option: %v\n%s", err, out)
				}
			}

			paneOut, err := tmux("display-message", "-p", "-t", "w", "#{pane_id}").Output()
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			pane := strings.TrimSpace(string(paneOut))
			cmds, err := verbs["carousel"].build(pane, "@0", "w", nil)
			if err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(dir, "cmd.conf")
			if err := os.WriteFile(conf, []byte(cmds[0]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
				t.Fatalf("source-file: %v\n%s", err, out)
			}

			// run-shell -b is asynchronous; poll for the winning stub's
			// launch marker.
			logs := map[string]string{"option": optLog, "path": pathLog}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if b, _ := os.ReadFile(logs[tc.wantLog]); len(b) > 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			for name, log := range logs {
				b, _ := os.ReadFile(log)
				launched := len(b) > 0
				if name == tc.wantLog && !launched {
					t.Fatalf("%s stub was never exec'd", name)
				}
				if name != tc.wantLog && launched {
					t.Fatalf("%s stub was exec'd, want only %s", name, tc.wantLog)
				}
			}
		})
	}
}

func writeStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writeStub %s: %v", path, err)
	}
}

// startIsolatedTmux starts a private tmux server whose unix socket path stays
// within macOS sun_path (104 bytes). Nix-build sandboxes give t.TempDir() a
// long prefix; a -S path derived from it plus the test name exceeds that
// limit, while -L with a fixed name under os.MkdirTemp("", "lz") does not.
func startIsolatedTmux(t *testing.T, extraEnv ...string) func(args ...string) *exec.Cmd {
	t.Helper()
	tmpdir, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	const socket = "s"
	env := append(os.Environ(), "TMUX_TMPDIR="+tmpdir)
	env = append(env, extraEnv...)
	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", "w", "-x", "80", "-y", "24")
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command("tmux", "-L", socket, "kill-server")
		stop.Env = env
		stop.Run()
	})
	return func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", append([]string{"-L", socket}, args...)...)
		cmd.Env = env
		return cmd
	}
}

func TestToolVerbBuildsRemoteFloatInRemoteCwd(t *testing.T) {
	v, ok := verbs["tool"]
	if !ok {
		t.Fatal("no tool verb")
	}
	// Exact shape per tool, matching config/tmux.conf.nix's floatShort/floatFull
	// binds byte for byte.
	tests := []struct {
		tool  string
		flags string
	}{
		{"prdash", remoteFloatShort},
		{"yazi", remoteFloatShort},
		{"lazygit", remoteFloatFull},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			cmds, err := v.build("%5", "@2", "sess", []string{tc.tool})
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 {
				t.Fatalf("want one command, got %v", cmds)
			}
			script := toolResolveScript(tc.tool)
			if strings.Contains(script, "'") {
				t.Fatalf("resolve script must have zero single quotes: %q", script)
			}
			// The create branch keeps the geometry byte for byte; the reuse gate
			// wraps it, so it is asserted inside its own quoting.
			wantCreate := fmt.Sprintf("new-pane -t %%5 -c '#{pane_current_path}' %s %s ; set -p -t @2 @pane_label %s",
				tc.flags, tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)), tc.tool)
			if !strings.Contains(cmds[0], tmuxQuote(wantCreate)) {
				t.Fatalf("command\n got %q\nwant create branch %q", cmds[0], wantCreate)
			}
			if !strings.Contains(cmds[0], tmuxQuote(floatLookup(tc.tool))) {
				t.Fatalf("command %q does not gate on %q", cmds[0], floatLookup(tc.tool))
			}
			if strings.Contains(cmds[0], "@float_geom") {
				t.Fatalf("command %q must not stamp @float_geom: the remote's own tmux-float-refit would fight the mirror for authority over it", cmds[0])
			}
		})
	}
	// With no cwd argument the format stays, as the only thing left to fall back
	// on; the tool must be resolved off the remote PATH rather than a local
	// store path either way.
	cmds, err := v.build("%5", "@2", "sess", []string{"prdash"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{nestedQuote("-c '#{pane_current_path}'"), "show-environment -g PATH", "command -v prdash", "exec prdash"} {
		if !strings.Contains(cmds[0], want) {
			t.Fatalf("command %q missing %q", cmds[0], want)
		}
	}
	if strings.Contains(cmds[0], "/nix/store") {
		t.Fatalf("command %q must not carry a local store path", cmds[0])
	}
	if !v.moves || !v.layout {
		t.Fatal("the verb opens a float that takes focus: needs moves+layout")
	}
}

// A tool press must reuse the float its window already holds for that tool
// instead of stacking another one. new-pane -A is a z-order flag (the float
// stays visible above a zoomed pane), not attach-if-exists, so the reuse has to
// be an explicit lookup — and the lookup key, @pane_label, has to be stamped on
// the float the verb creates or the second press could not find the first.
//
// The current window is deliberately NOT the target window: if-shell -t pins
// the condition's context but not the branch's, so this is what makes a missing
// -t on `set -wF` or on `run-shell` fail rather than pass by accident.
func TestToolVerbDoesNotStackFloats(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	v, ok := verbs["tool"]
	if !ok {
		t.Fatal("no tool verb")
	}

	dir := t.TempDir()
	stubDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stub has to outlive the press: the bind pins remain-on-exit off, so a
	// command that exits closes its own pane and the assertion would read zero
	// floats on the fixed tree as well as on the broken one.
	writeStub(t, filepath.Join(stubDir, "prdash"), "#!/bin/sh\nsleep 600\n")

	tmux := startIsolatedTmux(t, "PATH="+stubDir+":"+os.Getenv("PATH"))

	baseOut, err := tmux("display-message", "-p", "-t", "w", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	base := strings.TrimSpace(string(baseOut))

	// A second window becomes the session's current one, so the target window
	// (window 0, holding the base pane) is not the current window.
	if out, err := tmux("new-window", "-t", "w:").CombinedOutput(); err != nil {
		t.Fatalf("new-window: %v\n%s", err, out)
	}

	cmds, err := v.build(base, "@0", "w", []string{"prdash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want one command, got %v", cmds)
	}
	conf := filepath.Join(dir, "cmd.conf")
	if err := os.WriteFile(conf, []byte(cmds[0]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	press := func() {
		t.Helper()
		if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
			t.Fatalf("source-file: %v\n%s", err, out)
		}
	}
	// Every floating pane in the window, as (pane id, @pane_label, active).
	floats := func() [][3]string {
		t.Helper()
		// -a, not -t w: a session target resolves to its CURRENT window, and the
		// press happens in the window that is not current. '|' rather than
		// whitespace, so an unset @pane_label does not collapse the field list.
		out, err := tmux("list-panes", "-a", "-F",
			"#{pane_id}|#{pane_floating_flag}|#{@pane_label}|#{pane_active}").Output()
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		var got [][3]string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Split(line, "|")
			if len(f) == 4 && f[1] == "1" {
				got = append(got, [3]string{f[0], f[2], f[3]})
			}
		}
		return got
	}

	press()
	first := floats()
	if len(first) != 1 {
		t.Fatalf("first press: %d floats %v, want 1", len(first), first)
	}

	// Un-focus the float, so the second press has to focus it rather than merely
	// inheriting the focus new-pane left behind.
	if out, err := tmux("select-pane", "-t", base).CombinedOutput(); err != nil {
		t.Fatalf("select-pane: %v\n%s", err, out)
	}
	press()
	second := floats()
	if len(second) != 1 {
		t.Fatalf("second press stacked another float: %d %v, want 1", len(second), second)
	}
	// The reuse has to have been keyed on @pane_label: the float the window now
	// holds is the one the create branch stamped.
	if second[0][1] != "prdash" {
		t.Fatalf("reused float carries label %q, want prdash", second[0][1])
	}
	if second[0][0] != first[0][0] || second[0][2] != "1" {
		t.Fatalf("second press did not focus the existing float: %v (first press was %v)", second, first)
	}
}

// A pane spawned through fish gets a PATH rebuilt from the login profile, so the
// script restores tmux's own before looking the tool up. The three shapes
// show-environment can answer with, run for real under /bin/sh against a stub.
func TestToolPathRestore(t *testing.T) {
	// The restore prefix, verbatim from the shipped script.
	prefix, _, found := strings.Cut(toolResolveScript("prdash"), "command -v")
	if !found {
		t.Fatal("toolResolveScript no longer has a command -v")
	}

	tests := []struct {
		name string
		stub string
		want string
	}{
		{"global PATH present", "echo PATH=/opt/a:/opt/b", "/opt/a:/opt/b:/login"},
		{"variable unset", "echo unknown variable: PATH >&2; exit 1", "/login"},
		{"empty value", "echo PATH=", "/login"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "tmux")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+tc.stub+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", prefix+`printf %s "$PATH"`)
			cmd.Env = []string{"PATH=" + dir + ":/login"}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			// The stub dir is only there so `tmux` resolves, and the restore
			// prepends ahead of it; drop it wherever it landed to compare.
			if got := strings.Replace(string(out), dir+":", "", 1); got != tc.want {
				t.Errorf("PATH = %q, want %q", got, tc.want)
			}
		})
	}
}

// A remote profile (fish's login rebuild) may export its own BROWSER, which
// would override the tmux global BROWSER=<og-open path> and silently disable
// URL forwarding; the restore must win over that, exactly as the PATH restore
// wins over fish's rebuilt PATH.
func TestToolBrowserRestore(t *testing.T) {
	// The restore prefix, verbatim from the shipped script.
	prefix, _, found := strings.Cut(toolResolveScript("prdash"), "command -v")
	if !found {
		t.Fatal("toolResolveScript no longer has a command -v")
	}

	tests := []struct {
		name string
		stub string
		want string
	}{
		{"global present", "echo BROWSER=/nix/store/x/bin/og-open", "/nix/store/x/bin/og-open"},
		{"variable unset", "echo unknown variable: BROWSER >&2; exit 1", "profile"},
		{"empty value", "echo BROWSER=", "profile"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "tmux")
			script := "#!/bin/sh\ncase \"$3\" in\nBROWSER) " + tc.stub + ";;\nPATH) echo PATH=/opt/a;;\nesac\n"
			if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", prefix+`printf %s "${BROWSER-unset}"`)
			cmd.Env = []string{"PATH=" + dir + ":/login", "BROWSER=profile"}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := string(out); got != tc.want {
				t.Errorf("BROWSER = %q, want %q", got, tc.want)
			}
		})
	}
}

// split-window does not format-expand its shell-command but run-shell does, so
// the restore trims with #* rather than #PATH= — under run-shell the latter's
// #P would expand to the pane index. Keep the body free of every sequence tmux
// would treat as a format, so it stays correct wherever it is used.
func TestToolResolveScriptSurvivesFormatExpansion(t *testing.T) {
	script := toolResolveScript("prdash")
	for _, bad := range []string{"#{", "#(", "#P", "#S", "#W", "#T", "#D", "#F", "#I", "#H"} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains tmux format %q, which run-shell would expand: %q", bad, script)
		}
	}
}

// nestedQuote is tmuxQuote's transformation without its outer quotes: how a
// string comes out once it is embedded inside another single-quoted tmux
// argument. The tool verb's create branch is exactly that — a nested command
// string inside the reuse gate's own quoting.
func nestedQuote(s string) string {
	q := tmuxQuote(s)
	return q[1 : len(q)-1]
}

// The cwd the bind reads off @bridge_dir is what makes the float open in the
// window it was pressed in: tmux expands a -c format against the client's
// current pane rather than the -t target, so the remote leg cannot resolve it
// (#643). A value that could be read as something other than a path drops back
// to that format instead of reaching the remote command line.
func TestToolVerbUsesSuppliedCwd(t *testing.T) {
	v := verbs["tool"]

	kept := []string{
		"/home/noams/git/toddl",
		"/home/noams/Data/git/.worktrees/git/lazytmux/feat-640-mirror-crew-pane-borders",
		"/home/noams/two words", // run-shell splits, #{qs:} does not — the verb must take it whole
		"/home/noams/it's-here", // tmuxQuote owns the escaping
		"/home/noams/a;b,c:d",   // literal inside the quotes
		"/" + strings.Repeat("d", maxRemoteToolCwd-1),
	}
	for _, dir := range kept {
		cmds, err := v.build("%5", "@2", "sess", []string{"prdash", dir})
		if err != nil {
			t.Fatalf("dir %q: %v", dir, err)
		}
		// Inside the reuse gate's own quoting: the create branch is a nested
		// command string now, so its quotes are re-escaped one level deeper.
		want := nestedQuote("new-pane -t %5 -c " + tmuxQuote(dir) + " ")
		if !strings.Contains(cmds[0], want) {
			t.Fatalf("dir %q\n got %q\nwant prefix %q", dir, cmds[0], want)
		}
	}

	dropped := []string{
		"",                           // an unset @bridge_dir quotes as an empty argument
		"relative/path",              // never a cwd the remote was sitting in
		"~/git/toddl",                // the shell would have expanded this, tmux will not
		"/home/noams/a#b",            // new-pane format-expands -c
		"/home/noams/#(id)",          // ... so this would be a command the remote runs
		"/home/noams/a\nkill-server", // would end the daemon's command line and start another
		"/home/noams/a\x7fb",
		"/" + strings.Repeat("d", maxRemoteToolCwd),
	}
	for _, dir := range dropped {
		cmds, err := v.build("%5", "@2", "sess", []string{"prdash", dir})
		if err != nil {
			t.Fatalf("dir %q: %v", dir, err)
		}
		if !strings.Contains(cmds[0], nestedQuote("-c '#{pane_current_path}'")) {
			t.Fatalf("dir %q was not dropped back to the format: %q", dir, cmds[0])
		}
		if strings.Contains(cmds[0], dir) && dir != "" {
			t.Fatalf("dir %q reached the command line: %q", dir, cmds[0])
		}
	}
}

// The cwd is optional on the wire: a mirror whose local config predates #643
// sends the two-argument form, and refusing it would break the tool binds for
// as long as that server lives.
func TestToolVerbCwdIsOptionalOnTheWire(t *testing.T) {
	c := newCtlStateWith("@1", "%2")
	base := []string{wire.CtlProtocolVersion, "tool", "%2", "prdash"}
	for _, argv := range [][]string{base, append(base, "/home/noams/git/toddl")} {
		if _, err := c.parseCtl(argv, "sess"); err != nil {
			t.Fatalf("argv %v rejected: %v", argv, err)
		}
	}
	if _, err := c.parseCtl(append(base, "/a", "/b"), "sess"); err == nil {
		t.Fatal("a third argument was accepted")
	}
}

func TestToolVerbRejectsUnlistedTool(t *testing.T) {
	v := verbs["tool"]
	for _, tool := range []string{"", "rm -rf /", "prdash; id", "PRDASH", "sh"} {
		if _, err := v.build("%5", "@2", "sess", []string{tool}); err == nil {
			t.Fatalf("tool %q was accepted", tool)
		}
	}
	for tool := range remoteTools {
		if _, err := v.build("%5", "@2", "sess", []string{tool}); err != nil {
			t.Fatalf("tool %q rejected: %v", tool, err)
		}
	}
}

func TestThemeVerbBuildsSilentRemoteApply(t *testing.T) {
	v, ok := verbs["theme"]
	if !ok {
		t.Fatal("no theme verb")
	}
	cmds, err := v.build("%5", "@2", "sess", []string{"light"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want one command, got %v", cmds)
	}
	script := themeApplyScript("light")
	if strings.Contains(script, "'") {
		t.Fatalf("apply script must have zero single quotes: %q", script)
	}
	wantCmd := fmt.Sprintf("run-shell -b -t %%5 %s", tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
	if cmds[0] != wantCmd {
		t.Fatalf("command\n got %q\nwant %q", cmds[0], wantCmd)
	}
	// A remote without theme-toggle must stay silent: nothing may open a pane or
	// write where a mirror would repaint it.
	for _, ban := range []string{"split-window", "display-message", "echo"} {
		if strings.Contains(cmds[0], ban) {
			t.Fatalf("command %q must not contain %q", cmds[0], ban)
		}
	}
	if v.moves || v.layout || v.windows {
		t.Fatal("applying a theme changes no remote structure: no reconcile intent")
	}
}

func TestThemeVerbRejectsUnlistedTheme(t *testing.T) {
	v := verbs["theme"]
	for _, theme := range []string{"", "mocha", "dark; id", "Light"} {
		if _, err := v.build("%5", "@2", "sess", []string{theme}); err == nil {
			t.Fatalf("theme %q was accepted", theme)
		}
	}
	for theme := range remoteThemes {
		if _, err := v.build("%5", "@2", "sess", []string{theme}); err != nil {
			t.Fatalf("theme %q rejected: %v", theme, err)
		}
	}
}

// A `-t` run-shell job's stdout and non-zero exit both land in view mode on the
// target pane, and a view-mode overlay on a mirrored pane wedges the mirror.
// The host PATH is withheld so no real theme-toggle can flip the desktop theme
// and so the absent case is really absent; the verb body needs nothing from
// PATH (/bin/sh is absolute, command -v is a builtin).
func TestThemeVerbNeverOverlaysPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tests := []struct {
		name   string
		toggle bool
	}{
		{"no theme-toggle on PATH", false},
		{"theme-toggle prints and exits 1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "ran")
			if tc.toggle {
				stub := "#!/bin/sh\n: >\"" + marker + "\"\necho noisy\nexit 1\n"
				if err := os.WriteFile(filepath.Join(binDir, "theme-toggle"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			tmux := startIsolatedTmux(t, "PATH="+binDir)
			paneOut, err := tmux("display-message", "-p", "-t", "w", "#{pane_id}").Output()
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			pane := strings.TrimSpace(string(paneOut))

			cmds, err := verbs["theme"].build(pane, "@0", "w", []string{"light"})
			if err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(dir, "cmd.conf")
			if err := os.WriteFile(conf, []byte(cmds[0]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
				t.Fatalf("source-file: %v\n%s", err, out)
			}

			if !tc.toggle {
				time.Sleep(time.Second)
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(marker); err == nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("theme-toggle stub never ran")
				}
				time.Sleep(300 * time.Millisecond)
			}

			out, err := tmux("display-message", "-p", "-t", pane, "#{pane_in_mode}").Output()
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != "0" {
				shown, _ := tmux("capture-pane", "-p", "-M", "-t", pane).CombinedOutput()
				t.Errorf("pane_in_mode = %q, want 0 — the job's output or exit status reached the pane:\n%s", got, shown)
			}
		})
	}
}

// Nothing under nix flake check ever runs this body against a real remote
// run-shell, so these substring assertions are the only regression net the two
// emptiness guards will ever have — and what they prevent is severe: an empty
// branch falls through tmux-pr-enrich.sh:463 into tick mode, where --force
// detaches a whole-server run_full_pass, and an empty dir skips the conditional
// cd at :234, writing another repo's PR onto the remote window's own @pr_*.
func TestEnrichRefreshVerbGuardsBranchAndDir(t *testing.T) {
	v, ok := verbs["enrich-refresh"]
	if !ok {
		t.Fatal("no enrich-refresh verb")
	}
	script := enrichRefreshScript("@2")
	if strings.Contains(script, "'") {
		t.Fatalf("refresh script must have zero single quotes: %q", script)
	}
	for _, want := range []string{`[ -n "$b" ]`, `[ -n "$d" ]`, "|| exit 0"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script %q missing %q: an empty branch runs a whole-server pass, an empty dir writes another repo's PR", script, want)
		}
	}
	// run-shell format-expands the whole string before /bin/sh sees it, so the
	// body must carry no sequence tmux would read as a format — #P in
	// particular would corrupt ${p#*=} into ${p1ATH=}.
	for _, bad := range []string{"#{", "#(", "#P", "#S", "#W", "#T", "#D", "#F", "#I", "#H"} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains tmux format %q, which run-shell would expand: %q", bad, script)
		}
	}

	cmds, err := v.build("%5", "@2", "my proj", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("want one command, got %v", cmds)
	}
	wantCmd := fmt.Sprintf("run-shell -b -t %%5 %s", tmuxQuote("exec /bin/sh -c "+tmuxQuote(script)))
	if cmds[0] != wantCmd {
		t.Fatalf("command\n got %q\nwant %q", cmds[0], wantCmd)
	}
	// The poller's target is win, never <sess>:<win>: RemoteSession may hold
	// spaces, and quoting it here needs the single quotes this body bans.
	if !strings.Contains(cmds[0], "--target @2") {
		t.Errorf("command %q must target the remote window id", cmds[0])
	}
	if strings.Contains(cmds[0], "my proj") {
		t.Errorf("command %q must not carry the remote session name", cmds[0])
	}
	if v.args != 0 || v.moves || v.layout || v.windows || v.needsView || v.probe {
		t.Error("the poller takes no arguments, opens nothing and reports through a label row: no reconcile intent")
	}
}

// parseCtl refuses a pane it cannot map to a window, and the refusal reaches the
// user as a --display-error banner — so a gesture inside a mirrored float has to
// resolve from the moment the float exists. Two windows in the mapping: the
// reconcile's setWindowPanes clears every pane mapped to its own window before
// re-setting, and the float has to come back with them.
//
// Mid-pass as well as after: reconcileLayout's trailing re-read can send it round
// again, and the in-loop assertion is what keeps the float routable meanwhile.
// The probe rides the round-trip seam, which is the only place inside the loop a
// test can observe.
func TestCtlResolvesAMirroredFloatDuringAReconcile(t *testing.T) {
	f := &layoutTmux{windowID: "@101\n"}
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1"}
	w.localPanes = []string{"%l0", "%l1"}
	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9
	w.layout = "stale"

	c := newCtlState()
	c.setWindowPanes("@1", w.allRemotePanes())

	base := setupWindowRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + tiledFloatLayout + " %0 0", "%end 1 1 1", // readLayout
		"%begin 1 2 1", "@1 " + tiledFloatLayout + " %0 0", "%end 1 2 1", // trailing re-read: converged
	}, "\n") + "\n")
	reads := 0
	var midPass error
	rt := func(cmds ...string) replies {
		for _, cmd := range cmds {
			if !strings.Contains(cmd, "window_layout") {
				continue
			}
			reads++
			// The trailing re-read of the first pass: the in-loop
			// setWindowPanes has run, the post-loop one has not.
			if reads == 2 {
				_, midPass = c.parseCtl([]string{wire.CtlProtocolVersion, "zoom", "%9"}, "rem")
			}
		}
		return base(cmds...)
	}

	reconcileLayout(f.config(), w, func(string) {}, NewRouter(), noHellos, c, newConverger(), rt)

	if reads != 2 {
		t.Fatalf("%d layout reads, want 2 — the probe never ran mid-pass", reads)
	}
	if midPass != nil {
		t.Errorf("mid-pass zoom on the float: %v", midPass)
	}
	if _, err := c.parseCtl([]string{wire.CtlProtocolVersion, "zoom", "%9"}, "rem"); err != nil {
		t.Errorf("zoom on the float after the reconcile: %v", err)
	}
}

// A float the reconcile itself created has to be routable too: reconcileFloats
// runs after the pass loop, so only the trailing re-assert can know about it.
func TestCtlResolvesAFloatAddedByTheReconcile(t *testing.T) {
	f := &layoutTmux{windowID: "@101\n", newPaneIDs: []string{"%l9\n"}}
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1"}
	w.localPanes = []string{"%l0", "%l1"}
	w.layout = "stale"

	c := newCtlState()
	c.setWindowPanes("@1", w.allRemotePanes())

	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	go io.Copy(io.Discard, peer)

	rt := setupWindowRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + tiledFloatLayout + " %0 0", "%end 1 1 1", // readLayout: the float is already there
		"%begin 1 2 1", "@1 " + tiledFloatLayout + " %0 0", "%end 1 2 1", // trailing re-read: converged
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // the new float's seed: cursor
		"%begin 1 4 1", "FLOAT", "%end 1 4 1", // the new float's seed: capture
	}, "\n") + "\n")

	reconcileLayout(f.config(), w, func(string) {}, NewRouter(), hellos(map[string]net.Conn{"%9": conn}), c, newConverger(), rt)

	if w.localFloats["%9"] != "%l9" {
		t.Fatalf("localFloats = %v, want the reconcile to have mirrored %%9", w.localFloats)
	}
	if _, err := c.parseCtl([]string{wire.CtlProtocolVersion, "zoom", "%9"}, "rem"); err != nil {
		t.Errorf("zoom on the freshly mirrored float: %v", err)
	}
}

// pressAgain is the exact user-facing nack a carousel press gets while the
// bridge re-dials, spelled out here rather than built from pressAgainErr: it
// reaches the user through display-message, so a silent change to it is a
// change to what the status line says.
const pressAgain = "re-dialling for foot — press again"

// carouselPress is the argv the prefix + I bind sends for a mirrored pane.
func carouselPress() []string {
	return []string{wire.CtlProtocolVersion, "carousel", "%3"}
}

// handlerFixture is one mirrored window, a seam whose viewer has switched to
// foot while xterm-kitty is still advertised, and a recorder for whatever the
// handler submits.
func handlerFixture(t *testing.T, adv, viewer string) (*ctlState, *viewReplacer, *Viewing, *[]string) {
	t.Helper()
	cst := newCtlStateWith("@1", "%3")
	rep, view, resolved := replacerFixture(adv, true)
	*resolved = viewer
	var sent []string
	return cst, rep, view, &sent
}

func sender(sent *[]string) func(...string) bool {
	return func(cmds ...string) bool {
		*sent = append(*sent, cmds...)
		return true
	}
}

// Acceptance 2: the common press — the viewer is the terminal the control
// client already advertises — must run the carousel and never be nacked.
func TestHandleCtlSubmitsWhenTheViewerMatches(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "foot", "foot")

	if err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent)); err != nil {
		t.Fatalf("handleCtl: %v, want no error", err)
	}
	if len(*sent) != 1 {
		t.Errorf("sent = %q, want the one carousel command", *sent)
	}
	if n := wakeUps(rep); n != 0 {
		t.Errorf("wake-ups = %d, want none", n)
	}
}

// A stale viewing identity nacks and raises, and submits nothing: the gesture
// must not run under a control client advertising a terminal the user is no
// longer looking through.
func TestHandleCtlNacksAndRaisesOnAStaleViewer(t *testing.T) {
	cst, rep, view, sent := handlerFixture(t, "xterm-kitty", "foot")

	err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent))
	if err == nil || err.Error() != pressAgain {
		t.Fatalf("error = %v, want %q", err, pressAgain)
	}
	if len(*sent) != 0 {
		t.Errorf("sent = %q, want nothing submitted", *sent)
	}
	if n := wakeUps(rep); n != 1 {
		t.Errorf("wake-ups = %d, want exactly 1", n)
	}
	// No intent may be registered either, or the next drain reconciles a
	// window for a command that was never sent.
	if windows, layouts, reseeds := cst.takeIntents(); windows || len(layouts) != 0 || len(reseeds) != 0 {
		t.Errorf("intents = (%v, %v, %v), want none", windows, layouts, reseeds)
	}
	if got := view.Desired(); got != "foot" {
		t.Errorf("Desired = %q, want foot — the raised dial reads it", got)
	}
}

// R8: a second press during the window is the likeliest user behaviour, and it
// must read as the same instruction, never as "your bridge is broken".
func TestHandleCtlGivesAnInFlightPressTheSameText(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "xterm-kitty", "foot")

	first := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent))
	second := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent))
	if first == nil || second == nil {
		t.Fatalf("errors = (%v, %v), want both nacked", first, second)
	}
	if first.Error() != second.Error() {
		t.Errorf("texts differ:\n first  = %q\n second = %q", first, second)
	}
	if n := wakeUps(rep); n != 1 {
		t.Errorf("wake-ups = %d, want 1 — the second press raises nothing", n)
	}
	if len(*sent) != 0 {
		t.Errorf("sent = %q, want nothing submitted", *sent)
	}
}

// The press after a completed replacement is the deterministic one the nack
// promises: Advertised now names what the new client carries, so it submits.
func TestHandleCtlSubmitsAfterAReplacementCompleted(t *testing.T) {
	cst, rep, view, sent := handlerFixture(t, "xterm-kitty", "foot")

	if err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent)); err == nil {
		t.Fatal("first press was not nacked")
	}
	// What replaceConn does at its publish point, and what the attach loop
	// does once it returns.
	view.setAdvertised("foot")
	rep.done()

	if err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent)); err != nil {
		t.Fatalf("second press: %v, want it to submit", err)
	}
	if len(*sent) != 1 {
		t.Errorf("sent = %q, want the one carousel command", *sent)
	}
}

// A replacement that never landed leaves Advertised untouched, which is what
// makes the next press raise again rather than run against the old backend
// forever (acceptance 7).
func TestHandleCtlRaisesAgainAfterAFailedReplacement(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "xterm-kitty", "foot")

	if err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent)); err == nil {
		t.Fatal("first press was not nacked")
	}
	// replaceConn's notReplaced path: the loop took the wake-up, the dial
	// published nothing and advertised nothing, and done() re-arms the seam.
	if n := wakeUps(rep); n != 1 {
		t.Fatalf("wake-ups = %d, want 1 from the first press", n)
	}
	rep.done()

	err := handleCtl(cst, rep, newCarouselProbe(), carouselPress(), "rem", nil, sender(sent))
	if err == nil || err.Error() != pressAgain {
		t.Fatalf("error = %v, want %q again", err, pressAgain)
	}
	if n := wakeUps(rep); n != 1 {
		t.Errorf("wake-ups = %d, want the second press to raise again — a failure must not latch", n)
	}
	if len(*sent) != 0 {
		t.Errorf("sent = %q, want nothing submitted", *sent)
	}
}

// Only the verb the table marks needsView consults the viewing identity: a
// split does not care which terminal the control client advertises, so a stale
// one must not cost it a nack.
func TestHandleCtlLeavesOtherVerbsAloneOnAStaleViewer(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "xterm-kitty", "foot")

	argv := []string{wire.CtlProtocolVersion, "split-h", "%3"}
	if err := handleCtl(cst, rep, newCarouselProbe(), argv, "rem", nil, sender(sent)); err != nil {
		t.Fatalf("handleCtl: %v, want no error", err)
	}
	if len(*sent) != 1 {
		t.Errorf("sent = %q, want the one split command", *sent)
	}
	if n := wakeUps(rep); n != 0 {
		t.Errorf("wake-ups = %d, want none", n)
	}
}

// floatDragPastEdge is the float-geom request for a float flush against the
// left edge dragged further left: the clamp moves it, so the verb carries
// local commands.
func floatDragPastEdge() []string {
	return []string{wire.CtlProtocolVersion, "float-geom", "%3", "%9", "-7", "6", "38", "10", "100", "30"}
}

// The local float is put on the clamped box before the remote command goes
// out, so a reconcile that remote command triggers lands after it and wins.
func TestHandleCtlRunsLocalCommandsBeforeTheRemoteSend(t *testing.T) {
	cst, rep, _, _ := handlerFixture(t, "foot", "foot")
	var log []string
	local := func(args ...string) error {
		log = append(log, "local "+strings.Join(args, " "))
		return nil
	}
	send := func(cmds ...string) bool {
		for _, c := range cmds {
			log = append(log, "send "+c)
		}
		return true
	}

	if err := handleCtl(cst, rep, newCarouselProbe(), floatDragPastEdge(), "rem", local, send); err != nil {
		t.Fatalf("handleCtl: %v, want no error", err)
	}
	want := []string{
		"local " + strings.Join(wantLocalFloatResize("%9", 40, 12), " "),
		"local move-pane -t %9 -X 0 -Y 5",
		"send " + wantFloatGeom("%3", 1, 6, 38, 10),
	}
	if !reflect.DeepEqual(log, want) {
		t.Errorf("order = %q, want %q", log, want)
	}
}

// A local apply that fails nacks the drag and sends nothing: the remote
// command alone would leave the local float where the drag left it.
func TestHandleCtlSendsNothingWhenALocalCommandFails(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "foot", "foot")
	boom := errors.New("can't find pane: %9")
	local := func(...string) error { return boom }

	err := handleCtl(cst, rep, newCarouselProbe(), floatDragPastEdge(), "rem", local, sender(sent))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	if len(*sent) != 0 {
		t.Errorf("sent = %q, want nothing submitted", *sent)
	}
	if windows, layouts, reseeds := cst.takeIntents(); windows || len(layouts) != 0 || len(reseeds) != 0 {
		t.Errorf("intents = (%v, %v, %v), want none", windows, layouts, reseeds)
	}
}

// Only tmux's own -F evaluation can show the guard's order, size and zoom
// clauses actually refuse.
func TestTileLayoutCommandGuardsOnLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also puts tmux in nativeBuildInputs — so a missing tmux there
		// means that input was pruned, not that this is a dev machine.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))

	if out, err := tmux("split-window", "-d", "-h", "-t", "w").CombinedOutput(); err != nil {
		t.Fatalf("split-window: %v\n%s", err, out)
	}
	idsOut, err := tmux("list-panes", "-t", "w", "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	ids := strings.Fields(string(idsOut))
	if len(ids) != 2 {
		t.Fatalf("pane ids = %v, want 2", ids)
	}
	id0, id1 := ids[0], ids[1]
	digits0, digits1 := strings.TrimPrefix(id0, "%"), strings.TrimPrefix(id1, "%")

	confDir := t.TempDir()
	// apply writes tileLayoutCommand's production output to a file and
	// sources it, the same way the control connection's command line would
	// reach tmux's own parser.
	apply := func(t *testing.T, L controlmode.Layout) {
		t.Helper()
		conf := filepath.Join(confDir, "cmd.conf")
		if err := os.WriteFile(conf, []byte(tileLayoutCommand(id0, L)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := tmux("source-file", conf).CombinedOutput(); err != nil {
			t.Fatalf("source-file: %v\n%s", err, out)
		}
	}
	paneWidths := func(t *testing.T) map[string]string {
		t.Helper()
		out, err := tmux("list-panes", "-t", "w", "-F", "#{pane_id}|#{pane_width}").Output()
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		widths := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			id, w, ok := strings.Cut(line, "|")
			if !ok {
				t.Fatalf("list-panes line %q missing '|'", line)
			}
			widths[id] = w
		}
		return widths
	}
	// reset re-splits evenly before each subtest and returns the resulting
	// widths, so a "no-op" assertion compares against tmux's own numbers
	// rather than a hardcoded guess.
	reset := func(t *testing.T) map[string]string {
		t.Helper()
		if out, err := tmux("select-layout", "-t", "w", "even-horizontal").CombinedOutput(); err != nil {
			t.Fatalf("select-layout even-horizontal: %v\n%s", err, out)
		}
		return paneWidths(t)
	}
	windowSize := func(t *testing.T) string {
		t.Helper()
		out, err := tmux("display-message", "-p", "-t", "w", "#{window_width}x#{window_height}").Output()
		if err != nil {
			t.Fatalf("display-message: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	zoomedFlag := func(t *testing.T) string {
		t.Helper()
		out, err := tmux("display-message", "-p", "-t", "w", "#{window_zoomed_flag}").Output()
		if err != nil {
			t.Fatalf("display-message: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("matching order, size, unzoomed applies", func(t *testing.T) {
		reset(t)
		L, err := tiledArg(fmt.Sprintf("0000,80x24,0,0{30x24,0,0,%s,49x24,31,0,%s}", digits0, digits1))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, L)
		if got := paneWidths(t)[id0]; got != "30" {
			t.Errorf("pane %s width = %s, want 30", id0, got)
		}
	})

	t.Run("swapped order is a no-op", func(t *testing.T) {
		before := reset(t)
		L, err := tiledArg(fmt.Sprintf("0000,80x24,0,0{30x24,0,0,%s,49x24,31,0,%s}", digits1, digits0))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, L)
		if got := paneWidths(t); got[id0] != before[id0] || got[id1] != before[id1] {
			t.Errorf("widths = %v, want unchanged from %v", got, before)
		}
	})

	t.Run("size mismatch is a no-op", func(t *testing.T) {
		before := reset(t)
		L, err := tiledArg(fmt.Sprintf("0000,90x24,0,0{40x24,0,0,%s,49x24,41,0,%s}", digits0, digits1))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, L)
		if got := paneWidths(t); got[id0] != before[id0] || got[id1] != before[id1] {
			t.Errorf("widths = %v, want unchanged from %v", got, before)
		}
		if got := windowSize(t); got != "80x24" {
			t.Errorf("window size = %s, want unchanged 80x24", got)
		}
	})

	t.Run("zoomed is a no-op", func(t *testing.T) {
		reset(t)
		if out, err := tmux("resize-pane", "-Z", "-t", id0).CombinedOutput(); err != nil {
			t.Fatalf("resize-pane -Z: %v\n%s", err, out)
		}
		L, err := tiledArg(fmt.Sprintf("0000,80x24,0,0{30x24,0,0,%s,49x24,31,0,%s}", digits0, digits1))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, L)
		if got := zoomedFlag(t); got != "1" {
			t.Errorf("window_zoomed_flag = %s, want still 1", got)
		}
	})
}
