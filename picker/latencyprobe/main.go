// Command latencyprobe measures keystroke-to-echo latency against a live
// tmux server over a control-mode client: it sends `send-keys -l <marker>`
// into a pane and times how long the marker takes to come back in that
// pane's `%output`.
//
// Control mode is the right stand-in for the remote bridge's own path: the
// bridge daemon types into the remote pane via `send-keys` on a control
// client and reads the pane's echo back as `%output` notifications, so
// probing a pane the same way measures the same server-side latency the
// daemon would see, without running the bridge itself.
//
// -pane must name a quiet echo pane (e.g. one running `cat`): the marker is
// matched in the pane's contiguous output, so a pane printing its own output
// could split the marker across two `%output` notifications or bury it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// unescape reverses tmux control mode's octal escaping of %output payloads:
// tmux escapes every backslash and every byte outside printable ASCII as a
// backslash followed by three octal digits. A trailing backslash not
// followed by three octal digits is not an escape and is kept as-is.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v, ok := 0, true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// percentile returns the p-th percentile (0 <= p <= 1) of sorted using
// nearest-rank indexing. An empty slice reports 0.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

func main() {
	tmuxBin := flag.String("tmux", "tmux", "tmux binary")
	sock := flag.String("L", "probe", "socket name")
	sess := flag.String("t", "probe", "session to attach the control client to")
	pane := flag.String("pane", "", "pane id to type into (must be a quiet echo pane, e.g. running cat)")
	n := flag.Int("n", 200, "samples")
	interval := flag.Duration("interval", 50*time.Millisecond, "gap between keystrokes")
	timeout := flag.Duration("timeout", 5*time.Second, "per-sample timeout")
	label := flag.String("label", "", "label printed on the result line")
	slow := flag.Duration("slow", 20*time.Millisecond, "log samples slower than this to stderr")
	maxMissed := flag.Int("max-missed", 3, "stop after this many consecutive timeouts: the echo path is gone, not slow")
	flag.Parse()

	cmd := exec.Command(*tmuxBin, "-L", *sock, "-C", "attach", "-t", *sess) //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	cmd.Env = append(os.Environ(), "TMUX=")
	in, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "latencyprobe: stdin pipe:", err)
		os.Exit(1)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "latencyprobe: stdout pipe:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "latencyprobe: start:", err)
		os.Exit(1)
	}
	done := make(chan struct{})
	defer func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		<-done
		_ = cmd.Wait()
	}()

	var mu sync.Mutex
	pending := map[string]time.Time{}
	var lat []time.Duration
	buf := ""
	prefix := "%output " + *pane + " "
	go func() {
		defer close(done)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			now := time.Now()
			mu.Lock()
			buf += unescape(line[len(prefix):])
			for m, t0 := range pending {
				if !strings.Contains(buf, m) {
					continue
				}
				d := now.Sub(t0)
				lat = append(lat, d)
				if d > *slow {
					fmt.Fprintf(os.Stderr, "slow %s sent=%s lat=%sms\n", m, t0.Format("15:04:05.000000"), ms(d))
				}
				delete(pending, m)
			}
			if len(buf) > 256 {
				buf = buf[len(buf)-64:]
			}
			mu.Unlock()
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "latencyprobe: reading control client output:", err)
		}
	}()

	// Let the control client finish attaching before the first send-keys.
	time.Sleep(300 * time.Millisecond)

	timeouts, missed := 0, 0
	aborted := false
	for i := 0; i < *n && !aborted; i++ {
		m := fmt.Sprintf("q%dz", i+100000)
		mu.Lock()
		pending[m] = time.Now()
		mu.Unlock()
		_, _ = fmt.Fprintf(in, "send-keys -t %s -l %s\n", *pane, m)
		deadline := time.Now().Add(*timeout)
		for {
			mu.Lock()
			_, still := pending[m]
			mu.Unlock()
			if !still {
				missed = 0
				break
			}
			if time.Now().After(deadline) {
				mu.Lock()
				delete(pending, m)
				mu.Unlock()
				timeouts++
				missed++
				aborted = missed >= *maxMissed
				break
			}
			time.Sleep(200 * time.Microsecond)
		}
		time.Sleep(*interval)
	}

	mu.Lock()
	defer mu.Unlock()
	slices.Sort(lat)
	if aborted {
		fmt.Fprintf(os.Stderr, "latencyprobe: %d consecutive timeouts, gave up\n", missed)
	}
	fmt.Printf("%s aborted=%t n=%d timeouts=%d p50=%sms p95=%sms p99=%sms max=%sms\n",
		*label, aborted, len(lat), timeouts,
		ms(percentile(lat, 0.5)), ms(percentile(lat, 0.95)), ms(percentile(lat, 0.99)), ms(percentile(lat, 1)))
}
