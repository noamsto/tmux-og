package manifest

import (
	"strings"
	"testing"
)

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

func TestPromptBoxTypedBorderRunKeepsState(t *testing.T) {
	ms, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := ForCommand(ms, "claude")
	if !ok {
		t.Fatal("no claude manifest")
	}
	full := strings.Repeat("─", 40)
	footer := "  🤖 Opus 5.5 | 📊 280k/1M\n  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← 1 agent"
	panel := "\n\n  ● main\n  ◯ a\n  ◯ b"
	for name, screen := range map[string]string{
		"typed border run":      full + "\n❯ why does the footer row ──── vanish\n" + full + "\n" + footer + panel,
		"indented typed border": full + "\n❯ look\n  ──────\n  a\n  b\n  c\n" + full + "\n" + footer + panel,
	} {
		t.Run(name, func(t *testing.T) {
			state, flags, _ := Match(m, screen, "", false)
			if state != "idle" || flags["bg"] != 1 {
				t.Fatalf("Match = %q bg=%d, want idle bg=1", state, flags["bg"])
			}
		})
	}
}

func TestPromptBoxRegion(t *testing.T) {
	const bar = "────────────"
	footer := "  🤖 Opus\n  -- INSERT -- · 2 shells"
	panel := "\n\n  ● main\n  ◯ a\n  ◯ b"
	three := "❯ one\n  two\n  three\n"
	five := "❯ one\n  two\n  three\n  four\n  five\n"
	full := strings.Repeat("─", 40)
	realFooter := "  🤖 Opus 5.5 | 📊 280k/1M\n  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← 1 agent"
	typedBorder := "❯ why does the footer row ──── vanish\n" + full + "\n" + realFooter
	typedRows := "❯ look\n  ──────\n  a\n  b\n  c\n" + full + "\n" + realFooter
	cases := []struct{ name, screen, want string }{
		{"border run inside typed prompt text", full + "\n" + typedBorder,
			typedBorder},
		{"border run inside typed prompt text with panel", full + "\n" + typedBorder + panel,
			typedBorder},
		{"indented border run among typed rows", full + "\n" + typedRows,
			typedRows},
		{"panel below blank row", "old\n" + bar + "\n❯ \n" + bar + "\n" + footer + panel,
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"no blank row, footer capped at four rows", bar + "\n❯ \n" + bar + "\na\nb\nc\nd\ne\nf",
			"❯ \n" + bar + "\na\nb\nc\nd"},
		{"3-row typed prompt", bar + "\n" + three + bar + "\n" + footer,
			three + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"3-row typed prompt with panel", bar + "\n" + three + bar + "\n" + footer + panel,
			three + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"5-row typed prompt", bar + "\n" + five + bar + "\n" + footer,
			five + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"5-row typed prompt with panel", bar + "\n" + five + bar + "\n" + footer + panel,
			five + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"last prompt line wins within the box", bar + "\n❯ one\n❯ two\n  x\n" + bar + "\n" + footer,
			"❯ two\n  x\n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"panel row with shell count after blank row", bar + "\n❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT --\n\n  ◯ a · 3 shells",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT --"},
		{"whitespace-only row ends the footer", bar + "\n❯ \n" + bar + "\n" + footer + "\n   \n  ◯ a · 3 shells",
			"❯ \n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"last box wins over scrollback", bar + "\n❯ old\n" + bar + "\nold footer · 3 shells\n\nx\n" + bar + "\n❯ new\n" + bar + "\n" + footer,
			"❯ new\n" + bar + "\n  🤖 Opus\n  -- INSERT -- · 2 shells"},
		{"dialog does not anchor on an unbordered echo", "❯ old echo\nanswer\nDo you want to proceed?\n❯ 1. Yes\n  2. No", ""},
		{"dialog bordered below an echo without top border", "❯ old echo\n" + bar + "\nDo you want to proceed?\n❯ 1. Yes", ""},
		{"no box", "❯ hi\nplain\ntext", ""},
		{"borders without prompt", bar + "\nplain\n" + bar + "\n" + footer, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := regionText("prompt_box", c.screen, ""); got != c.want {
				t.Fatalf("region = %q, want %q", got, c.want)
			}
		})
	}
}
