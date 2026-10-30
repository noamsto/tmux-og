package controlmode

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// v1 builds a v1 layout string from a body, with tmux's checksum.
func v1(body string) string {
	return fmt.Sprintf("%04x,%s", layoutChecksum(body), body)
}

// Real dumps of three even columns (%0 %1 %2) at 200x50 and of the window
// after tmux killed %1, captured from a scratch tmux server.
const (
	threeColsV2 = `{"V":2,"L":{"t":"h","w":200,"h":50,"x":0,"y":0,"c":[{"t":"p","w":66,"h":50,"x":0,"y":0,"l":1,"i":0,"I":"%0"},{"t":"p","w":66,"h":50,"x":67,"y":0,"l":0,"i":1,"I":"%1"},{"t":"p","w":66,"h":50,"x":134,"y":0,"a":true,"i":2,"I":"%2"}]}}`
	killedV2    = `{"V":2,"L":{"t":"h","w":200,"h":50,"x":0,"y":0,"c":[{"t":"p","w":66,"h":50,"x":0,"y":0,"l":0,"i":0,"I":"%0"},{"t":"p","w":133,"h":50,"x":67,"y":0,"a":true,"i":1,"I":"%2"}]}}`
	threeColsV1 = "200x50,0,0{66x50,0,0,0,66x50,67,0,1,66x50,134,0,2}"
	killedV1    = "200x50,0,0{66x50,0,0,0,133x50,67,0,2}"
	evenBody    = "200x50,0,0{99x50,0,0,0,100x50,100,0,2}"
)

func TestRebalanceClosedCell(t *testing.T) {
	// floatV2 is the three-column window (with %1 closed when closed is
	// true) plus a float leaf %9, which tmux's v2 dump lists among the cells.
	floatV2 := func(closed bool) string {
		mid, w := "", 133
		if !closed {
			mid, w = `{"t":"p","w":66,"h":50,"x":67,"y":0,"i":1,"I":"%1"},`, 66
		}
		return `{"V":2,"L":{"t":"h","w":200,"h":50,"x":0,"y":0,"c":[` +
			`{"t":"p","w":66,"h":50,"x":0,"y":0,"i":0,"I":"%0"},` + mid +
			fmt.Sprintf(`{"t":"p","w":%d,"h":50,"x":67,"y":0,"i":2,"I":"%%2"},`, w) +
			`{"t":"p","w":40,"h":10,"x":5,"y":5,"i":3,"z":0,"I":"%9"}]}}`
	}

	tests := []struct {
		name      string
		prev, cur string
		wantBody  string // empty: expect no change
		wantIDs   []string
	}{
		{
			name: "v1 three columns, middle closed",
			prev: v1(threeColsV1), cur: v1(killedV1),
			wantBody: evenBody, wantIDs: []string{"%0", "%2"},
		},
		{
			name: "v2 prev and cur",
			prev: threeColsV2, cur: killedV2,
			wantBody: evenBody, wantIDs: []string{"%0", "%2"},
		},
		{
			name: "v2 prev, v1 cur",
			prev: threeColsV2, cur: v1(killedV1),
			wantBody: evenBody, wantIDs: []string{"%0", "%2"},
		},
		{
			name: "v1 prev, v2 cur",
			prev: v1(threeColsV1), cur: killedV2,
			wantBody: evenBody, wantIDs: []string{"%0", "%2"},
		},
		{
			// D (%3) closed in the hand-sized right column; the left column
			// is untouched, only C/E even out (sum 49 -> 24 + 25).
			name: "nested column, other column keeps hand sizes",
			prev: v1("200x50,0,0{99x50,0,0[99x10,0,0,0,99x39,0,11,1]," +
				"100x50,100,0[100x15,100,0,2,100x15,100,16,3,100x18,100,32,4]}"),
			cur: v1("200x50,0,0{99x50,0,0[99x10,0,0,0,99x39,0,11,1]," +
				"100x50,100,0[100x20,100,0,2,100x29,100,21,4]}"),
			wantBody: "200x50,0,0{99x50,0,0[99x10,0,0,0,99x39,0,11,1]," +
				"100x50,100,0[100x24,100,0,2,100x25,100,25,4]}",
			wantIDs: []string{"%0", "%1", "%2", "%4"},
		},
		{
			name: "k == 2 is a collapse",
			prev: v1("200x50,0,0{99x50,0,0,0,100x50,100,0[100x25,100,0,1,100x24,100,26,2]}"),
			cur:  v1("200x50,0,0{99x50,0,0,0,100x50,100,0,1}"),
		},
		{
			// Perpendicular children keep their own internal heights.
			name: "container children, middle leaf closed",
			prev: v1("200x50,0,0{66x50,0,0[66x20,0,0,0,66x29,0,21,1],66x50,67,0,2," +
				"66x50,134,0[66x10,134,0,3,66x39,134,11,4]}"),
			cur: v1("200x50,0,0{150x50,0,0[150x20,0,0,0,150x29,0,21,1]," +
				"49x50,151,0[49x10,151,0,3,49x39,151,11,4]}"),
			wantBody: "200x50,0,0{99x50,0,0[99x20,0,0,0,99x29,0,21,1]," +
				"100x50,100,0[100x10,100,0,3,100x39,100,11,4]}",
			wantIDs: []string{"%0", "%1", "%3", "%4"},
		},
		{
			// {B|C} (100 wide, 30+69) shrinks to 66: 65 split 30:69 is
			// 19.7 + 45.3, the leftover cell goes to the larger remainder.
			name: "same-axis child rescales proportionally",
			prev: v1("200x50,0,0{20x50,0,0,0,20x50,21,0{9x50,21,0,1,10x50,31,0,2},20x50,42,0,5,20x50,63,0,3}"),
			cur: v1("200x50,0,0{20x50,0,0,0,100x50,21,0{30x50,21,0,1,69x50,52,0,2}," +
				"78x50,122,0,3}"),
			wantBody: "200x50,0,0{66x50,0,0,0,66x50,67,0{20x50,67,0,1,45x50,88,0,2}," +
				"66x50,134,0,3}",
			wantIDs: []string{"%0", "%1", "%2", "%3"},
		},
		{
			name:     "float leaf ignored",
			prev:     floatV2(false),
			cur:      floatV2(true),
			wantBody: evenBody,
			wantIDs:  []string{"%0", "%2"},
		},
		{
			name: "two panes gone",
			prev: v1("200x50,0,0{49x50,0,0,0,49x50,50,0,1,49x50,100,0,2,49x50,150,0,3}"),
			cur:  v1("200x50,0,0{49x50,0,0,0,150x50,50,0,3}"),
		},
		{
			name: "one pane arrived",
			prev: v1(threeColsV1),
			cur:  v1("200x50,0,0{49x50,0,0,0,49x50,50,0,1,49x50,100,0,2,49x50,150,0,3}"),
		},
		{
			name: "one gone, one arrived",
			prev: v1(threeColsV1),
			cur:  v1("200x50,0,0{66x50,0,0,0,66x50,67,0,2,66x50,134,0,3}"),
		},
		{
			name: "parent not found in cur",
			prev: v1(threeColsV1),
			cur:  v1("200x50,0,0[200x24,0,0,0,200x25,0,25,2]"),
		},
		{
			name: "already even",
			prev: v1(threeColsV1),
			cur:  v1(evenBody),
		},
		{
			// 5 columns wide {a|b|c} + x shrinks to 3: no room for 3 leaves.
			name: "below one cell",
			prev: v1("20x10,0,0{4x10,0,0,4,5x10,5,0{1x10,5,0,0,1x10,7,0,1,1x10,9,0,2},9x10,11,0,3}"),
			cur:  v1("7x10,0,0{5x10,0,0{1x10,0,0,0,1x10,2,0,1,1x10,4,0,2},1x10,6,0,3}"),
		},
		{
			// The cached tree has the same geometry as the live one but a
			// different shape; D's cached parent {B|D} is a collapse.
			name: "stale cached tree at equal geometry",
			prev: v1("200x50,0,0[200x24,0,0{99x24,0,0,0,100x24,100,0,2}," +
				"200x25,0,25{99x25,0,25,1,100x25,100,25,3}]"),
			cur: v1("200x50,0,0{99x50,0,0[99x24,0,0,0,99x25,0,25,1],100x50,100,0,2}"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := RebalanceClosedCell(tt.prev, tt.cur)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.wantBody == "" {
				if changed || got.Raw != "" || got.Panes != nil {
					t.Fatalf("changed = %v, got %+v; want no change", changed, got)
				}
				return
			}
			if !changed {
				t.Fatal("changed = false, want true")
			}
			if want := v1(tt.wantBody); got.Raw != want {
				t.Errorf("Raw = %q, want %q", got.Raw, want)
			}
			if ids := paneIDs(got.Panes); !slices.Equal(ids, tt.wantIDs) {
				t.Errorf("Panes = %v, want %v", ids, tt.wantIDs)
			}
			if got.Floats != nil {
				t.Errorf("Floats = %v, want nil", got.Floats)
			}
			reparsed, err := ParseLayout(got.Raw)
			if err != nil {
				t.Fatalf("Raw does not reparse: %v", err)
			}
			if reparsed.W != got.W || reparsed.H != got.H || len(reparsed.Panes) != len(got.Panes) {
				t.Errorf("reparsed %dx%d (%d panes), got %dx%d (%d panes)",
					reparsed.W, reparsed.H, len(reparsed.Panes), got.W, got.H, len(got.Panes))
			}
			for i, p := range got.Panes {
				if reparsed.Panes[i] != p {
					t.Errorf("pane %d = %+v, reparsed %+v", i, p, reparsed.Panes[i])
				}
			}
		})
	}
}

func TestRebalanceClosedCellKeepsOtherCellsByteIdentical(t *testing.T) {
	left := "99x50,0,0[99x10,0,0,0,99x39,0,11,1]"
	got, changed, err := RebalanceClosedCell(
		v1("200x50,0,0{"+left+",100x50,100,0[100x15,100,0,2,100x15,100,16,3,100x18,100,32,4]}"),
		v1("200x50,0,0{"+left+",100x50,100,0[100x20,100,0,2,100x29,100,21,4]}"),
	)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	if !strings.Contains(got.Raw, left) {
		t.Errorf("Raw %q lost the untouched cell %q", got.Raw, left)
	}
}

func TestRebalanceClosedCellErrors(t *testing.T) {
	good := v1(threeColsV1)
	for name, in := range map[string][2]string{
		"bad prev": {"nonsense", good},
		"bad cur":  {good, "nonsense"},
		"bad v2":   {good, `{"V":2,"L":`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, changed, err := RebalanceClosedCell(in[0], in[1]); err == nil || changed {
				t.Errorf("changed = %v, err = %v; want an error", changed, err)
			}
		})
	}
}

func paneIDs(cells []PaneCell) []string {
	ids := make([]string, len(cells))
	for i, c := range cells {
		ids[i] = c.ID
	}
	return ids
}
