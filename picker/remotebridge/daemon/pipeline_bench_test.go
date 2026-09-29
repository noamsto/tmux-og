package daemon

import (
	"net"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// buildOutputStream synthesizes a tmux control-mode stream of n "%output %1
// <payload>" lines, each carrying a payloadLen-byte plain-ASCII payload (no
// backslashes, so Unescape is a no-op and fed/received byte counts match
// exactly). Returns the stream and the total payload bytes across all lines.
func buildOutputStream(n, payloadLen int) (string, int64) {
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
	return sb.String(), int64(n) * int64(payloadLen)
}

// BenchmarkOutputPipeline measures end-to-end throughput of the daemon's
// output hot path: controlmode.Reader parsing -> Router.Route -> outputSink's
// pump goroutine -> wire.WriteFrame, drained by wire.ReadFrame on the other
// end of a net.Pipe the way a real renderer would.
func BenchmarkOutputPipeline(b *testing.B) {
	const lines = 10000
	const payloadLen = 300
	stream, wantTotal := buildOutputStream(lines, payloadLen)
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()

	for b.Loop() {
		local, remote := net.Pipe()
		sink := newOutputSink(local, nil)
		router := NewRouter()
		router.Register("%1", sink)

		done := make(chan int64, 1)
		go func() {
			var got int64
			for {
				f, err := wire.ReadFrame(remote)
				if err != nil {
					done <- got
					return
				}
				if f.Type == wire.FrameOutput {
					got += int64(len(f.Payload))
				}
				if got >= wantTotal {
					done <- got
					return
				}
			}
		}()

		rd := controlmode.NewReader(strings.NewReader(stream))
		for {
			line, ok := rd.Next()
			if !ok {
				break
			}
			if line.Kind == controlmode.Output {
				router.Route(line.Pane, line.Data)
			}
		}
		sink.Close()

		got := <-done
		_ = local.Close()
		_ = remote.Close()
		if got != wantTotal {
			b.Fatalf("pipeline delivered %d bytes, want %d", got, wantTotal)
		}
	}
}
