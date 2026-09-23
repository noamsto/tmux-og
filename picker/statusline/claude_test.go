package main

import (
	"os"
	"strings"
	"testing"
)

func TestPaletteSelectsByTheme(t *testing.T) {
	dark := claudePalette("dark")
	if dark.waiting != "#fab387" {
		t.Fatalf("dark waiting = %q, want #fab387", dark.waiting)
	}
	light := claudePalette("light")
	if light.waiting != "#fe640b" {
		t.Fatalf("light waiting = %q, want #fe640b", light.waiting)
	}
}

func TestFadeHexEndpoints(t *testing.T) {
	if got := fadeHex("#000000", "#ffffff", 0); got != "#000000" {
		t.Fatalf("pct 0 = %q, want #000000", got)
	}
	if got := fadeHex("#000000", "#ffffff", 100); got != "#ffffff" {
		t.Fatalf("pct 100 = %q, want #ffffff", got)
	}
	if got := fadeHex("#000000", "#ffffff", 50); got != "#7f7f7f" {
		t.Fatalf("pct 50 = %q, want #7f7f7f", got)
	}
}

func TestFadedHueUnseenPinsBright(t *testing.T) {
	p := claudePalette("dark")
	if got := p.fadedHue("waiting", 100, true); got != "#fab387" {
		t.Fatalf("unseen waiting = %q, want bright #fab387", got)
	}
	if got := p.fadedHue("waiting", 100, false); got != p.idle {
		t.Fatalf("faded waiting = %q, want idle %q", got, p.idle)
	}
}

func TestPriorityState(t *testing.T) {
	cases := []struct {
		c    counts
		want string
	}{
		{counts{errorN: 1, waiting: 1}, "error"},
		{counts{waiting: 1, processing: 3}, "waiting"},
		{counts{denied: 1, compacting: 1}, "denied"},
		{counts{processing: 2, done: 1}, "processing"},
		{counts{idle: 1}, "idle"},
		{counts{}, ""},
	}
	for _, c := range cases {
		if got := c.c.priorityState(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.c, got, c.want)
		}
	}
}

func TestFadePct(t *testing.T) {
	now := int64(1000)
	if got := fadePct("waiting", now, now-10); got != 0 {
		t.Errorf("fresh waiting = %d, want 0", got)
	}
	if got := fadePct("waiting", now, now-30-45); got != 100 {
		t.Errorf("fully stale waiting = %d, want 100", got)
	}
	if got := fadePct("waiting", now, now-52); got < 40 || got > 55 {
		t.Errorf("mid-fade waiting = %d, want ~48", got)
	}
}

func TestFormatIssueList(t *testing.T) {
	if got := formatIssueList(3, []string{"ENG-1", "ENG-2"}); got != "ENG-1 ENG-2" {
		t.Errorf("got %q", got)
	}
	if got := formatIssueList(3, []string{"A", "B", "C", "D", "E"}); got != "A B C +2" {
		t.Errorf("got %q", got)
	}
	if got := formatIssueList(3, nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestAggregateSessionFromDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/issues", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/panes/1", []byte("state=waiting\ntimestamp=2000\nsession=work\n"), 0o644)
	os.WriteFile(dir+"/panes/2", []byte("state=processing\ntimestamp=2000\nsession=other\n"), 0o644)
	os.WriteFile(dir+"/issues/1", []byte("ENG-9\n"), 0o644)

	agg := aggregateSession(dir, "work", now, nil)
	if agg.counts.total != 1 {
		t.Fatalf("total = %d, want 1", agg.counts.total)
	}
	if agg.counts.priorityState() != "waiting" {
		t.Fatalf("state = %q, want waiting", agg.counts.priorityState())
	}
	if len(agg.issues) != 1 || agg.issues[0] != "ENG-9" {
		t.Fatalf("issues = %v, want [ENG-9]", agg.issues)
	}
}

func TestClaudeSegment(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/issues", 0o755)
	now := int64(5000)
	os.WriteFile(dir+"/panes/7", []byte("state=waiting\ntimestamp=5000\nsession=s\n"), 0o644)
	os.WriteFile(dir+"/issues/7", []byte("ENG-1\n"), 0o644)

	got := claudeSegment(dir, "s", "dark", now, nil)
	want := "#[fg=#fab387]󰔟#[fg=default] #[fg=#6c7086]ENG-1#[fg=default] "
	if got != want {
		t.Fatalf("claudeSegment\n got %q\nwant %q", got, want)
	}

	if got := claudeSegment(dir, "absent", "dark", now, nil); got != "" {
		t.Fatalf("absent session = %q, want empty", got)
	}
}

func TestClaudeSegmentHaltedShowsAge(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	now := int64(5000)

	// idle is a halted state → the segment carries a dim "last active" time.
	os.WriteFile(dir+"/panes/3", []byte("state=idle\ntimestamp=4700\nsession=h\n"), 0o644)
	got := claudeSegment(dir, "h", "dark", now, nil)
	if !strings.Contains(got, "]5m#[fg=default] ") {
		t.Fatalf("idle segment %q missing dim age 5m", got)
	}

	// processing is active → no age, the live icon already conveys it.
	os.WriteFile(dir+"/panes/3", []byte("state=processing\ntimestamp=4700\nsession=h\n"), 0o644)
	if got := claudeSegment(dir, "h", "dark", now, nil); strings.Contains(got, "5m") {
		t.Fatalf("active segment %q must not show an age", got)
	}
}

func TestRelAgo(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0s"}, {47, "47s"}, {60, "1m"}, {300, "5m"},
		{3600, "1h"}, {7200, "2h"}, {86400, "1d"}, {259200, "3d"},
	}
	for _, c := range cases {
		if got := relAgo(c.secs); got != c.want {
			t.Errorf("relAgo(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestAggregateSessionScreenOnlyCountsViaLiveIDs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/screen/9", []byte("state=waiting\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"9": true})
	if agg.counts.total != 1 {
		t.Fatalf("screen-only total = %d, want 1", agg.counts.total)
	}
	if agg.counts.priorityState() != "waiting" {
		t.Fatalf("state = %q, want waiting", agg.counts.priorityState())
	}
}

func TestAggregateSessionHookWinsOverScreen(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/panes/1", []byte("state=waiting\ntimestamp=2000\nsession=work\n"), 0o644)
	os.WriteFile(dir+"/screen/1", []byte("state=idle\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"1": true})
	if agg.counts.total != 1 {
		t.Fatalf("total = %d, want 1", agg.counts.total)
	}
	if agg.counts.priorityState() != "waiting" {
		t.Fatalf("state = %q, want waiting (hook-first)", agg.counts.priorityState())
	}
}

func TestAggregateSessionScreenOtherSessionExcluded(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/screen/9", []byte("state=processing\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"8": true})
	if agg.counts.total != 0 {
		t.Fatalf("foreign screen-only total = %d, want 0", agg.counts.total)
	}
}

func TestAggregateSessionScreenStalenessMatchesPanes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	// waiting fade starts at 30s; age 52 is mid-ramp (same as TestFadePct).
	os.WriteFile(dir+"/screen/9", []byte("state=waiting\ntimestamp=1948\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"9": true})
	if agg.counts.total != 1 {
		t.Fatalf("screen-only total = %d, want 1", agg.counts.total)
	}
	if agg.minFade != fadePct("waiting", now, now-52) {
		t.Fatalf("screen-only minFade = %d, want %d (mid-fade, same fadePct as panes/)", agg.minFade, fadePct("waiting", now, now-52))
	}
}

func TestAggregateSessionStaleProcessingOverriddenByScreen(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(400)
	os.WriteFile(dir+"/panes/1", []byte("state=processing\ntimestamp=0\nsession=work\nunseen=1\n"), 0o644)
	os.WriteFile(dir+"/screen/1", []byte("state=idle\ntimestamp=400\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"1": true})
	if agg.counts.priorityState() != "idle" {
		t.Fatalf("state = %q, want idle (stale processing + live screen)", agg.counts.priorityState())
	}
	if agg.unseen {
		t.Fatal("unseen must clear on screen override")
	}
}

func TestAggregateSessionStaleProcessingRehomesViaLiveIDs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(400)
	os.WriteFile(dir+"/panes/1", []byte("state=processing\ntimestamp=0\nsession=stale-sess\nunseen=1\n"), 0o644)
	os.WriteFile(dir+"/screen/1", []byte("state=idle\ntimestamp=400\n"), 0o644)

	work := aggregateSession(dir, "work", now, map[string]bool{"1": true})
	if work.counts.total != 1 || work.counts.priorityState() != "idle" {
		t.Fatalf("work total=%d state=%q, want 1 idle (override re-homes via liveIDs)", work.counts.total, work.counts.priorityState())
	}
	stale := aggregateSession(dir, "stale-sess", now, map[string]bool{})
	if stale.counts.total != 0 {
		t.Fatalf("hook session total = %d, want 0 (override does not keep session=)", stale.counts.total)
	}
}
