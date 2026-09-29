// Command keyprobe measures keypress-to-repaint latency of a full-screen
// program in a tmux pane (the pickers) over a control-mode client: it sends
// a key with `send-keys` and times how long the pane takes to emit `%output`.
//
// Two numbers per sample, both from the moment the key is written:
//   - first: the first %output chunk, i.e. the repaint the key caused;
//   - settled: the last chunk before -quiet of silence, which also covers
//     anything that lands later (a preview arriving after a cursor move).
//
// Keys are tmux key names (Down, Up, BSpace, a, C-g). Each sample sends the
// next one from -keys in rotation, then waits for the pane to go quiet so the
// next sample starts from rest. With -burst N, N keys go out back to back at
// -burst-gap (a held key's repeat rate) and one sample is the time from the
// first key to the pane going quiet.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(p*float64(len(sorted)-1))]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

// summarize renders "name n= p50= p95= max=" for a set of samples.
func summarize(name string, d []time.Duration) string {
	s := append([]time.Duration(nil), d...)
	slices.Sort(s)
	return fmt.Sprintf("%s n=%d p50=%sms p95=%sms max=%sms", name, len(s),
		ms(percentile(s, 0.5)), ms(percentile(s, 0.95)), ms(percentile(s, 1)))
}

// waitQuiet drains ticks until none arrives for quiet, returning the time of
// the last one seen (zero if none).
func waitQuiet(ticks <-chan time.Time, quiet time.Duration) time.Time {
	var last time.Time
	for {
		select {
		case t := <-ticks:
			last = t
		case <-time.After(quiet):
			return last
		}
	}
}

func main() {
	tmuxBin := flag.String("tmux", "tmux", "tmux binary")
	sock := flag.String("L", "probe", "socket name")
	sess := flag.String("t", "probe", "session to attach the control client to")
	pane := flag.String("pane", "", "pane id running the program (required)")
	keys := flag.String("keys", "Down,Up", "comma-separated tmux key names, sent in rotation")
	n := flag.Int("n", 100, "samples")
	quiet := flag.Duration("quiet", 80*time.Millisecond, "silence that ends a sample")
	burst := flag.Int("burst", 0, "keys per sample sent back to back at -burst-gap (0: one key per sample)")
	burstGap := flag.Duration("burst-gap", 33*time.Millisecond, "gap between keys in a burst (a held key repeats at ~30 Hz)")
	size := flag.String("size", "", "WxH to give the control client, so it does not resize the session's windows")
	jitter := flag.Duration("jitter", 20*time.Millisecond, "random delay before each sample, so samples do not lock phase with the program's frame ticker")
	label := flag.String("label", "", "label printed on the result lines")
	flag.Parse()
	if *pane == "" {
		fmt.Fprintln(os.Stderr, "keyprobe: -pane is required")
		os.Exit(2)
	}
	seq := strings.Split(*keys, ",")

	cmd := exec.Command(*tmuxBin, "-L", *sock, "-C", "attach", "-t", *sess) //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	cmd.Env = append(os.Environ(), "TMUX=")
	in, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "keyprobe:", err)
		os.Exit(1)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "keyprobe:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "keyprobe:", err)
		os.Exit(1)
	}
	defer func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	ticks := make(chan time.Time, 4096)
	prefix := "%output " + *pane + " "
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), prefix) {
				ticks <- time.Now()
			}
		}
		if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
			fmt.Fprintln(os.Stderr, "keyprobe: reading control client output:", err)
		}
	}()

	if *size != "" {
		_, _ = fmt.Fprintf(in, "refresh-client -C %s\n", *size)
	}
	time.Sleep(300 * time.Millisecond)
	waitQuiet(ticks, 4**quiet)

	var first, settled []time.Duration
	for i := 0; i < *n; i++ {
		if *jitter > 0 {
			time.Sleep(rand.N(*jitter)) //nolint:gosec // probe jitter only, not security-sensitive
		}
		t0 := time.Now()
		if *burst > 0 {
			for j := 0; j < *burst; j++ {
				_, _ = fmt.Fprintf(in, "send-keys -t %s %s\n", *pane, seq[(i**burst+j)%len(seq)])
				time.Sleep(*burstGap)
			}
		} else {
			_, _ = fmt.Fprintf(in, "send-keys -t %s %s\n", *pane, seq[i%len(seq)])
		}
		select {
		case t := <-ticks:
			first = append(first, t.Sub(t0))
			if last := waitQuiet(ticks, *quiet); !last.IsZero() {
				t = last
			}
			settled = append(settled, t.Sub(t0))
		case <-time.After(2 * time.Second):
			fmt.Fprintf(os.Stderr, "keyprobe: sample %d: no output, is %s running a full-screen program?\n", i, *pane)
		}
	}
	prefixed := func(s string) string { return strings.TrimSpace(*label + " " + s) }
	if *burst > 0 {
		fmt.Println(prefixed(summarize(fmt.Sprintf("burst%d-settled", *burst), settled)))
		return
	}
	fmt.Println(prefixed(summarize("first", first)))
	fmt.Println(prefixed(summarize("settled", settled)))
}
