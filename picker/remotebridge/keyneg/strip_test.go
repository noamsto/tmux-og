package keyneg

import (
	"strings"
	"testing"
)

func TestFeedStripsModifyOtherKeysSet(t *testing.T) {
	got := NewFilter().Feed([]byte("before\x1b[>4;2mafter"))
	if want := "beforeafter"; string(got) != want {
		t.Fatalf("Feed() = %q, want %q", got, want)
	}
}

func TestFeedStripsModifyOtherKeysReset(t *testing.T) {
	got := NewFilter().Feed([]byte("before\x1b[>4nafter"))
	if want := "beforeafter"; string(got) != want {
		t.Fatalf("Feed() = %q, want %q", got, want)
	}
}

func TestFeedPreservesUnrelatedPrivateCSI(t *testing.T) {
	// "CSI > 1;95;0 c" is a DA2 reply, not modifyOtherKeys — must survive.
	in := "\x1b[>1;95;0c"
	got := NewFilter().Feed([]byte(in))
	if string(got) != in {
		t.Fatalf("Feed() = %q, want unchanged %q", got, in)
	}
}

func TestFeedPreservesOrdinarySGR(t *testing.T) {
	in := "\x1b[31mHello\x1b[0m"
	got := NewFilter().Feed([]byte(in))
	if string(got) != in {
		t.Fatalf("Feed() = %q, want unchanged %q", got, in)
	}
}

func TestFeedSequenceSplitAcrossFeeds(t *testing.T) {
	f := NewFilter()
	seq := "\x1b[>4;2m"
	cut := len(seq) / 2
	got1 := f.Feed([]byte("before" + seq[:cut]))
	if want := "before"; string(got1) != want {
		t.Fatalf("first Feed() = %q, want %q", got1, want)
	}
	got2 := f.Feed([]byte(seq[cut:] + "after"))
	if want := "after"; string(got2) != want {
		t.Fatalf("second Feed() = %q, want %q", got2, want)
	}
}

func TestFeedPrefixSplitAcrossFeeds(t *testing.T) {
	f := NewFilter()
	got1 := f.Feed([]byte("before\x1b["))
	if want := "before"; string(got1) != want {
		t.Fatalf("first Feed() = %q, want %q", got1, want)
	}
	got2 := f.Feed([]byte(">4;2mafter"))
	if want := "after"; string(got2) != want {
		t.Fatalf("second Feed() = %q, want %q", got2, want)
	}
}

func TestFlushReleasesHeldPartial(t *testing.T) {
	f := NewFilter()
	f.Feed([]byte("\x1b[>4;"))
	got := f.Flush()
	if want := "\x1b[>4;"; string(got) != want {
		t.Fatalf("Flush() = %q, want %q", got, want)
	}
	if f.Flush() != nil {
		t.Fatal("second Flush() should return nil once drained")
	}
}

func TestFeedOverlongPendingReleasedAsLiteral(t *testing.T) {
	// Digits keep scanParams genuinely unterminated (a NUL byte would end
	// the params scan immediately, never reaching the maxPending check).
	in := "\x1b[>" + strings.Repeat("9", maxPending+1)
	got := NewFilter().Feed([]byte(in))
	if string(got) != in {
		t.Fatalf("Feed() = %q, want unchanged %q", got, in)
	}
}

// TestFeedStripTable is the contract: one strip case and its paired forward
// case per family. The forward halves are the load-bearing ones — a query
// paints nothing, so over-stripping one costs nothing, while a stripped reply
// is bytes a program was painting.
func TestFeedStripTable(t *testing.T) {
	const keep = "\x00" // sentinel for want == in; a strip case wants ""
	cases := []struct{ name, in, want string }{
		{"DA1 query", "\x1b[c", ""},
		{"DA1 zero", "\x1b[0c", ""},
		{"DA1 reply", "\x1b[?1;2;4c", keep},
		{"DA1 non-zero param", "\x1b[1c", keep},

		{"DA2 query", "\x1b[>c", ""},
		{"DA2 zero", "\x1b[>0c", ""},
		{"DA2 reply", "\x1b[>1;95;0c", keep},

		{"DA3 query", "\x1b[=c", ""},
		{"DA3 zero", "\x1b[=0c", ""},

		{"XTVERSION query", "\x1b[>q", ""},
		{"XTVERSION zero", "\x1b[>0q", ""},
		{"DECLL", "\x1b[0q", keep},
		{"DECSCUSR", "\x1b[2 q", keep},

		{"DSR 5", "\x1b[5n", ""},
		{"DSR 6", "\x1b[6n", ""},
		{"DSR reply 0", "\x1b[0n", keep},
		{"DSR reply 3", "\x1b[3n", keep},

		{"DSR-DEC 6", "\x1b[?6n", ""},
		{"DSR-DEC 996", "\x1b[?996n", ""},
		{"DSR-DEC reply", "\x1b[?997n", keep},

		{"DECRQM private", "\x1b[?2004$p", ""},
		{"DECRQM ansi", "\x1b[4$p", ""},
		{"DECRQM reply", "\x1b[?2004;2$y", keep},

		{"DECRQSS", "\x1bP$qm\x1b\\", ""},
		{"DECRQSS reply", "\x1bP1$r0m\x1b\\", keep},

		{"XTWINOPS 11", "\x1b[11t", ""},
		{"XTWINOPS 16", "\x1b[16t", ""},
		{"XTWINOPS 21", "\x1b[21t", ""},
		{"XTWINOPS 10 action", "\x1b[10t", keep},
		{"XTWINOPS 22 action", "\x1b[22;0t", keep},
		{"XTWINOPS reply", "\x1b[6;32;16t", keep},

		{"OSC 10 query", "\x1b]10;?\x1b\\", ""},
		{"OSC 11 query", "\x1b]11;?\x07", ""},
		{"OSC 12 query", "\x1b]12;?\x07", ""},
		{"OSC 4 query", "\x1b]4;1;?\x07", ""},
		{"OSC 4 set", "\x1b]4;1;rgb:00/00/00\x07", keep},
		{"OSC 11 reply", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07", keep},
		{"OSC 0 title", "\x1b]0;fish\x07", keep},

		{"kitty query", "\x1b[?u", ""},
		{"kitty reply", "\x1b[?1u", keep},
		{"kitty push", "\x1b[>1u", keep},
		{"kitty pop", "\x1b[<1u", keep},
		{"kitty set", "\x1b[=1;1u", keep},

		{"modifyOtherKeys set", "\x1b[>4;2m", ""},
		{"modifyOtherKeys reset", "\x1b[>4n", ""},

		{"SGR", "\x1b[31m", keep},
		{"aborted CSI", "\x1b[\n", keep},
		{"charset escape", "\x1b(B", keep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == keep {
				want = tc.in
			}
			if got := NewFilter().Feed([]byte(tc.in)); string(got) != want {
				t.Fatalf("Feed(%q) = %q, want %q", tc.in, got, want)
			}
			// Again in a stream, so a badly sized consume shows up as a
			// neighbour that lost bytes rather than as a silent pass.
			if got := NewFilter().Feed([]byte("A" + tc.in + "B")); string(got) != "A"+want+"B" {
				t.Fatalf("Feed(%q) = %q, want %q", "A"+tc.in+"B", got, "A"+want+"B")
			}
		})
	}
}

// TestFeedForwardsPassthroughWrappedSequences: a query the remote tmux did not
// answer because it was wrapped for passthrough gets its one reply locally, so
// the wrapper and everything in it must survive byte-for-byte. tmux doubles
// every ESC in the payload, so a scan for the first ST cuts inside it.
func TestFeedForwardsPassthroughWrappedSequences(t *testing.T) {
	for _, in := range []string{
		"\x1bPtmux;\x1b\x1b]52;c;aGk=\x1b\x1b\\\x1b\\",
		"\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\tail",
		// The load-bearing one: a naive first-ST scan leaves the region at the
		// inner terminator and strips the 6n that follows it.
		"\x1bPtmux;\x1b\x1b]11;?\x1b\x1b\\\x1b\x1b[6n\x1b\\",
	} {
		if got := NewFilter().Feed([]byte(in)); string(got) != in {
			t.Fatalf("Feed(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestFeedRegionSurvivesFeedBoundary(t *testing.T) {
	f := NewFilter()
	got := string(f.Feed([]byte("\x1b_Ga=t;abc\x1b")))
	got += string(f.Feed([]byte("\\\x1b[6n")))
	if want := "\x1b_Ga=t;abc\x1b\\"; got != want {
		t.Fatalf("Feed() = %q, want %q", got, want)
	}
}

// The doubled ESC of a passthrough payload is the one region byte whose meaning
// needs the byte after it, so a Feed boundary between the two halves is the
// branch most likely to break under a later refactor: the pair must stay literal
// payload and must not be read as the wrapper's terminator.
func TestFeedDoubledESCSurvivesFeedBoundary(t *testing.T) {
	f := NewFilter()
	got := string(f.Feed([]byte("\x1bPtmux;\x1b")))
	got += string(f.Feed([]byte("\x1babc\x1b\\")))
	if want := "\x1bPtmux;\x1b\x1babc\x1b\\"; got != want {
		t.Fatalf("Feed() = %q, want %q", got, want)
	}
}

func TestFeedOverlongRegionReArms(t *testing.T) {
	body := strings.Repeat("x", maxRegion+1)
	in := "\x1b_G" + body + "\x1b[6n"
	got := NewFilter().Feed([]byte(in))
	if want := "\x1b_G" + body; string(got) != want {
		t.Fatalf("Feed() = %q, want %q (region left, 6n still stripped)", got, want)
	}
}

func TestFeedOverlongOSCReleasedOnceThenReArms(t *testing.T) {
	body := strings.Repeat("x", maxRegion+1)
	in := "\x1b]0;" + body + "\x07\x1b[6n"
	got := NewFilter().Feed([]byte(in))
	if want := "\x1b]0;" + body + "\x07"; string(got) != want {
		t.Fatalf("Feed() = %q, want %q (released exactly once)", got, want)
	}
}

// TestFeedLongOSCThenQueryInOneFeed is the shape the sink pump actually
// delivers: drainOutput coalesces many %output frames into one Feed, so a
// prompt's window title and fzf's startup queries arrive together.
func TestFeedLongOSCThenQueryInOneFeed(t *testing.T) {
	title := "\x1b]0;fish /home/noams/Data/git/tmux-og\x07"
	got := NewFilter().Feed([]byte(title + "\x1b[6n"))
	if string(got) != title {
		t.Fatalf("Feed() = %q, want %q", got, title)
	}
}

func TestFeedQuerySplitAcrossFeeds(t *testing.T) {
	f := NewFilter()
	if got := f.Feed([]byte("A\x1b[6")); string(got) != "A" {
		t.Fatalf("first Feed() = %q, want %q", got, "A")
	}
	if got := f.Feed([]byte("nB")); string(got) != "B" {
		t.Fatalf("second Feed() = %q, want %q", got, "B")
	}
}

func TestFeedHoldsLoneTrailingESC(t *testing.T) {
	f := NewFilter()
	if got := f.Feed([]byte("A\x1b")); string(got) != "A" {
		t.Fatalf("Feed() = %q, want %q", got, "A")
	}
	if got := f.Flush(); string(got) != "\x1b" {
		t.Fatalf("Flush() = %q, want %q", got, "\x1b")
	}
}

// TestFeedRegionEndsWhereTmuxEndsIt is the #544 hole: tmux's OSC and APC string
// states begin with INPUT_STATE_ANYWHERE, so CAN, SUB and a bare ESC all leave
// the string and everything after is parsed live. A walker that runs past one
// forwards a query the local tmux then answers — reopening #338 in 11 bytes.
// The DCS handler has no such macro, which is what keeps "\ePtmux;" intact.
func TestFeedRegionEndsWhereTmuxEndsIt(t *testing.T) {
	const keep = "\x00" // sentinel for want == in
	cases := []struct{ name, in, want string }{
		{"OSC aborted by CAN", "\x1b]0;\x18\x1b[>4;2m\x07", "\x1b]0;\x18\x07"},
		{"OSC aborted by SUB", "\x1b]0;\x1a\x1b[>4;2m\x07", "\x1b]0;\x1a\x07"},
		{"OSC aborted by ESC", "\x1b]0;\x1b[>4;2m\x07", "\x1b]0;\x07"},
		{"OSC aborted by doubled ESC", "\x1b]0;\x1b\x1b[>4;2m\x07", "\x1b]0;\x1b\x07"},
		{"colour OSC aborted by CAN", "\x1b]11;\x18\x1b[6n\x07", "\x1b]11;\x18\x07"},
		{"colour OSC aborted by ESC", "\x1b]11;\x1b[6n\x07", "\x1b]11;\x07"},

		{"APC aborted by CAN", "\x1b_Ga=t;\x18\x1b[6n\x1b\\", "\x1b_Ga=t;\x18\x1b\\"},
		{"APC aborted by SUB", "\x1b_Ga=t;\x1a\x1b[6n\x1b\\", "\x1b_Ga=t;\x1a\x1b\\"},
		{"APC aborted by ESC", "\x1b_Ga=t;\x1b[6n\x1b\\", "\x1b_Ga=t;\x1b\\"},
		{"APC unchanged", "\x1b_Ga=t;abc\x1b\\", keep},
		{"APC keeps BEL", "\x1b_Ga=t;\x07abc\x1b\\", keep},

		{"DCS keeps CAN", "\x1bPtmux;\x18\x1b\x1b[6n\x1b\\", keep},
		{"DCS keeps SUB", "\x1bPtmux;\x1a\x1b\x1b[6n\x1b\\", keep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == keep {
				want = tc.in
			}
			if got := NewFilter().Feed([]byte(tc.in)); string(got) != want {
				t.Fatalf("Feed(%q) = %q, want %q", tc.in, got, want)
			}
		})
	}
}

// TestFeedHeldOSCScanIsIncremental: a colour query's terminator scan must
// resume where the last Feed left off. Rescanning the whole hold each time is
// quadratic in bytes a remote pane chooses to dribble — maxRegion bounds the
// memory, nothing bounded the CPU.
func TestFeedHeldOSCScanIsIncremental(t *testing.T) {
	f := NewFilter()
	f.Feed([]byte("\x1b]4;"))
	for n := range 4096 {
		if got := f.Feed([]byte("1")); len(got) != 0 {
			t.Fatalf("Feed() = %q, want nothing emitted while held", got)
		}
		if f.oscScan != len(f.held) {
			t.Fatalf("after %d held bytes: oscScan = %d, want %d", n+1, f.oscScan, len(f.held))
		}
	}
	if got := f.Feed([]byte(";?\x07")); len(got) != 0 {
		t.Fatalf("Feed() = %q, want the query stripped whole", got)
	}
}

// TestFeedHeldOSCResumesAcrossESCBoundary: the resume point must be AT a
// trailing lone ESC, not past it, or the pair that decides its meaning is
// split across two scans and the terminator is missed.
func TestFeedHeldOSCResumesAcrossESCBoundary(t *testing.T) {
	f := NewFilter()
	if got := f.Feed([]byte("\x1b]11;?\x1b")); len(got) != 0 {
		t.Fatalf("first Feed() = %q, want nothing emitted while held", got)
	}
	if got := f.Feed([]byte("\\A")); string(got) != "A" {
		t.Fatalf("second Feed() = %q, want %q", got, "A")
	}
}

// TestEndRegionStripsQueriesAfterAnOpenRegion: once the caller has returned
// the local parser to ground, a query must be stripped rather than walked past
// as region payload. Every region kind, including a DCS with a doubled ESC
// half-read.
func TestEndRegionStripsQueriesAfterAnOpenRegion(t *testing.T) {
	for _, open := range []string{"\x1bPx", "\x1bPx\x1b", "\x1b_x", "\x1b]2;title"} {
		f := NewFilter()
		if got := f.Feed([]byte(open)); string(got) != open {
			t.Fatalf("Feed(%q) = %q, want it forwarded", open, got)
		}
		f.EndRegion()
		if got, want := f.Feed([]byte("a\x1b[6n\x1b[>4;2m\x1b]11;?\x07b")), "ab"; string(got) != want {
			t.Errorf("after %q and EndRegion, Feed = %q, want %q", open, got, want)
		}
	}
}

// TestEndRegionKeepsHeldBytes: a held partial sequence never reached the local
// parser, so it still completes and is stripped after EndRegion.
func TestEndRegionKeepsHeldBytes(t *testing.T) {
	f := NewFilter()
	if got := f.Feed([]byte("a\x1b[6")); string(got) != "a" {
		t.Fatalf("Feed = %q, want the partial held", got)
	}
	f.EndRegion()
	if got := f.Feed([]byte("nb")); string(got) != "b" {
		t.Errorf("Feed = %q, want the completed query stripped", got)
	}
}

// TestPendingDCSEscape: only a DCS with a forwarded lone trailing ESC leaves
// the local parser in dcs_escape. A completed DCS terminates it, and an OSC or
// APC ends at a bare ESC, so neither is ever pending.
func TestPendingDCSEscape(t *testing.T) {
	f := NewFilter()
	if got := f.Feed([]byte("\x1bPx\x1b")); string(got) != "\x1bPx\x1b" {
		t.Fatalf("Feed = %q, want the DCS and its lone ESC forwarded", got)
	}
	if !f.PendingDCSEscape() {
		t.Error("PendingDCSEscape = false after a forwarded lone ESC inside a DCS")
	}
	// The next byte completes ESC ESC (still in the DCS) and clears the flag.
	f.Feed([]byte("x"))
	if f.PendingDCSEscape() {
		t.Error("PendingDCSEscape = true after a non-ESC byte arrived")
	}
	// A terminating \ ends the region.
	f = NewFilter()
	f.Feed([]byte("\x1bPa\x1b"))
	f.Feed([]byte("\\"))
	if f.PendingDCSEscape() {
		t.Error("PendingDCSEscape = true after the DCS was terminated")
	}
	// OSC and APC end at a bare ESC and are never pending.
	for _, open := range []string{"\x1b]2;title\x1b", "\x1b_x\x1b"} {
		f = NewFilter()
		f.Feed([]byte(open))
		if f.PendingDCSEscape() {
			t.Errorf("PendingDCSEscape = true for %q", open)
		}
	}
}
