package main

import "testing"

func TestBuildGradientLengthAndStart(t *testing.T) {
	g := buildGradient([]string{"#000000", "#ffffff"}, 24)
	if len(g) != 24 {
		t.Fatalf("len = %d, want 24", len(g))
	}
	if g[0] != "#000000" {
		t.Errorf("g[0] = %q, want #000000", g[0])
	}
}

func TestPlasmaBoundedAndAnimated(t *testing.T) {
	for _, c := range []struct {
		x, y int
		t    float64
	}{{0, 0, 0}, {80, 24, 3.7}, {13, 7, 100}} {
		v := plasma(c.x, c.y, c.t)
		if v < 0 || v > 1 {
			t.Errorf("plasma(%d,%d,%v) = %v, out of [0,1]", c.x, c.y, c.t, v)
		}
	}
	if plasma(10, 5, 0) == plasma(10, 5, 1) {
		t.Error("plasma should vary over time")
	}
	if plasma(0, 0, 1) != plasma(0, 0, 1) {
		t.Error("plasma must be deterministic")
	}
}

func TestCellHashRangeAndDeterminism(t *testing.T) {
	seen := map[float64]bool{}
	for x := range 16 {
		for y := range 8 {
			h := cellHash(x, y, 0)
			if h < 0 || h >= 1 {
				t.Fatalf("cellHash(%d,%d,0) = %v, out of [0,1)", x, y, h)
			}
			seen[h] = true
		}
	}
	if len(seen) < 32 {
		t.Errorf("cellHash too clumpy: %d distinct values over 128 cells", len(seen))
	}
	if cellHash(3, 4, 5) != cellHash(3, 4, 5) {
		t.Error("cellHash must be deterministic")
	}
}

func TestShadeDimsButNeverBlack(t *testing.T) {
	if shade("#ffffff", 1) != "#ffffff" {
		t.Errorf("full brightness should be identity, got %q", shade("#ffffff", 1))
	}
	r, g, b := hexToRGB(shade("#ffffff", 0))
	if r == 0 || g == 0 || b == 0 {
		t.Error("shade floor should keep glyphs visible at v=0")
	}
	if r >= 0xff {
		t.Errorf("shade(_, 0) should dim, got r=%d", r)
	}
}

func TestHexRoundTrip(t *testing.T) {
	r, g, b := hexToRGB("#89b4fa")
	if r != 0x89 || g != 0xb4 || b != 0xfa {
		t.Fatalf("hexToRGB = %d,%d,%d", r, g, b)
	}
	if got := rgbToHex(r, g, b); got != "#89b4fa" {
		t.Errorf("rgbToHex = %q, want #89b4fa", got)
	}
}
