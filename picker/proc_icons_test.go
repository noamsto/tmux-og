package main

import "testing"

// iconMap's glyphs come from $TMUX_OG_CONFIG at runtime and vary by config
// (e.g. the tmux-conf-extraction-assertions matrix overrides claude's icon to
// "C"), so these cases compare against iconMap/fallbackIcon rather than a
// hardcoded glyph.
func TestBuildProcIconsNormalizesWrapped(t *testing.T) {
	claudeIcon, ok := iconMap["claude"]
	if !ok {
		t.Fatal("iconMap has no entry for \"claude\"")
	}
	unknownProc := "definitely-not-a-known-proc-name"
	if _, ok := iconMap[unknownProc]; ok {
		t.Fatalf("iconMap unexpectedly has an entry for %q", unknownProc)
	}

	cases := []struct {
		name string
		proc string
		want string
	}{
		{"plain claude", "claude", claudeIcon},
		{"nix-wrapped claude", ".claude-wrapped", claudeIcon},
		{"unknown proc with empty fallback yields nothing", unknownProc, fallbackIcon},
		{"leading dot without -wrapped suffix is left unchanged", "." + unknownProc, fallbackIcon},
		{"trailing -wrapped without leading dot is left unchanged", unknownProc + "-wrapped", fallbackIcon},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := buildProcIcons([]string{tc.proc}, 1)
			want := ""
			if tc.want != "" {
				want = tc.want + " "
			}
			if got != want {
				t.Errorf("buildProcIcons([%q]) = %q, want %q", tc.proc, got, want)
			}
		})
	}
}
