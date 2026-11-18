package palette

import (
	"math"
	"testing"
)

var latte = map[string]string{
	"@thm_text": "#4c4f69", "@thm_subtext_1": "#5c5f77", "@thm_subtext_0": "#6c6f85",
	"@thm_overlay_2": "#7c7f93", "@thm_overlay_1": "#8c8fa1", "@thm_overlay_0": "#9ca0b0",
	"@thm_blue": "#1e66f5", "@thm_maroon": "#e64553", "@thm_red": "#d20f39",
	"@thm_teal": "#179299", "@thm_mauve": "#8839ef", "@thm_mantle": "#e6e9ef",
	"@thm_base": "#eff1f5",
}

var darkLiterals = map[string]bool{
	"@thm_text/#cdd6f4": true, "@thm_subtext_0/#a6adc8": true, "@thm_overlay_1/#7f849c": true,
	"@thm_surface_2/#585b70": true, "@thm_overlay_0/#6c7086": true, "@thm_surface_1/#45475a": true,
	"@thm_lavender/#b4befe": true, "@thm_peach/#fab387": true, "@thm_red/#f38ba8": true,
	"@thm_green/#a6e3a1": true, "@thm_yellow/#f9e2af": true, "@thm_mauve/#cba6f7": true,
	"@thm_blue/#89b4fa": true, "@thm_surface_2/#45475a": true, "@thm_base/#1e1e2e": true,
	"@thm_mantle/#181825": true, "@thm_teal/#94e2d5": true,
}

func allRoles() []Role {
	rs := []Role{Rule, SelBg, Base, Mantle}
	for _, n := range RowRoles {
		rs = append(rs, n.Role)
	}
	return rs
}

func TestLightHexMatchesLatte(t *testing.T) {
	for _, r := range allRoles() {
		if got := latte[r.LightOpt]; got != r.LightHex {
			t.Errorf("%s: LightHex %s, Latte %s", r.LightOpt, r.LightHex, got)
		}
	}
}

func TestDarkLiterals(t *testing.T) {
	for _, r := range allRoles() {
		if !darkLiterals[r.DarkOpt+"/"+r.DarkHex] {
			t.Errorf("unlisted dark literal %s %s", r.DarkOpt, r.DarkHex)
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
	o["@thm_subtext_1"] = "#123456"
	if got := Resolve(o, Primary); got != "#123456" {
		t.Errorf("option = %s", got)
	}
	if got := Primary.Env(true); got != "THM_SUBTEXT_1" {
		t.Errorf("env = %s", got)
	}
}
