package manifest

import "testing"

func mkManifest(rules ...Rule) Manifest { return Manifest{ID: "t", Rules: rules} }

func TestMatchTitleRegionWins(t *testing.T) {
	m := mkManifest(
		Rule{State: "processing", Priority: 100, Region: "title", Contains: []string{"⠐"}},
		Rule{State: "idle", Priority: 10, Region: "whole", Contains: []string{"❯"}},
	)
	got, _, ok := Match(m, "❯ ", "⠐ doing things", false)
	if !ok || got != "processing" {
		t.Fatalf("Match = (%q,%v), want (processing,true)", got, ok)
	}
}

func TestMatchNotExcludes(t *testing.T) {
	m := mkManifest(Rule{
		State: "idle", Priority: 10, Region: "whole",
		Contains: []string{"❯"},
		Not:      []Predicate{{Contains: []string{"do you want to proceed?"}}},
	})
	if _, _, ok := Match(m, "❯ do you want to proceed?", "", false); ok {
		t.Fatal("Match should be excluded by not-predicate")
	}
}

func TestMatchAltScreenSuppressed(t *testing.T) {
	m := mkManifest(Rule{State: "processing", Priority: 100, Region: "whole", Contains: []string{"x"}})
	if _, _, ok := Match(m, "xxx", "", true); ok {
		t.Fatal("alt-screen should suppress all matches")
	}
}

func TestMatchSkipHoldsPrior(t *testing.T) {
	m := mkManifest(Rule{State: "skip", Priority: 100, Region: "whole", Contains: []string{"select model"}})
	got, _, ok := Match(m, "select model", "", false)
	if ok || got != "" {
		t.Fatalf("skip rule should yield (\"\",false), got (%q,%v)", got, ok)
	}
}

func TestMatchLastLines(t *testing.T) {
	m := mkManifest(Rule{State: "waiting", Priority: 100, Region: "last_lines:2", Contains: []string{"proceed?"}})
	screen := "line a\nline b\nline c\ndo you want to proceed?"
	if got, _, ok := Match(m, screen, "", false); !ok || got != "waiting" {
		t.Fatalf("last_lines match = (%q,%v)", got, ok)
	}
}

func TestMatchEmptyRuleDoesNotMatch(t *testing.T) {
	m := mkManifest(Rule{State: "idle", Priority: 100, Region: "whole"})
	if got, _, ok := Match(m, "anything at all", "", false); ok {
		t.Fatalf("empty rule should not match, got (%q,%v)", got, ok)
	}
}

func TestPromptBlockRegion(t *testing.T) {
	const bar = "────────────"
	footer := "  🤖 Opus\n  -- INSERT -- · 1 shell"
	cases := []struct{ name, screen, want string }{
		{"panel below footer", "old\n" + bar + "\n❯ \n" + bar + "\n" + footer + "\n\n  ● main\n  ◯ a\n  ◯ b\n  ◯ c",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell\n  ● main"},
		{"trailing blank rows", "❯ \n" + bar + "\n" + footer + "\n  \n\n  \n",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell"},
		{"panel row starting with prompt glyph", "❯ \n" + bar + "\n" + footer + "\n❯ row\n  ◯ a",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell\n❯ row"},
		{"menu cursor without border falls back to the older prompt", "❯ \n" + bar + "\n" + footer + "\n\nold\n❯ 1. Yes\n  2. No",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell\nold"},
		{"no border", "❯ hi\nplain\ntext", ""},
		{"last prompt wins over scrollback", "❯ old\n" + bar + "\nold footer · 3 shells\nx\ny\nz\n" + bar + "\n❯ new\n" + bar + "\n" + footer,
			"❯ new\n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell"},
		{"multi-line prompt", "❯ one\n  two\n" + bar + "\n" + footer,
			"❯ one\n  two\n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 1 shell"},
		{"leftover rows below footer without shell", "❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT --\nran 2 shells · 3 shells\nx\ny",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT --\nran 2 shells · 3 shells"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := regionText("prompt_block:5", c.screen, ""); got != c.want {
				t.Fatalf("region = %q, want %q", got, c.want)
			}
		})
	}
}
