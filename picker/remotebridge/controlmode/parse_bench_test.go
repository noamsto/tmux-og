package controlmode

import (
	"strings"
	"testing"
)

// buildOutputStreamForBench synthesizes n "%output %1 <payload>" lines, each
// carrying a payloadLen-byte plain-ASCII payload, mirroring the stream shape
// used by the daemon package's pipeline benchmark.
func buildOutputStreamForBench(n, payloadLen int) string {
	const chunk = "the quick brown fox jumps over the lazy dog "
	var payload strings.Builder
	for payload.Len() < payloadLen {
		payload.WriteString(chunk)
	}
	line := payload.String()[:payloadLen]

	var sb strings.Builder
	sb.Grow(n * (len(line) + 16))
	for range n {
		sb.WriteString("%output %1 ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// BenchmarkControlmodeReaderNext isolates Reader.Next()'s parsing throughput,
// separate from the daemon package's full pipeline benchmark, so a scanner-
// buffer or allocation change here shows up on its own.
func BenchmarkControlmodeReaderNext(b *testing.B) {
	const lines = 10000
	const payloadLen = 300
	stream := buildOutputStreamForBench(lines, payloadLen)
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()

	for b.Loop() {
		r := NewReader(strings.NewReader(stream))
		for {
			_, ok := r.Next()
			if !ok {
				break
			}
		}
	}
}
