package drainbuf

import (
	"strings"
	"sync"
	"testing"
)

func TestAppendThenTakeReturnsBytesInOrder(t *testing.T) {
	b := New(1024)
	b.Append([]byte("hello "))
	b.Append([]byte("world"))

	data, truncated, closed := b.Take()
	if string(data) != "hello world" {
		t.Fatalf("Take data = %q, want %q", data, "hello world")
	}
	if truncated {
		t.Fatalf("truncated = true, want false")
	}
	if closed {
		t.Fatalf("closed = true, want false")
	}
}

func TestTakeDrains(t *testing.T) {
	b := New(1024)
	b.Append([]byte("abc"))
	b.Take()

	data, _, _ := b.Take()
	if len(data) != 0 {
		t.Fatalf("second Take data = %q, want empty", data)
	}
}

func TestAppendBeyondCapKeepsNewestSuffixAndReportsTruncated(t *testing.T) {
	b := New(8)
	b.Append([]byte("aaaa"))
	b.Append([]byte("bcdefg")) // total 10 bytes past an 8-byte max -> drops oldest

	data, truncated, _ := b.Take()
	if !truncated {
		t.Fatalf("truncated = false, want true after overflow")
	}
	if len(data) > 8 {
		t.Fatalf("len(data) = %d, want <= max 8", len(data))
	}
	const full = "aaaabcdefg"
	if !strings.HasSuffix(full, string(data)) {
		t.Fatalf("data = %q, want a newest suffix of %q", data, full)
	}
}

func TestTruncatedFlagClearsAfterTake(t *testing.T) {
	b := New(4)
	b.Append([]byte("aaaaaa")) // overflow
	if _, truncated, _ := b.Take(); !truncated {
		t.Fatalf("first Take truncated = false, want true")
	}
	b.Append([]byte("xy"))
	if _, truncated, _ := b.Take(); truncated {
		t.Fatalf("second Take truncated = true, want false (flag should reset)")
	}
}

func TestAppendNeverBlocksWithoutConsumer(t *testing.T) {
	b := New(1024)
	done := make(chan struct{})
	go func() {
		chunk := make([]byte, 512)
		for range 100_000 {
			b.Append(chunk)
		}
		close(done)
	}()
	<-done // must complete; a blocking Append would deadlock here
}

func TestCloseReportedByTake(t *testing.T) {
	b := New(1024)
	b.Close()
	if _, _, closed := b.Take(); !closed {
		t.Fatalf("closed = false after Close(), want true")
	}
}

func TestNotifyPulsesOnAppend(t *testing.T) {
	b := New(1024)
	b.Append([]byte("x"))
	select {
	case <-b.Notify():
	default:
		t.Fatalf("Notify() did not fire after Append")
	}
}

func TestAppendStaysCheapWhenFull(t *testing.T) {
	// Regression: a full buffer must not reallocate+copy the whole cap on every
	// Append, or the stdin drain becomes O(cap)/byte under sustained overload
	// and tmux buffers the backlog in-server anyway.
	b := New(1 << 16)
	chunk := make([]byte, 512)
	for range 1000 { // drive well past cap so we're in steady overflow
		b.Append(chunk)
	}
	allocs := testing.AllocsPerRun(2000, func() { b.Append(chunk) })
	if allocs >= 1.0 {
		t.Fatalf("Append allocs/op = %.3f when full, want <1 (amortized trim)", allocs)
	}
}

func TestConcurrentAppendAndTakeRace(t *testing.T) {
	b := New(4096)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		chunk := make([]byte, 128)
		for range 10_000 {
			b.Append(chunk)
		}
		b.Close()
	}()
	go func() {
		defer wg.Done()
		for {
			_, _, closed := b.Take()
			if closed {
				return
			}
		}
	}()
	wg.Wait()
}
