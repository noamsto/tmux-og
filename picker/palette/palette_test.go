package palette

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// latte mirrors the live values the pinned catppuccin plugin sets in
// catppuccin_latte_tmux.conf: its subtext_0/1 are swapped against upstream
// Latte, and it sets @thm_fg/@thm_bg but never @thm_text/@thm_base.
var latte = map[string]string{
	"@thm_fg": "#4c4f69", "@thm_subtext_0": "#5c5f77", "@thm_subtext_1": "#6c6f85",
	"@thm_overlay_2": "#7c7f93", "@thm_overlay_1": "#8c8fa1", "@thm_overlay_0": "#9ca0b0",
	"@thm_blue": "#1e66f5", "@thm_maroon": "#e64553", "@thm_red": "#d20f39",
	"@thm_teal": "#179299", "@thm_mauve": "#8839ef", "@thm_mantle": "#e6e9ef",
	"@thm_bg": "#eff1f5",
}

// oldLatte holds the Latte values the status bar read before the status roles,
// for the before column of TestStatusPairsReport.
var oldLatte = map[string]string{
	"dim": "#8c8fa1", "pr_closed": "#9ca0b0", "pr_pending": "#fe640b",
	"pr_success": "#40a02b", "park": "#9ca0b0", "host_peach": "#fe640b",
	"host_teal": "#179299", "host_pink": "#ea76cb", "host_yellow": "#df8e1d",
	"host_sky": "#04a5e5", "float": "#179299",
}

type darkHalf struct{ opt, hex string }

var darkLiterals = map[string]darkHalf{
	"Text": {"@thm_fg", "#cdd6f4"}, "Primary": {"@thm_subtext_0", "#a6adc8"},
	"Quiet": {"@thm_overlay_1", "#7f849c"}, "Dim": {"@thm_surface_2", "#585b70"},
	"Faint": {"@thm_overlay_1", "#7f849c"}, "Muted": {"@thm_overlay_0", "#6c7086"},
	"Rule": {"@thm_surface_1", "#45475a"}, "Accent": {"@thm_lavender", "#b4befe"},
	"Highlight": {"@thm_peach", "#fab387"}, "Warn": {"@thm_red", "#f38ba8"},
	"Success": {"@thm_green", "#a6e3a1"}, "LevelMed": {"@thm_yellow", "#f9e2af"},
	"Teal": {"@thm_teal", "#94e2d5"}, "Mauve": {"@thm_mauve", "#cba6f7"},
	"Blue": {"@thm_blue", "#89b4fa"}, "SelBg": {"@thm_surface_2", "#45475a"},
	"Base": {"@thm_bg", "#1e1e2e"}, "Mantle": {"@thm_mantle", "#181825"},
	"dim": {"@thm_overlay_1", "#7f849c"}, "pr_closed": {"@thm_overlay_0", "#6c7086"},
	"pr_pending": {"@thm_peach", "#fab387"}, "pr_success": {"@thm_green", "#a6e3a1"},
	"park": {"@thm_overlay_0", "#6c7086"}, "host_peach": {"@thm_peach", "#fab387"},
	"host_teal": {"@thm_teal", "#94e2d5"}, "host_pink": {"@thm_pink", "#f5c2e7"},
	"host_yellow": {"@thm_yellow", "#f9e2af"}, "host_sky": {"@thm_sky", "#89dceb"},
	"float": {"@thm_teal", "#94e2d5"},
}

func namedRoles() []NamedRole {
	var out []NamedRole
	rs := []NamedRole{{"Rule", Rule}, {"SelBg", SelBg}, {"Base", Base}, {"Mantle", Mantle}}
	seen := map[string]bool{}
	for _, n := range append(append(rs, RowRoles...), StatusRoles...) {
		if !seen[n.Name] {
			seen[n.Name] = true
			out = append(out, n)
		}
	}
	return out
}

func allRoles() []Role {
	rs := []Role{Rule, SelBg, Base, Mantle}
	for _, n := range RowRoles {
		rs = append(rs, n.Role)
	}
	for _, n := range StatusRoles {
		rs = append(rs, n.Role)
	}
	return rs
}

func TestLightHexMatchesLatte(t *testing.T) {
	for _, r := range allRoles() {
		if r.LightOpt == "" {
			continue
		}
		if got := latte[r.LightOpt]; got != r.LightHex {
			t.Errorf("%s: LightHex %s, Latte %s", r.LightOpt, r.LightHex, got)
		}
	}
}

func TestDarkLiterals(t *testing.T) {
	rs := namedRoles()
	if len(rs) != len(darkLiterals) {
		t.Errorf("%d roles, %d dark literals", len(rs), len(darkLiterals))
	}
	for _, n := range rs {
		want, ok := darkLiterals[n.Name]
		if !ok {
			t.Errorf("%s: no dark literal", n.Name)
			continue
		}
		if n.Role.DarkOpt != want.opt || n.Role.DarkHex != want.hex {
			t.Errorf("%s: dark %s %s, want %s %s", n.Name, n.Role.DarkOpt, n.Role.DarkHex, want.opt, want.hex)
		}
	}
}

func TestStatusDarkLiterals(t *testing.T) {
	old := map[string]string{
		"dim": "@thm_overlay_1", "pr_closed": "@thm_overlay_0", "pr_pending": "@thm_peach",
		"pr_success": "@thm_green", "park": "@thm_overlay_0", "host_peach": "@thm_peach",
		"host_teal": "@thm_teal", "host_pink": "@thm_pink", "host_yellow": "@thm_yellow",
		"host_sky": "@thm_sky", "float": "@thm_teal",
	}
	if len(StatusRoles) != len(old) {
		t.Errorf("%d status roles, %d pinned", len(StatusRoles), len(old))
	}
	for _, n := range StatusRoles {
		if got := n.Role.Opt(false); got != old[n.Name] {
			t.Errorf("%s: dark option %s, want %s", n.Name, got, old[n.Name])
		}
	}
}

func TestLiteralRoleHasNoEnv(t *testing.T) {
	if got := HostPeach.Env(true); got != "" {
		t.Errorf("literal Env = %q", got)
	}
	if got := HostPeach.Env(false); got != "THM_PEACH" {
		t.Errorf("dark Env = %q", got)
	}
}

// lab converts sRGB hex to CIELAB (D65) for a perceptual separation check.
func lab(hex string) [3]float64 {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	lin := func(c uint64) float64 {
		f := float64(c) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	r, g, b := lin(v>>16&0xff), lin(v>>8&0xff), lin(v&0xff)
	x := (0.4124*r + 0.3576*g + 0.1805*b) / 0.95047
	y := 0.2126*r + 0.7152*g + 0.0722*b
	z := (0.0193*r + 0.1192*g + 0.9505*b) / 1.089
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
}

// minHostSeparation is the smallest CIE76 distance between two light host
// tints; the closest pair measures ~27 (teal vs sky).
const minHostSeparation = 25.0

func TestStatusHostTintsDistinct(t *testing.T) {
	var hosts []NamedRole
	for _, n := range StatusRoles {
		if strings.HasPrefix(n.Name, "host_") {
			hosts = append(hosts, n)
		}
	}
	for i, a := range hosts {
		for _, b := range hosts[i+1:] {
			la, lb := lab(a.Role.LightHex), lab(b.Role.LightHex)
			d := math.Sqrt((la[0]-lb[0])*(la[0]-lb[0]) + (la[1]-lb[1])*(la[1]-lb[1]) + (la[2]-lb[2])*(la[2]-lb[2]))
			if d < minHostSeparation {
				t.Errorf("%s vs %s: dE %.1f < %.1f", a.Name, b.Name, d, minHostSeparation)
			}
		}
	}
}

// TestStatusPairsReport logs the before and after ratio of each status-bar
// pair on Latte; run with -v for the PR table.
func TestStatusPairsReport(t *testing.T) {
	for _, n := range StatusRoles {
		bg := Base
		if n.Name == "park" {
			bg = Mantle
		}
		t.Logf("%-12s before %s %.2f  after %s %.2f on %s", n.Name,
			oldLatte[n.Name], Contrast(oldLatte[n.Name], bg.LightHex),
			n.Role.LightHex, Contrast(n.Role.LightHex, bg.LightHex), bg.LightHex)
		if strings.HasPrefix(n.Name, "host_") {
			t.Logf("%-12s on mantle before %.2f after %.2f", n.Name,
				Contrast(oldLatte[n.Name], Mantle.LightHex), Contrast(n.Role.LightHex, Mantle.LightHex))
		}
	}
}

func TestLightPairsMeetFloor(t *testing.T) {
	for _, p := range LightPairs {
		if c := Contrast(p.Fg.LightHex, p.Bg.LightHex); c < p.Floor {
			t.Errorf("%s %s: %.2f < %.1f", p.Surface, p.Name, c, p.Floor)
		}
	}
}

func TestContrastKnown(t *testing.T) {
	if c := Contrast("#4c4f69", "eff1f5"); math.Abs(c-7.06) > 0.05 {
		t.Errorf("text on base = %.2f", c)
	}
	if c := Contrast("#000000", "#ffffff"); math.Abs(c-21) > 0.01 {
		t.Errorf("black on white = %.2f", c)
	}
}

func TestResolve(t *testing.T) {
	if got := Resolve(nil, Primary); got != "#a6adc8" {
		t.Errorf("empty opts = %s", got)
	}
	o := map[string]string{"@catppuccin_flavor": "latte"}
	if got := Resolve(o, Primary); got != "#5c5f77" {
		t.Errorf("latte fallback = %s", got)
	}
	o["@thm_subtext_0"] = "#123456"
	if got := Resolve(o, Primary); got != "#123456" {
		t.Errorf("option = %s", got)
	}
	if got := Primary.Env(true); got != "THM_SUBTEXT_0" {
		t.Errorf("env = %s", got)
	}
}

func TestEveryStatusRoleIsFloored(t *testing.T) {
	for _, sr := range StatusRoles {
		found := false
		for _, p := range LightPairs {
			if strings.HasPrefix(p.Name, sr.Name+"/") && (p.Surface == "status bar" || p.Surface == "park") {
				found = true
			}
		}
		if !found {
			t.Errorf("status role %s has no LightPairs entry", sr.Name)
		}
	}
}
