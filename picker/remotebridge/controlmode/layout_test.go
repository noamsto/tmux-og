package controlmode

import (
	"fmt"
	"testing"
)

func TestParseLayout(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantW   int
		wantH   int
		wantIDs []string
		wantP0  PaneCell
	}{
		{
			name:  "single pane",
			in:    "bd67,190x45,0,0,3",
			wantW: 190, wantH: 45,
			wantIDs: []string{"%3"},
			wantP0:  PaneCell{ID: "%3", W: 190, H: 45, X: 0, Y: 0},
		},
		{
			name:  "horizontal split (left-right)",
			in:    "4ed4,190x45,0,0{95x45,0,0,0,94x45,96,0,1}",
			wantW: 190, wantH: 45,
			wantIDs: []string{"%0", "%1"},
			wantP0:  PaneCell{ID: "%0", W: 95, H: 45, X: 0, Y: 0},
		},
		{
			name:  "vertical split (top-bottom)",
			in:    "b5e9,190x45,0,0[190x22,0,0,0,190x22,0,23,1]",
			wantW: 190, wantH: 45,
			wantIDs: []string{"%0", "%1"},
			wantP0:  PaneCell{ID: "%0", W: 190, H: 22, X: 0, Y: 0},
		},
		{
			name:  "nested",
			in:    "a1b2,190x45,0,0{95x45,0,0,0,94x45,96,0[94x22,96,0,1,94x22,96,23,2]}",
			wantW: 190, wantH: 45,
			wantIDs: []string{"%0", "%1", "%2"},
			wantP0:  PaneCell{ID: "%0", W: 95, H: 45, X: 0, Y: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLayout(tt.in)
			if err != nil {
				t.Fatalf("ParseLayout(%q) error: %v", tt.in, err)
			}
			if got.W != tt.wantW || got.H != tt.wantH {
				t.Errorf("window dims = %dx%d, want %dx%d", got.W, got.H, tt.wantW, tt.wantH)
			}
			var ids []string
			for _, p := range got.Panes {
				ids = append(ids, p.ID)
			}
			if len(ids) != len(tt.wantIDs) {
				t.Fatalf("pane ids = %v, want %v", ids, tt.wantIDs)
			}
			for i := range ids {
				if ids[i] != tt.wantIDs[i] {
					t.Errorf("pane[%d] id = %s, want %s", i, ids[i], tt.wantIDs[i])
				}
			}
			if got.Panes[0] != tt.wantP0 {
				t.Errorf("pane[0] = %+v, want %+v", got.Panes[0], tt.wantP0)
			}
			if got.Raw != tt.in {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.in)
			}
		})
	}
}

func TestParseLayoutError(t *testing.T) {
	for _, in := range []string{"", "nocomma", "bd67,notdims,0,0,3"} {
		if _, err := ParseLayout(in); err == nil {
			t.Errorf("ParseLayout(%q) expected error, got nil", in)
		}
	}
}

// TestParseLayoutFloats uses strings captured live from the pinned
// tmux-next-3.8, not hand-written ones — tmux interleaves float panes into
// the tiled split tree AND repeats them in a trailing "<...>" section, and
// rejects that exact string when replayed through select-layout (verified
// separately). ParseLayout must recover the tiled-only tree (for Raw / Panes
// / RemotePaneOrder) and expose the floats separately.
func TestParseLayoutFloats(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantTiled  []string // Panes ids, in order
		wantFloats []PaneCell
		wantRaw    string // reconstructed select-layout-safe Raw
	}{
		{
			// One tiled pane (%1) + one float (%2). tmux wraps the sole
			// tiled pane in a framing split purely to carry the float
			// sibling; pruning must collapse that split back to a plain
			// single-pane cell, matching what 3.7b (no floats) emits.
			name:       "single tiled pane with one float",
			in:         "e2cc,80x24,0,0[80x24,0,0,1,30x7,9,3,2]<30x7,9,3,2>",
			wantTiled:  []string{"%1"},
			wantFloats: []PaneCell{{ID: "%2", W: 30, H: 7, X: 9, Y: 3}},
			wantRaw:    "b25e,80x24,0,0,1",
		},
		{
			// Two tiled panes (%0, %1, horizontal split) + one float (%2)
			// interleaved between them in the tree.
			name:       "two tiled panes with one float",
			in:         "aaee,80x24,0,0{40x24,0,0,0,30x7,9,3,2,39x24,41,0,1}<30x7,9,3,2>",
			wantTiled:  []string{"%0", "%1"},
			wantFloats: []PaneCell{{ID: "%2", W: 30, H: 7, X: 9, Y: 3}},
			wantRaw:    "8205,80x24,0,0{40x24,0,0,0,39x24,41,0,1}",
		},
		{
			// Two tiled panes + two floats: the trailing section holds
			// multiple cells comma-joined inside a single "<...>", not one
			// "<...>" per float.
			name:      "two tiled panes with two floats",
			in:        "a012,80x24,0,0{40x24,0,0,0,14x2,49,15,3,30x7,9,3,2,39x24,41,0,1}<14x2,49,15,3,30x7,9,3,2>",
			wantTiled: []string{"%0", "%1"},
			wantFloats: []PaneCell{
				{ID: "%3", W: 14, H: 2, X: 49, Y: 15},
				{ID: "%2", W: 30, H: 7, X: 9, Y: 3},
			},
			wantRaw: "8205,80x24,0,0{40x24,0,0,0,39x24,41,0,1}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLayout(tt.in)
			if err != nil {
				t.Fatalf("ParseLayout(%q) error: %v", tt.in, err)
			}
			var ids []string
			for _, p := range got.Panes {
				ids = append(ids, p.ID)
			}
			if len(ids) != len(tt.wantTiled) {
				t.Fatalf("tiled pane ids = %v, want %v", ids, tt.wantTiled)
			}
			for i := range ids {
				if ids[i] != tt.wantTiled[i] {
					t.Errorf("tiled pane[%d] id = %s, want %s", i, ids[i], tt.wantTiled[i])
				}
			}
			if len(got.Floats) != len(tt.wantFloats) {
				t.Fatalf("floats = %+v, want %+v", got.Floats, tt.wantFloats)
			}
			for i := range got.Floats {
				if got.Floats[i] != tt.wantFloats[i] {
					t.Errorf("float[%d] = %+v, want %+v", i, got.Floats[i], tt.wantFloats[i])
				}
			}
			if got.Raw != tt.wantRaw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.wantRaw)
			}
		})
	}
}

// TestParseLayoutNoFloats pins that a layout with no floating panes (the
// tmux 3.7b shape, and the common next-3.8 shape) is unaffected: Raw stays
// byte-identical to the input.
func TestParseLayoutNoFloats(t *testing.T) {
	in := "8205,80x24,0,0{40x24,0,0,0,39x24,41,0,1}"
	got, err := ParseLayout(in)
	if err != nil {
		t.Fatalf("ParseLayout(%q) error: %v", in, err)
	}
	if len(got.Floats) != 0 {
		t.Errorf("Floats = %+v, want none", got.Floats)
	}
	if got.Raw != in {
		t.Errorf("Raw = %q, want %q", got.Raw, in)
	}
}

// TestParseLayoutV2 uses strings captured from the pinned tmux on a 100x30
// window: "in" is a CLI client's #{window_layout}, and wantRaw is a control
// client's v1 dump of the same window (no new-layouts flag), so Raw is
// checked against tmux's own tiled-only serialization.
func TestParseLayoutV2(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantW      int
		wantH      int
		wantTiled  []string
		wantFloats []PaneCell
		wantRaw    string
	}{
		{
			// h{p, p}, no floats.
			name:  "two tiled panes, no floats",
			in:    `{"V":2,"L":{"t":"h","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":50,"h":30,"x":0,"y":0,"l":0,"i":0,"I":"%0"},{"t":"p","w":49,"h":30,"x":51,"y":0,"a":true,"i":1,"I":"%1"}]}}`,
			wantW: 100, wantH: 30,
			wantTiled: []string{"%0", "%1"},
			wantRaw:   "6b8b,100x30,0,0{50x30,0,0,0,49x30,51,0,1}",
		},
		{
			// h{p, v{p,p}} then a float created while the v{...} split's
			// pane was active: tmux attaches it as a third child of that
			// split, not the root; captured as-is.
			name:  "nested split with a float attached to the inner split",
			in:    `{"V":2,"L":{"t":"h","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":50,"h":30,"x":0,"y":0,"l":1,"i":0,"I":"%2"},{"t":"v","w":49,"h":30,"x":51,"y":0,"c":[{"t":"p","w":49,"h":15,"x":51,"y":0,"l":0,"i":1,"I":"%3"},{"t":"p","w":49,"h":14,"x":51,"y":16,"a":true,"i":2,"I":"%4"},{"t":"p","w":38,"h":8,"x":6,"y":4,"i":3,"z":0,"I":"%5"}]}]}}`,
			wantW: 100, wantH: 30,
			wantTiled:  []string{"%2", "%3", "%4"},
			wantFloats: []PaneCell{{ID: "%5", W: 38, H: 8, X: 6, Y: 4}},
			wantRaw:    "f9df,100x30,0,0{50x30,0,0,2,49x30,51,0[49x15,51,0,3,49x14,51,16,4]}",
		},
		{
			// tmux never builds this tree itself, but select-layout accepts it
			// and dumps it back unchanged, with the v1 dump collapsing the
			// inner v as wantRaw does.
			name: "float nested one level deep, pruning collapses to single child",
			in: `{"V":2,"L":{"t":"h","w":100,"h":30,"x":0,"y":0,"c":[` +
				`{"t":"p","w":50,"h":30,"x":0,"y":0,"i":0,"I":"%2"},` +
				`{"t":"v","w":49,"h":30,"x":51,"y":0,"c":[` +
				`{"t":"p","w":49,"h":30,"x":51,"y":0,"i":1,"I":"%3"},` +
				`{"t":"p","w":38,"h":8,"x":6,"y":4,"i":2,"z":0,"I":"%5"}]}]}}`,
			wantW: 100, wantH: 30,
			wantTiled:  []string{"%2", "%3"},
			wantFloats: []PaneCell{{ID: "%5", W: 38, H: 8, X: 6, Y: 4}},
			wantRaw:    "6b94,100x30,0,0{50x30,0,0,2,49x30,51,0,3}",
		},
		{
			// One tiled pane + two floats: root itself becomes a split node
			// (v{p, float, float}), and pruning both floats collapses it
			// back to a leaf root.
			name:  "one tiled pane with two floats, root collapses to a leaf",
			in:    `{"V":2,"L":{"t":"v","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":100,"h":30,"x":0,"y":0,"a":true,"i":0,"I":"%9"},{"t":"p","w":18,"h":4,"x":61,"y":21,"i":2,"z":0,"I":"%11"},{"t":"p","w":38,"h":8,"x":6,"y":4,"i":1,"z":1,"I":"%10"}]}}`,
			wantW: 100, wantH: 30,
			wantTiled: []string{"%9"},
			wantFloats: []PaneCell{
				{ID: "%11", W: 18, H: 4, X: 61, Y: 21},
				{ID: "%10", W: 38, H: 8, X: 6, Y: 4},
			},
			wantRaw: "a886,100x30,0,0,9",
		},
		{
			// The nested case above, zoomed: the JSON reports the saved,
			// unzoomed tree.
			name:  "zoomed window still reports the unzoomed tree",
			in:    `{"V":2,"L":{"t":"h","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":50,"h":30,"x":0,"y":0,"a":true,"i":0,"I":"%2"},{"t":"v","w":49,"h":30,"x":51,"y":0,"c":[{"t":"p","w":49,"h":15,"x":51,"y":0,"l":1,"i":1,"I":"%3"},{"t":"p","w":49,"h":14,"x":51,"y":16,"l":0,"i":2,"I":"%4"},{"t":"p","w":38,"h":8,"x":6,"y":4,"i":3,"z":1,"I":"%5"}]}]}}`,
			wantW: 100, wantH: 30,
			wantTiled:  []string{"%2", "%3", "%4"},
			wantFloats: []PaneCell{{ID: "%5", W: 38, H: 8, X: 6, Y: 4}},
			wantRaw:    "f9df,100x30,0,0{50x30,0,0,2,49x30,51,0[49x15,51,0,3,49x14,51,16,4]}",
		},
		{
			// -B none float: no border inset, so the cell is the exact
			// create geometry (40x10 at 5,3).
			name:  "borderless float (-B none)",
			in:    `{"V":2,"L":{"t":"v","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":100,"h":30,"x":0,"y":0,"a":true,"i":0,"I":"%12"},{"t":"p","w":40,"h":10,"x":5,"y":3,"i":1,"z":0,"I":"%13"}]}}`,
			wantW: 100, wantH: 30,
			wantTiled:  []string{"%12"},
			wantFloats: []PaneCell{{ID: "%13", W: 40, H: 10, X: 5, Y: 3}},
			wantRaw:    "5471,100x30,0,0,12",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLayout(tt.in)
			if err != nil {
				t.Fatalf("ParseLayout(%q) error: %v", tt.in, err)
			}
			if got.W != tt.wantW || got.H != tt.wantH {
				t.Errorf("window dims = %dx%d, want %dx%d", got.W, got.H, tt.wantW, tt.wantH)
			}
			var ids []string
			for _, p := range got.Panes {
				ids = append(ids, p.ID)
			}
			if len(ids) != len(tt.wantTiled) {
				t.Fatalf("tiled pane ids = %v, want %v", ids, tt.wantTiled)
			}
			for i := range ids {
				if ids[i] != tt.wantTiled[i] {
					t.Errorf("tiled pane[%d] id = %s, want %s", i, ids[i], tt.wantTiled[i])
				}
			}
			if len(got.Floats) != len(tt.wantFloats) {
				t.Fatalf("floats = %+v, want %+v", got.Floats, tt.wantFloats)
			}
			for i := range got.Floats {
				if got.Floats[i] != tt.wantFloats[i] {
					t.Errorf("float[%d] = %+v, want %+v", i, got.Floats[i], tt.wantFloats[i])
				}
			}
			if got.Raw != tt.wantRaw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.wantRaw)
			}
		})
	}
}

func TestParseLayoutV2Errors(t *testing.T) {
	cases := map[string]string{
		"V wrong":                `{"V":1,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}}`,
		"V missing":              `{"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}}`,
		"unknown cell type":      `{"V":2,"L":{"t":"q","w":10,"h":10,"x":0,"y":0,"I":"%0"}}`,
		"pane with children":     `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0","c":[{"t":"p","w":5,"h":5,"x":0,"y":0,"I":"%1"}]}}`,
		"split with one child":   `{"V":2,"L":{"t":"h","w":10,"h":10,"x":0,"y":0,"c":[{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}]}}`,
		"split with no children": `{"V":2,"L":{"t":"h","w":10,"h":10,"x":0,"y":0,"c":[]}}`,
		"missing w":              `{"V":2,"L":{"t":"p","h":10,"x":0,"y":0,"I":"%0"}}`,
		"missing h":              `{"V":2,"L":{"t":"p","w":10,"x":0,"y":0,"I":"%0"}}`,
		"missing x":              `{"V":2,"L":{"t":"p","w":10,"h":10,"y":0,"I":"%0"}}`,
		"missing y":              `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"I":"%0"}}`,
		"missing I":              `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0}}`,
		"I not %+digits":         `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"bad"}}`,
		"only floats, no tiled pane": `{"V":2,"L":{"t":"h","w":10,"h":10,"x":0,"y":0,"c":[` +
			`{"t":"p","w":5,"h":5,"x":0,"y":0,"I":"%0","z":0},` +
			`{"t":"p","w":5,"h":5,"x":5,"y":0,"I":"%1","z":1}]}}`,
		"trailing garbage": `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}}garbage`,
		"trailing }":       `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}}}`,
		"trailing ]":       `{"V":2,"L":{"t":"p","w":10,"h":10,"x":0,"y":0,"I":"%0"}}]`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLayout(in); err == nil {
				t.Errorf("ParseLayout(%q) expected error, got nil", in)
			}
		})
	}
}

// TestParseLayoutV2DepthBomb pins that a v2 tree nested past maxLayoutDepth
// errors instead of panicking, both just past the cap and near encoding/json's
// own nesting limit. It checks the error, not the cost: parse time stays
// linear only because parseLayoutV2 decodes the tree once rather than per
// level. Built programmatically, since only the depth matters.
func TestParseLayoutV2DepthBomb(t *testing.T) {
	for _, depth := range []int{maxLayoutDepth + 1, 4900} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			if _, err := ParseLayout(depthBombLayout(depth)); err == nil {
				t.Error("ParseLayout of a depth bomb: expected error, got nil")
			}
		})
	}
}

// depthBombLayout builds a v2 layout nested depth levels deep. The innermost
// cell is a lone pane; each wrapping level is a two-child split pairing the
// previous level with a sibling pane, so every split stays valid (>= 2
// children) while nesting grows by one per level.
func depthBombLayout(depth int) string {
	body := `{"t":"p","w":1,"h":1,"x":0,"y":0,"I":"%0"}`
	sibling := `{"t":"p","w":1,"h":1,"x":0,"y":0,"I":"%1"}`
	for i := 0; i < depth; i++ {
		body = `{"t":"h","w":1,"h":1,"x":0,"y":0,"c":[` + body + `,` + sibling + `]}`
	}
	return `{"V":2,"L":` + body + `}`
}
