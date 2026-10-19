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

func TestBuildProcIconsDedupesWrappedAndPlain(t *testing.T) {
	claudeIcon, ok := iconMap["claude"]
	if !ok {
		t.Fatal("iconMap has no entry for \"claude\"")
	}
	got, dw := buildProcIcons([]string{".claude-wrapped", "claude"}, 5)
	if want := claudeIcon + " "; got != want {
		t.Errorf("buildProcIcons([.claude-wrapped claude]) = %q, want %q", got, want)
	}
	if wantDW := iconCellWidth(claudeIcon) + 1; dw != wantDW {
		t.Errorf("display width = %d, want %d", dw, wantDW)
	}
}

func TestBuildProcIconsKeepsDistinctIcons(t *testing.T) {
	claudeIcon, ok := iconMap["claude"]
	if !ok {
		t.Fatal("iconMap has no entry for \"claude\"")
	}
	var other string
	var otherProc string
	for proc, icon := range iconMap {
		if icon != "" && icon != claudeIcon {
			other, otherProc = icon, proc
			break
		}
	}
	if other == "" {
		t.Skip("iconMap has no second distinct icon")
	}
	got, _ := buildProcIcons([]string{".claude-wrapped", otherProc, "claude"}, 5)
	if want := claudeIcon + " " + other + " "; got != want {
		t.Errorf("buildProcIcons = %q, want %q", got, want)
	}
}
