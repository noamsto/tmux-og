// Package palette maps picker colour roles to Catppuccin options for both
// flavors and checks their contrast, so the light theme stays legible.
package palette

import (
	"math"
	"strconv"
	"strings"
)

// Role names the dark and light option a picker colour reads, with the
// fallback hex used when the option is unset.
type Role struct{ DarkOpt, DarkHex, LightOpt, LightHex string }

// Opt returns the tmux option the role reads for the theme.
func (r Role) Opt(light bool) string {
	if light {
		return r.LightOpt
	}
	return r.DarkOpt
}

// Hex returns the fallback hex of the option Opt(light) names.
func (r Role) Hex(light bool) string {
	if light {
		return r.LightHex
	}
	return r.DarkHex
}

// Env returns the THM_* environment variable carrying the role's colour.
// A literal role (no option for the theme) has no override and returns "".
func (r Role) Env(light bool) string {
	if r.Opt(light) == "" {
		return ""
	}
	return "THM_" + strings.ToUpper(strings.TrimPrefix(r.Opt(light), "@thm_"))
}

// Resolve picks the role's colour from opts; light is
// opts["@catppuccin_flavor"] == "latte", with no other theme source.
func Resolve(opts map[string]string, r Role) string {
	return ResolveTheme(opts, opts["@catppuccin_flavor"] == "latte", r)
}

// ResolveTheme is Resolve with an explicit theme.
func ResolveTheme(opts map[string]string, light bool, r Role) string {
	if v := opts[r.Opt(light)]; v != "" {
		return v
	}
	return r.Hex(light)
}

func rl(dOpt, dHex, lOpt, lHex string) Role { return Role{dOpt, dHex, lOpt, lHex} }

var (
	Text      = rl("@thm_fg", "#cdd6f4", "@thm_fg", "#4c4f69")
	Primary   = rl("@thm_subtext_0", "#a6adc8", "@thm_subtext_0", "#5c5f77")
	Quiet     = rl("@thm_overlay_1", "#7f849c", "@thm_subtext_0", "#5c5f77")
	Dim       = rl("@thm_surface_2", "#585b70", "@thm_subtext_1", "#6c6f85")
	Faint     = rl("@thm_overlay_1", "#7f849c", "@thm_overlay_2", "#7c7f93")
	Muted     = rl("@thm_overlay_0", "#6c7086", "@thm_overlay_2", "#7c7f93")
	Rule      = rl("@thm_surface_1", "#45475a", "@thm_overlay_2", "#7c7f93")
	Accent    = rl("@thm_lavender", "#b4befe", "@thm_blue", "#1e66f5")
	Highlight = rl("@thm_peach", "#fab387", "@thm_maroon", "#e64553")
	Warn      = rl("@thm_red", "#f38ba8", "@thm_red", "#d20f39")
	Success   = rl("@thm_green", "#a6e3a1", "@thm_teal", "#179299")
	LevelMed  = rl("@thm_yellow", "#f9e2af", "@thm_teal", "#179299")
	Teal      = rl("@thm_teal", "#94e2d5", "@thm_teal", "#179299")
	Mauve     = rl("@thm_mauve", "#cba6f7", "@thm_mauve", "#8839ef")
	Blue      = rl("@thm_blue", "#89b4fa", "@thm_blue", "#1e66f5")
	SelBg     = rl("@thm_surface_2", "#45475a", "@thm_mantle", "#e6e9ef")
	Base      = rl("@thm_bg", "#1e1e2e", "@thm_bg", "#eff1f5")
	Mantle    = rl("@thm_mantle", "#181825", "@thm_mantle", "#e6e9ef")
)

// Status-bar roles. Dark halves are the @thm_* options the bar always drew.
// The host tints have no Latte option clearing 3:1, so their light half is a
// literal hex (empty LightOpt).
var (
	StatusDim       = rl("@thm_overlay_1", "#7f849c", "@thm_overlay_2", "#7c7f93")
	StatusPRClosed  = rl("@thm_overlay_0", "#6c7086", "@thm_subtext_1", "#6c6f85")
	StatusPRPending = rl("@thm_peach", "#fab387", "@thm_maroon", "#e64553")
	StatusPRSuccess = rl("@thm_green", "#a6e3a1", "@thm_teal", "#179299")
	StatusPark      = rl("@thm_overlay_0", "#6c7086", "@thm_overlay_2", "#7c7f93")
	HostPeach       = rl("@thm_peach", "#fab387", "", "#c4500a")
	HostTeal        = rl("@thm_teal", "#94e2d5", "", "#179299")
	HostPink        = rl("@thm_pink", "#f5c2e7", "", "#c03fa0")
	HostYellow      = rl("@thm_yellow", "#f9e2af", "", "#8f7000")
	HostSky         = rl("@thm_sky", "#89dceb", "", "#127eae")
)

// NamedRole labels a role for contrast reports.
type NamedRole struct {
	Name string
	Role Role
}

// StatusRoles lists every role the status bar stamps as @og_c_<name>; the
// names are a contract with the scripts and the tmux config.
var StatusRoles = []NamedRole{
	{"dim", StatusDim}, {"pr_closed", StatusPRClosed}, {"pr_pending", StatusPRPending},
	{"pr_success", StatusPRSuccess}, {"park", StatusPark},
	{"host_peach", HostPeach}, {"host_teal", HostTeal}, {"host_pink", HostPink},
	{"host_yellow", HostYellow}, {"host_sky", HostSky},
}

// RowRoles lists every foreground role the session and window row builders
// draw; each is checked on both the plain and the selected row background.
var RowRoles = []NamedRole{
	{"Text", Text}, {"Primary", Primary}, {"Quiet", Quiet}, {"Dim", Dim},
	{"Faint", Faint}, {"Muted", Muted}, {"Accent", Accent},
	{"Highlight", Highlight}, {"Warn", Warn}, {"Success", Success},
	{"LevelMed", LevelMed}, {"Mauve", Mauve}, {"Blue", Blue}, {"Teal", Teal},
	{"host_peach", HostPeach}, {"host_teal", HostTeal}, {"host_pink", HostPink},
	{"host_yellow", HostYellow}, {"host_sky", HostSky},
}

const (
	FloorPrimary   = 4.5
	FloorSecondary = 3.0
)

// Pair is one foreground-on-background combination drawn by a picker surface.
type Pair struct {
	Surface, Name string
	Fg, Bg        Role
	Floor         float64
}

// LightPairs is every measured combination, checked against Latte.
var LightPairs = buildPairs()

func rowFloor(name string) float64 {
	switch name {
	case "Text", "Primary", "Quiet":
		return FloorPrimary
	}
	return FloorSecondary
}

func buildPairs() []Pair {
	var ps []Pair
	for _, rr := range RowRoles {
		for _, bg := range []struct {
			n string
			r Role
		}{{"Base", Base}, {"SelBg", SelBg}} {
			ps = append(ps, Pair{"rows", rr.Name + "/" + bg.n, rr.Role, bg.r, rowFloor(rr.Name)})
		}
	}
	chrome := []Pair{
		{"hints", "Faint/Base", Faint, Base, FloorSecondary},
		{"placeholder", "Muted/Base", Muted, Base, FloorSecondary},
		{"borders", "Rule/Base", Rule, Base, FloorSecondary},
		{"borders", "Rule/Mantle", Rule, Mantle, FloorSecondary},
		{"which-key", "Accent/Base", Accent, Base, FloorSecondary},
		{"which-key", "Primary/Base", Primary, Base, FloorPrimary},
		{"which-key", "Dim/SelBg", Dim, SelBg, FloorSecondary},
		{"links", "Blue/Base", Blue, Base, FloorSecondary},
		{"links", "Primary/SelBg", Primary, SelBg, FloorPrimary},
		{"wall", "Rule/Base", Rule, Base, FloorSecondary},
		{"wall", "Text/Mantle", Text, Mantle, FloorPrimary},
		{"search", "Accent/Base", Accent, Base, FloorSecondary},
		{"search", "Muted/Base", Muted, Base, FloorSecondary},
		{"enrich card", "Success/Mantle", Success, Mantle, FloorSecondary},
		{"enrich card", "Highlight/Mantle", Highlight, Mantle, FloorSecondary},
		{"enrich card", "Accent/Mantle", Accent, Mantle, FloorSecondary},
		{"enrich card", "Muted/Mantle", Muted, Mantle, FloorSecondary},
		{"enrich card", "Primary/Mantle", Primary, Mantle, FloorPrimary},
		{"splash", "Primary/Base", Primary, Base, FloorPrimary},
		{"splash", "Accent/Base", Accent, Base, FloorSecondary},
	}
	status := []Pair{
		{"status bar", "dim/Base", StatusDim, Base, FloorSecondary},
		{"status bar", "pr_closed/Base", StatusPRClosed, Base, FloorSecondary},
		{"status bar", "pr_pending/Base", StatusPRPending, Base, FloorSecondary},
		{"status bar", "pr_success/Base", StatusPRSuccess, Base, FloorSecondary},
		{"park", "park/Mantle", StatusPark, Mantle, FloorSecondary},
	}
	ps = append(ps, chrome...)
	return append(ps, status...)
}

func lum(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || len(hex) != 6 {
		return 0
	}
	ch := func(c uint64) float64 {
		f := float64(c) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(v>>16&0xff) + 0.7152*ch(v>>8&0xff) + 0.0722*ch(v&0xff)
}

// Contrast is the WCAG contrast ratio of two hex colours, with or without '#'.
func Contrast(fgHex, bgHex string) float64 {
	a, b := lum(fgHex), lum(bgHex)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}
