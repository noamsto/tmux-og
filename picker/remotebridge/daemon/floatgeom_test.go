package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestFloatOuterFromCellProbeRoundTrip pins the measured tmux next-3.8 probe:
// `new-pane -x 60 -y 20 -X 10 -Y 5` yields cell `58x18,11,6`, so the inverse
// must recover the original flags.
func TestFloatOuterFromCellProbeRoundTrip(t *testing.T) {
	c := controlmode.PaneCell{ID: "%2", W: 58, H: 18, X: 11, Y: 6}
	w, h, x, y := outerFromCell(c, 190, 45)
	if w != 60 || h != 20 || x != 10 || y != 5 {
		t.Errorf("outerFromCell = (%d,%d,%d,%d), want (60,20,10,5)", w, h, x, y)
	}
}

func TestFloatOuterFromCellClampsNegativeOffset(t *testing.T) {
	c := controlmode.PaneCell{ID: "%0", W: 40, H: 20, X: 0, Y: 0}
	_, _, x, y := outerFromCell(c, 190, 45)
	if x != 0 || y != 0 {
		t.Errorf("outerFromCell offset = (%d,%d), want (0,0)", x, y)
	}
}

// TestFloatOuterFromCellClampsToWindow covers a `-B none` remote float: a
// zero-inset cell already spanning the whole window would otherwise size the
// outer box past the window's own edge.
func TestFloatOuterFromCellClampsToWindow(t *testing.T) {
	c := controlmode.PaneCell{ID: "%0", W: 190, H: 45, X: 0, Y: 0}
	w, h, x, y := outerFromCell(c, 190, 45)
	if w != 190 || h != 45 {
		t.Errorf("outerFromCell size = (%d,%d), want (190,45)", w, h)
	}
	if x != 0 || y != 0 {
		t.Errorf("outerFromCell offset = (%d,%d), want (0,0)", x, y)
	}
}

// A `-B none` remote float flush against the far edge: the cell's own X plus
// the border the local mirror grows would put the outer box's right edge one
// past the window, so the offset has to give way rather than each axis clamping
// on its own.
func TestFloatOuterFromCellKeepsTheBoxInsideTheWindow(t *testing.T) {
	c := controlmode.PaneCell{ID: "%0", W: 50, H: 20, X: 140, Y: 25}
	w, h, x, y := outerFromCell(c, 190, 45)
	if x+w > 190 || y+h > 45 {
		t.Errorf("outerFromCell = (%d,%d,%d,%d), want a box inside 190x45", w, h, x, y)
	}
	if w != 52 || h != 22 || x != 138 || y != 23 {
		t.Errorf("outerFromCell = (%d,%d,%d,%d), want (52,22,138,23)", w, h, x, y)
	}
}

func TestClampInner(t *testing.T) {
	tests := []struct {
		name       string
		c          controlmode.PaneCell
		winW, winH int
		want       controlmode.PaneCell
	}{
		{
			name: "in-window cell round-trips unchanged",
			c:    controlmode.PaneCell{ID: "%3", X: 11, Y: 6, W: 38, H: 10},
			winW: 100, winH: 30,
			want: controlmode.PaneCell{ID: "%3", X: 11, Y: 6, W: 38, H: 10},
		},
		{
			name: "dragged off the left edge clamps the offset",
			c:    controlmode.PaneCell{ID: "%3", X: -5, Y: 6, W: 45, H: 10},
			winW: 100, winH: 30,
			want: controlmode.PaneCell{ID: "%3", X: 1, Y: 6, W: 45, H: 10},
		},
		{
			name: "past the bottom-right edge clamps both offsets",
			c:    controlmode.PaneCell{ID: "%3", X: 70, Y: 25, W: 38, H: 10},
			winW: 100, winH: 30,
			want: controlmode.PaneCell{ID: "%3", X: 61, Y: 19, W: 38, H: 10},
		},
		{
			name: "wider than the window clamps the size and the offset",
			c:    controlmode.PaneCell{ID: "%3", X: 0, Y: 6, W: 120, H: 10},
			winW: 100, winH: 30,
			want: controlmode.PaneCell{ID: "%3", X: 1, Y: 6, W: 98, H: 10},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clampInner(tc.c, tc.winW, tc.winH)
			if got != tc.want {
				t.Errorf("clampInner = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFloatCreateArgv(t *testing.T) {
	c := controlmode.PaneCell{ID: "%2", W: 58, H: 18, X: 11, Y: 6}
	got := floatCreateArgv("@7", c, 190, 45)
	want := []string{
		"new-pane", "-d", "-P", "-F", "#{pane_id}",
		"-t", "@7",
		"-B", "heavy", "-A",
		"-x", "60", "-y", "20",
		"-X", "10", "-Y", "5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("floatCreateArgv =\n%v\nwant\n%v", got, want)
	}
}

// wantResizeTrigger is floatResizeTrigger pinned literally: resize-pane -y's
// pane-border-status bump condition (cmd-resize-pane.c) read off the pane.
const wantResizeTrigger = "#{||:#{&&:#{==:#{pane-border-status},top},#{==:#{pane_top},1}},#{&&:#{==:#{pane-border-status},bottom},#{==:#{e|+:#{pane_top},#{pane_height}},#{e|-:#{window_height},1}}}}"

func TestFloatResizeArgv(t *testing.T) {
	c := controlmode.PaneCell{ID: "%2", W: 58, H: 18, X: 11, Y: 6}
	got := floatResizeArgv("%9", c, 190, 45)
	want := []string{"if-shell", "-t", "%9", "-F", "#{pane_floating_flag}",
		"if-shell -t %9 -F '" + wantResizeTrigger + "' 'resize-pane -t %9 -x 60 -y 19' 'resize-pane -t %9 -x 60 -y 20'"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("floatResizeArgv = %v, want %v", got, want)
	}
}

func TestFloatMoveArgv(t *testing.T) {
	c := controlmode.PaneCell{ID: "%2", W: 58, H: 18, X: 11, Y: 6}
	got := floatMoveArgv("%9", c, 190, 45)
	want := []string{"move-pane", "-t", "%9", "-X", "10", "-Y", "5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("floatMoveArgv = %v, want %v", got, want)
	}
}

func TestFloatGeomStamp(t *testing.T) {
	c := controlmode.PaneCell{ID: "%2", W: 58, H: 18, X: 11, Y: 6}
	got := floatGeomStamp(c, 190, 45)
	want := "60 20 10 5"
	if got != want {
		t.Errorf("floatGeomStamp = %q, want %q", got, want)
	}
}

// TestFloatResizeExactUnderPaneBorderStatusLiveTmux drives the float resize
// commands against a real server on every row where resize-pane -y's
// pane-border-status bump fires, and on one where it does not: both the
// float-geom verb's remote command and the reconcile's resize+move argv must
// land on exactly the requested box, whichever row the float starts from.
func TestFloatResizeExactUnderPaneBorderStatusLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))
	run := func(args ...string) string {
		t.Helper()
		out, err := tmux(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	geom := func(pane string) controlmode.PaneCell {
		t.Helper()
		var c controlmode.PaneCell
		f := strings.Split(run("display-message", "-p", "-t", pane, "#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}"), "|")
		if len(f) != 4 {
			t.Fatalf("geometry of %s = %q", pane, f)
		}
		for i, dst := range []*int{&c.X, &c.Y, &c.W, &c.H} {
			n, err := strconv.Atoi(f[i])
			if err != nil {
				t.Fatalf("geometry of %s = %q: %v", pane, f, err)
			}
			*dst = n
		}
		c.ID = pane
		return c
	}
	var winW, winH int
	_, _ = fmt.Sscanf(run("display-message", "-p", "-t", "w", "#{window_width} #{window_height}"), "%d %d", &winW, &winH)

	// Rows are the float's inner pane_top; the trigger is on the inner box, so
	// the same rows fire for a bordered and a borderless float.
	const h = 10
	rows := []struct {
		name string
		y    int
	}{
		{"top trigger", 1},
		{"bottom trigger", winH - 1 - h},
		{"mid", 6},
	}
	neutral := controlmode.PaneCell{X: 20, Y: 4, W: 30, H: 8}
	mid := controlmode.PaneCell{X: 15, Y: 7, W: 34, H: 7}

	for _, status := range []string{"top", "bottom", "off", "top-floating"} {
		for _, border := range []string{"heavy", "none"} {
			inset := 1
			if border == "none" {
				inset = 0
			}
			// float creates a float whose inner box is c; new-pane -y has no
			// pane-border-status bump, so it lands exactly.
			float := func(c controlmode.PaneCell) string {
				t.Helper()
				p := newPane(t, tmux, "new-pane", "-d", "-P", "-F", "#{pane_id}", "-t", "w", "-B", border,
					"-x", strconv.Itoa(c.W+2*inset), "-y", strconv.Itoa(c.H+2*inset),
					"-X", strconv.Itoa(c.X-inset), "-Y", strconv.Itoa(c.Y-inset), "sleep 300")
				if got := geom(p); got != (controlmode.PaneCell{ID: p, X: c.X, Y: c.Y, W: c.W, H: c.H}) {
					t.Fatalf("created float = %+v, want %+v", got, c)
				}
				return p
			}
			// cellFor is the cell whose outer box, as floatResizeArgv and
			// floatMoveArgv derive it, puts this float's inner box on want.
			cellFor := func(want controlmode.PaneCell) controlmode.PaneCell {
				d := 1 - inset
				return controlmode.PaneCell{X: want.X + d, Y: want.Y + d, W: want.W - 2*d, H: want.H - 2*d}
			}
			local := func(p string, want controlmode.PaneCell) {
				t.Helper()
				c := cellFor(want)
				run(floatResizeArgv(p, c, winW, winH)...)
				run(floatMoveArgv(p, c, winW, winH)...)
			}
			for _, row := range rows {
				target := controlmode.PaneCell{X: 11, Y: row.y, W: 38, H: h}
				name := fmt.Sprintf("%s/%s/%s", status, border, row.name)
				t.Run(name, func(t *testing.T) {
					run("set-option", "-w", "-t", "w", "pane-border-status", status)
					check := func(what, p string, want controlmode.PaneCell) {
						t.Helper()
						want.ID = p
						if got := geom(p); got != want {
							t.Errorf("%s: float = %d,%d %dx%d, want %d,%d %dx%d",
								what, got.X, got.Y, got.W, got.H, want.X, want.Y, want.W, want.H)
						}
					}

					// From elsewhere, and from the same row with only the width
					// changing (a side-border drag), where the move is a no-op and
					// the resize runs on the row itself.
					narrower := target
					narrower.W -= 4
					for _, start := range []controlmode.PaneCell{neutral, narrower} {
						p := float(start)
						script := filepath.Join(t.TempDir(), "geom.conf")
						if err := os.WriteFile(script, []byte(floatGeomCommand(p, target)+"\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						run("source-file", script)
						check(fmt.Sprintf("float-geom command from %d,%d %dx%d", start.X, start.Y, start.W, start.H), p, target)
						run("kill-pane", "-t", p)
					}

					p := float(target)
					local(p, mid)
					check("resize+move off the row", p, mid)
					run("kill-pane", "-t", p)

					p = float(mid)
					local(p, target)
					check("resize+move onto the row", p, target)
					run("kill-pane", "-t", p)
				})
			}
		}
	}
}

// A mirror pane re-tiled while a Move is pending must not be resized as a
// tiled pane: floatResizeArgv's pane_floating_flag guard leaves it alone.
func TestFloatResizeArgvLeavesATiledPaneAloneLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))
	p := newPane(t, tmux, "split-window", "-d", "-h", "-P", "-F", "#{pane_id}", "-t", "w")
	size := func() string {
		t.Helper()
		out, err := tmux("list-panes", "-t", "w", "-F", "#{pane_id}|#{pane_width}x#{pane_height}").Output()
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	before := size()
	argv := floatResizeArgv(p, controlmode.PaneCell{ID: p, X: 5, Y: 3, W: 20, H: 6}, 80, 24)
	if out, err := tmux(argv...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	if got := size(); got != before {
		t.Errorf("tiled panes after floatResizeArgv = %q, want unchanged %q", got, before)
	}
}
