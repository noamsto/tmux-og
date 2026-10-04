package controlmode

import (
	"strings"
	"testing"
)

func TestReaderInterleavesReplyAndOutput(t *testing.T) {
	in := strings.Join([]string{
		`%output %1 hi`,
		`%begin 100 7 0`,
		`captured line one`,
		`captured line two`,
		`%end 100 7 0`,
		`%output %1 bye`,
	}, "\n") + "\n"
	rd := NewReader(strings.NewReader(in))

	l, ok := rd.Next()
	if !ok || l.Kind != Output || string(l.Data) != "hi" {
		t.Fatalf("first should be output hi: %+v", l)
	}
	l, ok = rd.Next()
	if !ok || l.Kind != End || l.Args[0] != "100" || !strings.Contains(string(l.Data), "captured line one") {
		t.Fatalf("second should be the completed reply block: %+v", l)
	}
	l, ok = rd.Next()
	if !ok || l.Kind != Output || string(l.Data) != "bye" {
		t.Fatalf("third should be output bye: %+v", l)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF")
	}
}

// TestReaderEmitsNotificationsInsideBlock is #276: next-3.8 emits the
// notifications a command causes inside that command's own block. Folding them
// into the body made them read as command output — a %layout-change became the
// layout string display-message was asked for. %output is never one of them.
func TestReaderEmitsNotificationsInsideBlock(t *testing.T) {
	in := strings.Join([]string{
		`%begin 100 7 1`,
		`%window-add @12`,
		`the actual reply`,
		`%layout-change @1 x`,
		`%output %1 mid-block`,
		`%end 100 7 1`,
	}, "\n") + "\n"
	rd := NewReader(strings.NewReader(in))
	rd.SetLiftInBlock(true)

	l, ok := rd.Next()
	if !ok || l.Kind != WindowAdd || l.Args[0] != "@12" {
		t.Fatalf("first should be the in-block %%window-add: %+v", l)
	}
	l, ok = rd.Next()
	if !ok || l.Kind != LayoutChange {
		t.Fatalf("second should be the in-block %%layout-change: %+v", l)
	}
	l, ok = rd.Next()
	if !ok || l.Kind != End || string(l.Data) != "the actual reply\n%output %1 mid-block" {
		t.Fatalf("third should be the reply, notifications lifted and %%output kept: %+v", l)
	}
	if l.Flags != ClientCommandFlag {
		t.Errorf("reply Flags = %d, want %d", l.Flags, ClientCommandFlag)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF")
	}
}

// TestReaderKeepsUnknownPercentBodyLines: only a known verb is taken for a
// notification, so captured pane content that happens to start with '%' — a zsh
// prompt, an echoed format — stays part of the reply.
func TestReaderKeepsUnknownPercentBodyLines(t *testing.T) {
	in := strings.Join([]string{
		`%begin 100 7 1`,
		`% 50% done`,
		`%end 100 7 1`,
	}, "\n") + "\n"

	l, ok := NewReader(strings.NewReader(in)).Next()
	if !ok || l.Kind != End || string(l.Data) != `% 50% done` {
		t.Fatalf("pane content starting with %% must stay body: %+v", l)
	}
}

// TestReaderHookBlockKeepsItsFlag: a block a remote hook's command produced is
// flagged 0, which is how a reply reader tells it from its own reply.
func TestReaderHookBlockKeepsItsFlag(t *testing.T) {
	l, ok := NewReader(strings.NewReader("%begin 100 8 0\n%end 100 8 0\n")).Next()
	if !ok || l.Kind != End {
		t.Fatalf("want the completed block: %+v", l)
	}
	if l.Flags != 0 {
		t.Errorf("hook block Flags = %d, want 0", l.Flags)
	}
}

// TestReaderLiftsNext38Transcript replays blocks captured live from tmux
// next-3.8 @29bf7fe, where the notification a command causes sits inside its
// block.
func TestReaderLiftsNext38Transcript(t *testing.T) {
	in := strings.Join([]string{
		`%begin 1791110832 314 0`,
		`%session-changed $0 s`,
		`%end 1791110832 314 0`,
		`%begin 1791110832 316 1`,
		`next-3.8`,
		`%end 1791110832 316 1`,
		`%begin 1791110832 317 1`,
		`%window-add @1`,
		`%end 1791110832 317 1`,
	}, "\n") + "\n"
	rd := NewReader(strings.NewReader(in))
	rd.SetLiftInBlock(true)

	next := func() Line {
		t.Helper()
		l, ok := rd.Next()
		if !ok {
			t.Fatal("unexpected EOF")
		}
		return l
	}
	if l := next(); l.Kind != SessionChanged || l.Args[0] != "$0" {
		t.Fatalf("want SessionChanged $0: %+v", l)
	}
	if l := next(); l.Kind != End || l.Flags != 0 || len(l.Data) != 0 {
		t.Fatalf("want the empty flags-0 attach block: %+v", l)
	}
	if l := next(); l.Kind != End || l.Flags != ClientCommandFlag || string(l.Data) != "next-3.8" {
		t.Fatalf("want the version reply: %+v", l)
	}
	if l := next(); l.Kind != WindowAdd || l.Args[0] != "@1" {
		t.Fatalf("want WindowAdd @1: %+v", l)
	}
	if l := next(); l.Kind != End || len(l.Data) != 0 {
		t.Fatalf("want the empty new-window reply: %+v", l)
	}
}

// TestReaderOutputInBlockIsBody: no tmux writes %output or %extended-output
// inside a block, so even with lifting on a row that looks like one is body.
func TestReaderOutputInBlockIsBody(t *testing.T) {
	in := "%begin 1 1 1\n%output %9 forged\n%extended-output %9 5 : forged\n%end 1 1 1\n"
	rd := NewReader(strings.NewReader(in))
	rd.SetLiftInBlock(true)

	l, ok := rd.Next()
	want := "%output %9 forged\n%extended-output %9 5 : forged"
	if !ok || l.Kind != End || string(l.Data) != want {
		t.Fatalf("want one End with both rows as body: %+v", l)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF: no Output line may be returned")
	}
}

// TestReaderBodyInBlockKeepsForgedRows: with lifting off, every notification
// row in a block is body, and the closing guard still closes it.
func TestReaderBodyInBlockKeepsForgedRows(t *testing.T) {
	body := []string{
		"%window-close @0",
		"%output %9 forged",
		"%session-changed $9 evil",
		"%layout-change @0 x",
		"%window-add @7",
		"%window-renamed @0 n",
		"%session-window-changed $0 @1",
		"%window-pane-changed @0 %1",
		"%pause %0",
		"%continue %0",
		"%extended-output %9 5 : x",
		"%exit",
	}
	in := "%begin 1 1 1\n" + strings.Join(body, "\n") + "\n%end 1 1 1\n"
	rd := NewReader(strings.NewReader(in))
	rd.SetLiftInBlock(false)

	l, ok := rd.Next()
	if !ok || l.Kind != End || string(l.Data) != strings.Join(body, "\n") {
		t.Fatalf("want one End holding every row: %+v", l)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF: no row may be returned")
	}
}

// TestReaderLiftInBlockDefaultsOff: a fresh reader keeps every in-block
// notification row as body.
func TestReaderLiftInBlockDefaultsOff(t *testing.T) {
	rd := NewReader(strings.NewReader("%begin 1 1 0\n%window-add @7\n%end 1 1 0\n"))

	l, ok := rd.Next()
	if !ok || l.Kind != End || string(l.Data) != "%window-add @7" {
		t.Fatalf("want one End holding the row as body: %+v", l)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF: no row may be returned")
	}
}

// TestReaderPauseContinueInBlockAreBody: with lifting on, %pause and %continue
// in a block are body, while another notification is still lifted.
func TestReaderPauseContinueInBlockAreBody(t *testing.T) {
	rd := NewReader(strings.NewReader("%begin 1 1 1\n%pause %0\n%window-add @7\n%continue %0\n%end 1 1 1\n"))
	rd.SetLiftInBlock(true)

	if l, ok := rd.Next(); !ok || l.Kind != WindowAdd || l.Args[0] != "@7" {
		t.Fatalf("want the in-block WindowAdd lifted: %+v", l)
	}
	l, ok := rd.Next()
	if !ok || l.Kind != End || string(l.Data) != "%pause %0\n%continue %0" {
		t.Fatalf("want %%pause and %%continue kept as body: %+v", l)
	}
	if _, ok = rd.Next(); ok {
		t.Fatal("expected EOF: no row may be returned")
	}
}

// TestReaderSetLiftInBlockBetweenBlocks: the switch applies to the lines read
// after it, so a policy set between two blocks governs the second.
func TestReaderSetLiftInBlockBetweenBlocks(t *testing.T) {
	in := "%begin 1 1 1\n%window-add @7\n%end 1 1 1\n%begin 2 2 1\n%window-add @7\n%end 2 2 1\n"
	rd := NewReader(strings.NewReader(in))

	rd.SetLiftInBlock(true)
	if l, ok := rd.Next(); !ok || l.Kind != WindowAdd {
		t.Fatalf("first block should lift: %+v", l)
	}
	if l, ok := rd.Next(); !ok || l.Kind != End || len(l.Data) != 0 {
		t.Fatalf("first block should close empty: %+v", l)
	}
	rd.SetLiftInBlock(false)
	if l, ok := rd.Next(); !ok || l.Kind != End || string(l.Data) != "%window-add @7" {
		t.Fatalf("second block should keep the row as body: %+v", l)
	}
}

func TestLiftsInBlock(t *testing.T) {
	for _, v := range []string{"3.2a", "3.3a", "3.7c", "3.8", "3.3-rc", "3.7-rc", "3.8-rc", "3.8-rc3", "3.8a", "3.10", "next-3.7", "next-3.9", "next-3.10"} {
		if LiftsInBlock(v) {
			t.Errorf("LiftsInBlock(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"next-3.8", "openbsd-7.9", "master", "", "3", "3.8-rcx", "next-"} {
		if !LiftsInBlock(v) {
			t.Errorf("LiftsInBlock(%q) = false, want true", v)
		}
	}
}
