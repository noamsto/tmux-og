package main

import (
	"fmt"
	"math"
	"strconv"
)

// Catppuccin gradient anchors per theme: blue → sapphire → lavender → mauve.
var gradientAnchors = map[string][]string{
	"dark":  {"#89b4fa", "#74c7ec", "#b4befe", "#cba6f7"},
	"light": {"#1e66f5", "#209fb5", "#7287fd", "#8839ef"},
}

func hexToRGB(hex string) (int, int, int) {
	r, _ := strconv.ParseInt(hex[1:3], 16, 0)
	g, _ := strconv.ParseInt(hex[3:5], 16, 0)
	b, _ := strconv.ParseInt(hex[5:7], 16, 0)
	return int(r), int(g), int(b)
}

func rgbToHex(r, g, b int) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func lerp(a, b float64, t float64) float64 { return a + (b-a)*t }

// buildGradient interpolates the anchors into `steps` evenly-spaced colors,
// looping back to the first anchor so the ripple is seamless.
func buildGradient(anchors []string, steps int) []string {
	if len(anchors) == 0 {
		return nil
	}
	loop := append(append([]string{}, anchors...), anchors[0])
	segs := len(loop) - 1
	out := make([]string, steps)
	for i := range steps {
		p := float64(i) / float64(steps) * float64(segs)
		seg := int(p)
		if seg >= segs {
			seg = segs - 1
		}
		frac := p - float64(seg)
		r1, g1, b1 := hexToRGB(loop[seg])
		r2, g2, b2 := hexToRGB(loop[seg+1])
		out[i] = rgbToHex(
			int(lerp(float64(r1), float64(r2), frac)),
			int(lerp(float64(g1), float64(g2), frac)),
			int(lerp(float64(b1), float64(b2), frac)),
		)
	}
	return out
}

// plasma is a classic demoscene intensity field: three sine waves at different
// frequencies/directions summed and normalized to [0,1]. Driving both color and
// brightness from it gives an organic shimmer instead of a linear color stripe.
func plasma(x, y int, t float64) float64 {
	fx, fy := float64(x), float64(y)
	v := math.Sin(fx*0.17 + t)
	v += math.Sin((fx+fy)*0.09 + t*0.6)
	v += math.Sin(math.Hypot(fx, fy)*0.16 - t*1.3)
	return (v + 3) / 6
}

// cellHash is a deterministic per-cell pseudo-random in [0,1) — used to stagger
// the dissolve-in and pick noise glyphs without math/rand (frames must be
// reproducible for tests).
func cellHash(x, y, salt int) float64 {
	h := uint32(x*73856093) ^ uint32(y*19349663) ^ uint32(salt*83492791) //nolint:gosec // intentional wrapping hash
	h ^= h >> 13
	h *= 2654435761
	h ^= h >> 16
	return float64(h%1024) / 1024
}

// shade scales a gradient color's brightness by v in [0,1], floored so glyphs
// never fully vanish mid-shimmer.
func shade(hex string, v float64) string {
	const floor = 0.45
	k := floor + (1-floor)*v
	r, g, b := hexToRGB(hex)
	return rgbToHex(int(float64(r)*k), int(float64(g)*k), int(float64(b)*k))
}
