package palette

import (
	"math"
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
}

func namedRoles() []NamedRole {
	rs := []NamedRole{{"Rule", Rule}, {"SelBg", SelBg}, {"Base", Base}, {"Mantle", Mantle}}
	return append(rs, RowRoles...)
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
