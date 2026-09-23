package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUsageCachesSkipsMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "claude.json"), []byte(`{"windows":[{"label":"5h","pct":42}],"monthly":null}`), 0o644)
	os.WriteFile(filepath.Join(dir, "codex.json"), []byte(`not json`), 0o644)

	caches := loadUsageCaches(dir)
	if len(caches) != 1 {
		t.Fatalf("caches = %v, want only claude", caches)
	}
	if caches["claude"].Windows[0].Pct != 42 {
		t.Fatalf("claude pct = %v, want 42", caches["claude"].Windows[0].Pct)
	}
}

func TestLoadUsageCachesOldSchemaNoSpendField(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "claude.json"), []byte(`{"windows":[{"label":"5h","pct":42}],"monthly":{"label":"mo","pct":60}}`), 0o644)

	caches := loadUsageCaches(dir)
	c, ok := caches["claude"]
	if !ok {
		t.Fatalf("claude cache missing")
	}
	if c.Spend != nil {
		t.Fatalf("Spend = %+v, want nil for old-schema cache", c.Spend)
	}
	if c.Windows[0].Pct != 42 || c.Monthly.Pct != 60 {
		t.Fatalf("windows/monthly not parsed: %+v", c)
	}

	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL",
		thmSubtext0:           "#9a8", thmGreen: "#0f0", thmPeach: "#fa0", thmRed: "#f00",
	}
	got := usageSegment(a, caches, map[string]bool{"claude": true}, 0)
	want := "#[fg=#9a8]CL #[fg=#0f0]42%·5h #[fg=#0f0]60%·mo  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestUsageSegmentDisabled(t *testing.T) {
	caches := map[string]usageCache{"claude": {Windows: []usageWindow{{Label: "5h", Pct: 42}}}}
	open := map[string]bool{"claude": true}
	if got := usageSegment(args{usageMonthlyThreshold: 0}, caches, open, 0); got != "" {
		t.Fatalf("threshold 0 = %q, want empty (feature off)", got)
	}
}

func TestUsageSegmentWindowsColorsAndOrder(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL", iconUsageCodex: "CX", iconUsageCursor: "CU",
		thmSubtext0: "#9a8", thmGreen: "#0f0", thmPeach: "#fa0", thmRed: "#f00",
	}
	caches := map[string]usageCache{
		// Codex first in the map literal on purpose: output order must follow
		// usageAgentOrder (claude, codex), not map iteration.
		"codex":  {Windows: []usageWindow{{Label: "5h", Pct: 85}, {Label: "7d", Pct: 100}}},
		"claude": {Windows: []usageWindow{{Label: "5h", Pct: 42}, {Label: "7d", Pct: 18}}, Monthly: &usageWindow{Label: "mo", Pct: 5}},
	}
	open := map[string]bool{"codex": true, "claude": true}
	got := usageSegment(a, caches, open, 0)
	want := "#[fg=#9a8]CL #[fg=#0f0]42%·5h #[fg=#0f0]18%·7d" +
		"  #[fg=#9a8]CX #[fg=#fa0]85%·5h #[fg=#f00]100%·7d  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestUsageSegmentMonthlyOnlyAboveThreshold(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL",
		thmSubtext0:           "#9a8", thmGreen: "#0f0", thmPeach: "#fa0", thmRed: "#f00",
	}
	open := map[string]bool{"claude": true}
	caches := map[string]usageCache{
		"claude": {
			Windows: []usageWindow{{Label: "7d", Pct: 100}},
			Monthly: &usageWindow{Label: "mo", Pct: 51},
		},
	}
	got := usageSegment(a, caches, open, 0)
	want := "#[fg=#9a8]CL #[fg=#f00]100%·7d #[fg=#0f0]51%·mo  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}

	caches["claude"] = usageCache{
		Windows: []usageWindow{{Label: "7d", Pct: 100}},
		Monthly: &usageWindow{Label: "mo", Pct: 49},
	}
	if got := usageSegment(a, caches, open, 0); strings.Contains(got, "mo") {
		t.Fatalf("49%% monthly should stay hidden at threshold 50, got %q", got)
	}
}

func TestUsageSegmentEmptyAgentDropsOut(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL", iconUsageCursor: "CU",
		thmSubtext0: "#9a8", thmGreen: "#0f0",
	}
	// Cursor on an uncapped tier: provider writes windows:[] monthly:null.
	caches := map[string]usageCache{
		"claude": {Windows: []usageWindow{{Label: "5h", Pct: 10}}},
		"cursor": {Windows: []usageWindow{}, Monthly: nil},
	}
	open := map[string]bool{"claude": true, "cursor": true}
	got := usageSegment(a, caches, open, 0)
	if strings.Contains(got, "CU") {
		t.Fatalf("empty cursor cache should not render, got %q", got)
	}
	if !strings.Contains(got, "CL") {
		t.Fatalf("claude block missing, got %q", got)
	}
}

// TestUsageSegmentClosedAgentDropsOut is the actual new per-agent gate: an
// agent with a fresh, populated cache but no local pane running it must not
// render, even though caches[agent] exists and has data.
func TestUsageSegmentClosedAgentDropsOut(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL",
		thmSubtext0:           "#9a8", thmGreen: "#0f0",
	}
	caches := map[string]usageCache{
		"claude": {Windows: []usageWindow{{Label: "5h", Pct: 10}}},
	}
	got := usageSegment(a, caches, map[string]bool{}, 0)
	if got != "" {
		t.Fatalf("closed agent with fresh cache rendered %q, want empty", got)
	}
}

func TestUsageSegmentAllEmpty(t *testing.T) {
	a := args{usageMonthlyThreshold: 50, thmSubtext0: "#9a8"}
	if got := usageSegment(a, map[string]usageCache{}, nil, 0); got != "" {
		t.Fatalf("no caches = %q, want empty", got)
	}
}

func TestUsageSegmentResetSuffixOnlyNearExhaustion(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL",
		thmSubtext0:           "#9a8", thmGreen: "#0f0", thmPeach: "#fa0", thmRed: "#f00",
	}
	now := int64(10000)
	caches := map[string]usageCache{
		"claude": {Windows: []usageWindow{
			{Label: "5h", Pct: 85, ResetAt: now + 45*60},   // below 90: no suffix
			{Label: "7d", Pct: 100, ResetAt: now + 6*3600}, // exhausted: hours suffix
			{Label: "wk", Pct: 95, ResetAt: now + 3*86400}, // exhausted: days suffix
			{Label: "mo", Pct: 99, ResetAt: now - 60},      // reset in the past: no suffix
		}},
	}
	open := map[string]bool{"claude": true}
	got := usageSegment(a, caches, open, now)
	want := "#[fg=#9a8]CL #[fg=#fa0]85%·5h #[fg=#f00]100%·7d#[fg=#9a8] " + usageResetGlyph + " 6h #[fg=#f00]95%·wk#[fg=#9a8] " + usageResetGlyph + " 3d #[fg=#f00]99%·mo  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestUsageSegmentCursorMonthlyZeroDropsOut(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL", iconUsageCursor: "CU",
		thmSubtext0: "#9a8", thmGreen: "#0f0",
	}
	// Cursor with a hard limit but 0% spend: provider still writes monthly.
	caches := map[string]usageCache{
		"claude": {Windows: []usageWindow{{Label: "5h", Pct: 10}}},
		"cursor": {Windows: []usageWindow{}, Monthly: &usageWindow{Label: "mo", Pct: 0}},
	}
	open := map[string]bool{"claude": true, "cursor": true}
	got := usageSegment(a, caches, open, 0)
	if strings.Contains(got, "CU") {
		t.Fatalf("cursor at 0%% monthly should not render, got %q", got)
	}
}

// TestUsageSegmentSpendUnconditional pins that Spend renders regardless of
// the monthly-pct threshold: below threshold the monthly block stays hidden
// but $ still shows; above threshold both render.
func TestUsageSegmentSpendUnconditional(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageCursor:       "CU",
		thmSubtext0:           "#9a8", thmGreen: "#0f0", thmPeach: "#fa0", thmRed: "#f00",
	}
	caches := map[string]usageCache{
		"cursor": {
			Monthly: &usageWindow{Label: "mo", Pct: 20},
			Spend:   &usageSpend{Label: "mo", USD: 12.34, Period: "cycle"},
		},
	}
	open := map[string]bool{"cursor": true}
	got := usageSegment(a, caches, open, 0)
	want := "#[fg=#9a8]CU #[fg=#9a8]$12  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}

	caches["cursor"].Monthly.Pct = 51
	got = usageSegment(a, caches, open, 0)
	want = "#[fg=#9a8]CU #[fg=#0f0]51%·mo #[fg=#9a8]$12  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestUsageSegmentSpendZeroRenders(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageCursor:       "CU",
		thmSubtext0:           "#9a8",
	}
	caches := map[string]usageCache{
		"cursor": {Spend: &usageSpend{Label: "mo", USD: 0, Period: "cycle"}},
	}
	open := map[string]bool{"cursor": true}
	got := usageSegment(a, caches, open, 0)
	want := "#[fg=#9a8]CU #[fg=#9a8]$0.00  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestUsageSegmentPiOrderAndIcon(t *testing.T) {
	a := args{
		usageMonthlyThreshold: 50,
		iconUsageClaude:       "CL", iconUsagePi: "PI",
		thmSubtext0: "#9a8", thmGreen: "#0f0",
	}
	caches := map[string]usageCache{
		"pi":     {Windows: []usageWindow{{Label: "5h", Pct: 10}}},
		"claude": {Windows: []usageWindow{{Label: "5h", Pct: 20}}},
	}
	open := map[string]bool{"pi": true, "claude": true}
	got := usageSegment(a, caches, open, 0)
	want := "#[fg=#9a8]CL #[fg=#0f0]20%·5h  #[fg=#9a8]PI #[fg=#0f0]10%·5h  "
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestOpenAgentsFailOpen(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir)

	open := openAgents()
	for _, agent := range usageAgentOrder {
		if !open[agent] {
			t.Fatalf("open = %v, want every agent open on failed list-panes", open)
		}
	}
}
