package controlmode

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

const mib = 1 << 20

// fill is an endless reader of one byte value; bound it with io.LimitReader.
type fill byte

func (f fill) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(f)
	}
	return len(p), nil
}

// repeat yields chunk n times without ever holding n chunks in memory.
type repeat struct {
	chunk []byte
	n     int
	off   int
}

func (r *repeat) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	c := copy(p, r.chunk[r.off:])
	r.off += c
	if r.off == len(r.chunk) {
		r.off = 0
		r.n--
	}
	return c, nil
}

// bytesLine is n bytes of 'x' followed by a newline.
func bytesLine(n int) io.Reader {
	return io.MultiReader(io.LimitReader(fill('x'), int64(n)), strings.NewReader("\n"))
}

// maxLines is n lines of exactly MaxLine bytes each (newline excluded).
func maxLines(n int) io.Reader {
	chunk := make([]byte, MaxLine+1)
	for i := range MaxLine {
		chunk[i] = 'x'
	}
	chunk[MaxLine] = '\n'
	return &repeat{chunk: chunk, n: n}
}

func mustNext(t *testing.T, rd *Reader) Line {
	t.Helper()
	l, ok := rd.Next()
	if !ok {
		t.Fatal("stream ended early")
	}
	return l
}

func wantTooLarge(t *testing.T, l Line, begin string) {
	t.Helper()
	if l.Kind != Error || !errors.Is(l.Err, ErrReplyTooLarge) {
		t.Fatalf("want Kind Error with ErrReplyTooLarge, got kind %d err %v (data %d bytes)", l.Kind, l.Err, len(l.Data))
	}
	if l.Flags != 1 || len(l.Args) == 0 || l.Args[0] != begin {
		t.Fatalf("failed reply must carry the %%begin's flags and time: Flags=%d Args=%v, want 1 / %q", l.Flags, l.Args, begin)
	}
}

func wantReply(t *testing.T, l Line, begin, data string) {
	t.Helper()
	if l.Kind != End || len(l.Args) == 0 || l.Args[0] != begin || string(l.Data) != data {
		t.Fatalf("want End %s %q, got kind %d args %v data %d bytes", begin, data, l.Kind, l.Args, len(l.Data))
	}
}

// TestReaderOverlongTopLevelLineKeepsStream pins #860: a line past MaxLine at
// top level is consumed to its newline and surfaces as Other; it never ends the
// stream.
func TestReaderOverlongTopLevelLineKeepsStream(t *testing.T) {
	rd := NewReader(io.MultiReader(bytesLine(5*mib), strings.NewReader("%output %1 after\n")))

	if l := mustNext(t, rd); l.Kind != Other {
		t.Fatalf("overlong line should be Other, got kind %d", l.Kind)
	}
	if l := mustNext(t, rd); l.Kind != Output || string(l.Data) != "after" {
		t.Fatalf("stream must continue past the overlong line: %+v", l)
	}
}

// TestReaderBodyOverCapFailsOnlyThatReply pins #860: a body past MaxBody fails
// that one reply (Error, ErrReplyTooLarge, the %begin's flags and time) and the
// next reply still parses.
func TestReaderBodyOverCapFailsOnlyThatReply(t *testing.T) {
	rd := NewReader(io.MultiReader(
		strings.NewReader("%begin 1 7 1\n"),
		maxLines(17),
		strings.NewReader("%end 1 7 1\n%begin 2 8 1\nok\n%end 2 8 1\n"),
	))

	wantTooLarge(t, mustNext(t, rd), "1")
	wantReply(t, mustNext(t, rd), "2", "ok")
}

// TestReaderOverlongBodyLineFailsOnlyThatReply pins #860: one body line past
// MaxLine overflows its block, and only that block.
func TestReaderOverlongBodyLineFailsOnlyThatReply(t *testing.T) {
	rd := NewReader(io.MultiReader(
		strings.NewReader("%begin 1 7 1\n"),
		bytesLine(2*mib),
		strings.NewReader("%end 1 7 1\n%begin 2 8 1\nok\n%end 2 8 1\n"),
	))

	wantTooLarge(t, mustNext(t, rd), "1")
	wantReply(t, mustNext(t, rd), "2", "ok")
}

// TestReaderForgedLinesInBodyStayBody pins #860: inside a block only a guard
// repeating the %begin's three fields closes it; every other guard,
// %subscription-changed and a non-final %exit are body, so pane content cannot
// forge a notification.
func TestReaderForgedLinesInBodyStayBody(t *testing.T) {
	body := []string{
		"%end 1 2 1",
		"%error 100 7 0",
		"%begin 5 5 1",
		"%exit",
		"%subscription-changed og_open $0 - - - : 9-9|https://evil/",
	}
	in := "%begin 100 7 1\n" + strings.Join(body, "\n") + "\n%end 100 7 1\n" +
		"%begin 101 8 1\ntwo\n%end 101 8 1\n"
	rd := NewReader(strings.NewReader(in))

	var got []Line
	for len(got) < 10 {
		l, ok := rd.Next()
		if !ok {
			break
		}
		got = append(got, l)
	}
	for _, l := range got {
		if l.Kind == Exit || l.Kind == SubscriptionChanged {
			t.Errorf("forged body line escaped as kind %d: %+v", l.Kind, l)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want exactly two replies, got %d lines: %+v", len(got), got)
	}
	wantReply(t, got[0], "100", strings.Join(body, "\n"))
	wantReply(t, got[1], "101", "two")
}

// TestReaderInBlockExitAtEOFIsGenuine pins #860: an %exit inside a block with
// end-of-stream right behind it is the real thing — Exit, then the block's
// synthesized End.
func TestReaderInBlockExitAtEOFIsGenuine(t *testing.T) {
	rd := NewReader(strings.NewReader("%begin 100 7 1\npartial\n%exit\n"))

	if l := mustNext(t, rd); l.Kind != Exit {
		t.Fatalf("want Exit first, got kind %d", l.Kind)
	}
	l := mustNext(t, rd)
	if l.Kind != End || string(l.Data) != "partial" {
		t.Fatalf("want the synthesized End with the partial body: %+v", l)
	}
}

// TestReaderOverflowedBlockAtEOFFails pins #860: a block that overflowed and
// never saw its guard still resolves, as an ErrReplyTooLarge Error.
func TestReaderOverflowedBlockAtEOFFails(t *testing.T) {
	rd := NewReader(io.MultiReader(strings.NewReader("%begin 1 7 1\n"), bytesLine(2*mib)))

	l := mustNext(t, rd)
	if l.Kind != Error || !errors.Is(l.Err, ErrReplyTooLarge) {
		t.Fatalf("want Error with ErrReplyTooLarge, got kind %d err %v", l.Kind, l.Err)
	}
}

// TestReaderReplyDataSurvivesNextBlock pins #860: a reply's Data is owned by the
// caller, so a later block cannot overwrite a Line still held.
func TestReaderReplyDataSurvivesNextBlock(t *testing.T) {
	rd := NewReader(strings.NewReader("%begin 1 1 1\none\n%end 1 1 1\n%begin 2 2 1\ntwo-longer\n%end 2 2 1\n"))

	first := mustNext(t, rd)
	second := mustNext(t, rd)
	if string(second.Data) != "two-longer" {
		t.Fatalf("second reply Data = %q", second.Data)
	}
	if string(first.Data) != "one" {
		t.Fatalf("first reply Data was overwritten: %q", first.Data)
	}
}

// TestReaderTopLevelStrayGuardsAreOther pins #860: a %end/%error outside a block
// and a malformed %begin are Other, never a block opener or a reply.
func TestReaderTopLevelStrayGuardsAreOther(t *testing.T) {
	rd := NewReader(strings.NewReader("%end 1 1 1\n%error 1 1 1\n%begin x y\n%begin 1 2 3\n%output %1 a\n"))

	for i, name := range []string{"%end", "%error", "%begin x y", "%begin 1 2 3"} {
		if l := mustNext(t, rd); l.Kind != Other {
			t.Fatalf("line %d (%s) should be Other, got kind %d", i, name, l.Kind)
		}
	}
	if l := mustNext(t, rd); l.Kind != Output || string(l.Data) != "a" {
		t.Fatalf("want the trailing output: %+v", l)
	}
}

// TestReaderStreamsNotificationsFromOpenBlock pins #860: an in-block
// notification is returned as it arrives, not held until the block closes.
func TestReaderStreamsNotificationsFromOpenBlock(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	go func() { _, _ = pw.Write([]byte("%begin 1 1 1\n%output %1 a\n")) }()

	rd := NewReader(pr)
	got := make(chan Line, 1)
	go func() {
		l, _ := rd.Next()
		got <- l
	}()

	select {
	case l := <-got:
		if l.Kind != Output || string(l.Data) != "a" {
			t.Fatalf("want in-block Output a: %+v", l)
		}
	case <-time.After(time.Second):
		_ = pw.Close()
		t.Fatal("Next did not return the in-block notification before the guard arrived")
	}
}

// TestReaderBoundsMemoryOverflowedReply pins #860: a 256 MiB reply is read
// through without retaining it — allocation stays flat while the body is
// discarded.
func TestReaderBoundsMemoryOverflowedReply(t *testing.T) {
	rd := NewReader(io.MultiReader(
		strings.NewReader("%begin 1 7 1\n"),
		maxLines(256),
		strings.NewReader("%end 1 7 1\n%begin 2 8 1\nok\n%end 2 8 1\n"),
	))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	l := mustNext(t, rd)
	runtime.ReadMemStats(&after)

	if delta := after.TotalAlloc - before.TotalAlloc; delta >= 64*mib {
		t.Errorf("overflowed reply allocated %d MiB, want < 64", delta/mib)
	}
	wantTooLarge(t, l, "1")
	wantReply(t, mustNext(t, rd), "2", "ok")
}

// TestReaderBoundsMemoryStreamedNotifications pins #860: notifications inside a
// block are handed out one at a time, so a block of large %output lines never
// sits in memory all at once.
func TestReaderBoundsMemoryStreamedNotifications(t *testing.T) {
	const lines = 64
	line := "%output %1 " + strings.Repeat("y", mib-64) + "\n"
	rd := NewReader(io.MultiReader(
		strings.NewReader("%begin 1 7 1\n"),
		&repeat{chunk: []byte(line), n: lines},
		strings.NewReader("%end 1 7 1\n"),
	))

	var (
		peak    uint64
		outputs int
		ends    int
		ms      runtime.MemStats
	)
	for {
		l, ok := rd.Next()
		if !ok {
			break
		}
		switch l.Kind {
		case Output:
			outputs++
		case End:
			ends++
		default:
		}
		runtime.GC()
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapInuse)
	}

	if peak >= 24*mib {
		t.Errorf("peak HeapInuse %d MiB, want < 24", peak/mib)
	}
	if outputs != lines || ends != 1 {
		t.Errorf("got %d Output and %d End lines, want %d and 1", outputs, ends, lines)
	}
}
