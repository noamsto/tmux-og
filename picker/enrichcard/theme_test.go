package main

import (
	"testing"

	"github.com/noamsto/tmux-og/picker/palette"
)

func TestLightCfgMapsRoles(t *testing.T) {
	got := lightCfg(cfg{fg: "x", green: "x"}, map[string]string{"@catppuccin_flavor": "latte"})
	want := map[string]string{
		"fg": palette.Text.LightHex, "mauve": palette.Mauve.LightHex,
		"red": palette.Warn.LightHex, "green": palette.Success.LightHex,
		"peach": palette.Highlight.LightHex, "blue": palette.Accent.LightHex,
		"overlay0": palette.Muted.LightHex, "subtext0": palette.Primary.LightHex,
	}
	have := map[string]string{
		"fg": got.fg, "mauve": got.mauve, "red": got.red, "green": got.green,
		"peach": got.peach, "blue": got.blue, "overlay0": got.overlay0, "subtext0": got.subtext0,
	}
	for k, w := range want {
		if have[k] != w {
			t.Errorf("%s = %q, want %q", k, have[k], w)
		}
	}
}

func TestLightCfgHonoursThemeOptions(t *testing.T) {
	got := lightCfg(cfg{}, map[string]string{"@catppuccin_flavor": "latte", "@thm_teal": "#112233"})
	if got.green != "#112233" {
		t.Errorf("green = %q, want option override", got.green)
	}
}

func TestLightCfgDarkUnchanged(t *testing.T) {
	in := cfg{fg: "a", mauve: "b", red: "c", green: "d", peach: "e", blue: "f", overlay0: "g", subtext0: "h"}
	for _, opts := range []map[string]string{nil, {"@catppuccin_flavor": "mocha"}} {
		if got := lightCfg(in, opts); got != in {
			t.Errorf("dark cfg changed: %+v", got)
		}
	}
}
