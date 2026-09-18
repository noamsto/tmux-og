package render

import (
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
)

func keysPaths() *paths.Paths {
	return &paths.Paths{
		Scripts: map[string]string{
			"tmux-pr-enrich":   "/store/tmux-pr-enrich",
			"tmux-issue-stamp": "/store/tmux-issue-stamp",
		},
		Bin: map[string]string{
			"og-remote-bridge-ctl": "/store/ctl",
			"tmux-enrich-card":     "/store/card",
		},
	}
}

// The escaping is scoped to the inner new-pane command; the if-shell wrapper's
// own quotes stay bare, and a quote inside the suffix must come out escaped.
func TestFloatNewPaneGuardEscapesInnerCommandOnly(t *testing.T) {
	f := mkFloat("90%", "90%", "5%", "5%")
	got := floatNewPaneGuard(f, "", `"echo hi" \; set -p @pane_label hi`)
	want := `if-shell "tmux list-commands new-pane | grep -q -- -A" ` +
		`"new-pane -x 90% -y 90% -X 5% -Y 5% -B heavy -A \"echo hi\" \; set -p @pane_label hi \; ` +
		`set -p @float_geom '90% 90% 5% 5%' \; set -p remain-on-exit off" ` +
		`"new-pane -x 90% -y 90% -X 5% -Y 5% -B heavy \"echo hi\" \; set -p @pane_label hi \; ` +
		`set -p @float_geom '90% 90% 5% 5%' \; set -p remain-on-exit off"`
	if got != want {
		t.Fatalf("guard =\n%q\nwant\n%q", got, want)
	}
}

// Every bridged tool bind must gate on the same @pane_label value its own
// create branch stamps, and hand it to the shell through that tool's register:
// if either drifts, the press silently stacks another float instead of focusing
// the one that is open (#679). The label appears in three places, so this is the
// cross-check rather than an eyeball.
func TestBridgedFloatToolsReuseTheirOwnLabel(t *testing.T) {
	prdash := "/store/prdash"
	p := keysPaths()
	p.Prdash = &prdash
	for _, tc := range []struct {
		name string
		bind string
		tool string
	}{
		{"prdash", prdashBind(p), "prdash"},
		{"lazygit", lazygitBind(p), "lazygit"},
		{"yazi", yaziBind(p), "yazi"},
	} {
		if !strings.Contains(tc.bind, "set -p @pane_label "+tc.tool) {
			t.Fatalf("%s: bind does not stamp @pane_label %s: %q", tc.name, tc.tool, tc.bind)
		}
		if !strings.Contains(tc.bind, floatLookup(tc.tool)) {
			t.Fatalf("%s: bind does not gate on %s: %q", tc.name, floatLookup(tc.tool), tc.bind)
		}
		want := `run-shell "tmux select-pane -t #{q:` + floatRegister(tc.tool) + `}"`
		if !strings.Contains(tc.bind, want) {
			t.Fatalf("%s: bind has no focus branch %q: %q", tc.name, want, tc.bind)
		}
	}
}

// An absent optional tool must leave the template line's newline alone, so the
// output keeps exactly one empty line where the bind would have been.
func TestOptionalBindsCarryNoTrailingNewline(t *testing.T) {
	p := keysPaths()
	if got := carouselBind(p); got != "" {
		t.Fatalf("carouselBind with no toggle = %q, want empty", got)
	}
	if got := prdashBind(p); got != "" {
		t.Fatalf("prdashBind with no prdash = %q, want empty", got)
	}
	toggle, prdash := "/store/tmux-claude-images", "/store/prdash"
	p.CarouselToggle, p.Prdash = &toggle, &prdash
	for name, got := range map[string]string{
		"carouselBind": carouselBind(p),
		"prdashBind":   prdashBind(p),
	} {
		if got == "" || strings.HasSuffix(got, "\n") {
			t.Fatalf("%s = %q, want one line with no trailing newline", name, got)
		}
	}
}

func TestEnrichCardBindUsesRawIcons(t *testing.T) {
	cfg := &config.Config{Enrich: config.Enrich{Icons: map[string]string{"linear": "L#"}}}
	got := enrichCardBind(cfg, keysPaths())
	if !strings.Contains(got, `--icon-linear 'L#' --icon-github '`+enrichIconDefaults["github"]+`'`) {
		t.Fatalf("override or default icon missing: %q", got)
	}
	if strings.Contains(got, "L##") {
		t.Fatalf("card must carry the raw dialect: %q", got)
	}
}
