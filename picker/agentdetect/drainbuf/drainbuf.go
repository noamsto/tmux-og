// Package drainbuf decouples a producer that must never block (the stdin
// reader draining a tmux pipe-pane) from a consumer that may lag (the
// CPU-bound VT emulator). Bytes are held in a bounded in-process buffer; if
// the consumer falls behind and the buffer would exceed its cap, the oldest
// bytes are dropped and the next Take reports Truncated so the caller can
// reset its emulator. This keeps the drain unconditional, so tmux never
// buffers the backlog in-server (which otherwise grows without bound and
// wedges the whole server).
package drainbuf

import "sync"

type Buffer struct {
	mu     sync.Mutex
	data   []byte
	max    int
	trunc  bool
	closed bool
	notify chan struct{}
}

func New(maxBytes int) *Buffer {
	// data grows on demand; a pane's backlog is usually a few KiB.
	return &Buffer{max: maxBytes, notify: make(chan struct{}, 1)}
}

// Append copies p into the buffer and never blocks. When the buffered length
// exceeds max, the oldest bytes are dropped down to max/2 (newest retained)
// and the truncated flag is set. Trimming to a low-water mark leaves headroom
// so the drop happens at most once per ~max/2 bytes — amortized O(len(p)) per
// call, even under sustained overflow — which is what keeps stdin draining
// fast enough that tmux never buffers the backlog itself.
func (b *Buffer) Append(p []byte) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		keep := min(b.max/2, len(b.data))
		b.data = append(b.data[:0], b.data[len(b.data)-keep:]...)
		b.trunc = true
	}
	b.mu.Unlock()
	b.pulse()
}

// Close marks EOF; the next Take reports closed.
func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.pulse()
}

// Take transfers all buffered bytes to the caller and clears the buffer. It
// returns whether bytes were dropped since the last Take (truncated) and
// whether the producer has closed. Non-blocking.
func (b *Buffer) Take() (data []byte, truncated, closed bool) {
	b.mu.Lock()
	if n := len(b.data); n > 0 {
		data = make([]byte, n)
		copy(data, b.data)
		b.data = b.data[:0] // reuse the grown backing array
	}
	truncated, b.trunc = b.trunc, false
	closed = b.closed
	b.mu.Unlock()
	return data, truncated, closed
}

// Notify fires (coalesced) whenever Append or Close is called, so a consumer
// can select on it alongside its own ticker.
func (b *Buffer) Notify() <-chan struct{} { return b.notify }

func (b *Buffer) pulse() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}
