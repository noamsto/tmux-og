package daemon

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// sizeDebug arms the size-decision trace (#1266). Off by default, so every log
// site below is one atomic load; runMirror arms it from Config.SizeDebug and
// never clears it, so a re-open keeps the same switch. It logs each size
// decision and the remote's reply to it.
var sizeDebug atomic.Bool

// sizeDebugW is where the trace lines go; tests swap it under sizeDebugMu so a
// captured buffer sees them instead of the test process's stderr.
var (
	sizeDebugMu sync.Mutex
	sizeDebugW  io.Writer = os.Stderr
)

// sizeDebugf writes one trace line when the switch is armed and returns after a
// single atomic load otherwise.
func sizeDebugf(format string, args ...any) {
	if !sizeDebug.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	sizeDebugMu.Lock()
	w := sizeDebugW
	sizeDebugMu.Unlock()
	_, _ = fmt.Fprintf(w, "daemon: size: %s\n", msg)
}

// setSizeDebugWriter swaps the trace sink and returns a restore func. Test-only;
// production never calls it.
func setSizeDebugWriter(w io.Writer) func() {
	sizeDebugMu.Lock()
	old := sizeDebugW
	sizeDebugW = w
	sizeDebugMu.Unlock()
	return func() {
		sizeDebugMu.Lock()
		sizeDebugW = old
		sizeDebugMu.Unlock()
	}
}

// isSizeTraced reports whether cmd is a size-decision command worth tracing: a
// whole-client or per-window size assert, or an ignore-size ownership flip.
// Evaluated only when the switch is on (see stampAll).
func isSizeTraced(cmd string) bool {
	return strings.HasPrefix(cmd, "refresh-client -C ") || strings.Contains(cmd, "ignore-size")
}

// sizeGeomSig renders a clientGeoms map deterministically (keys sorted) for the
// reveal trace. A nil map — the baseline before the first successful poll — is
// "none"; the caller computes it only when the switch is on.
func sizeGeomSig(m map[string]string) string {
	if m == nil {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
	}
	return b.String()
}

// traceBody renders remote-derived text for the trace. It trims it, drops
// every control character (printable), then caps it at 200 bytes, so a reply
// body or a remote-supplied command cannot forge log lines, emit terminal
// escape sequences, or flood the trace.
func traceBody(data []byte) string {
	s := printable(strings.TrimSpace(string(data)))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
