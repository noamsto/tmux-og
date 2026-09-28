package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

type scriptedReply struct {
	line controlmode.Line
	ok   bool
}

func okReply(l controlmode.Line) scriptedReply { return scriptedReply{l, true} }

var errReply = okReply(controlmode.Line{Kind: controlmode.Error})

// scriptRT records every command as it is issued and answers them from
// script, in order across calls; an exhausted script is a lost stream.
func scriptRT(issued *[]string, script ...scriptedReply) roundTrip {
	return func(cmds ...string) replies {
		*issued = append(*issued, cmds...)
		return func() (controlmode.Line, bool) {
			if len(script) == 0 {
				return controlmode.Line{}, false
			}
			r := script[0]
			script = script[1:]
			return r.line, r.ok
		}
	}
}

type openerProbe struct {
	opened   []string
	notified []string
	openErr  error
	clock    time.Time
}

func (p *openerProbe) opener(session string) *urlOpener {
	return &urlOpener{
		host:    "devbox",
		session: session,
		seen:    map[string]bool{},
		open:    func(u string) error { p.opened = append(p.opened, u); return p.openErr },
		notify:  func(m string) { p.notified = append(p.notified, m) },
		launch:  func(f func()) { f() },
		now:     func() time.Time { return p.clock },
	}
}

// openLog builds a log of n fresh records whose nonces start at 1-first.
func openLog(first, n int) string {
	var b strings.Builder
	for i := first; i < first+n; i++ {
		fmt.Fprintf(&b, " 1-%d|http://a", i)
	}
	return b.String()
}

func TestURLOpenerCapsOpensPerReport(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	// As many records as fit under the value bound; 1000 would exceed it.
	v := openLog(1000, 700)
	if len(v) > openValueMaxLen {
		t.Fatalf("test value is %d bytes, over openValueMaxLen", len(v))
	}
	o.handle("$1", v)
	if len(p.opened) != openMaxPerReport {
		t.Errorf("opened %d, want %d", len(p.opened), openMaxPerReport)
	}
	if want := []string{"og-open: suppressed 684 URL opens from devbox (rate limit)"}; !reflect.DeepEqual(p.notified, want) {
		t.Errorf("notified %q, want %q", p.notified, want)
	}
	if len(o.seen) != 700 {
		t.Errorf("seen %d nonces, want all 700 so suppressed records never open later", len(o.seen))
	}
	p.opened = nil
	o.handle("$1", v)
	if len(p.opened) != 0 {
		t.Errorf("a suppressed record opened on the next report: %v", p.opened)
	}
}

func TestURLOpenerTokenBucket(t *testing.T) {
	p := &openerProbe{clock: time.Unix(1000, 0)}
	o := p.opener("$1")
	o.handle("$1", openLog(1, 16))
	o.handle("$1", openLog(17, 16))
	if len(p.opened) != openBurst {
		t.Errorf("opened %d, want the burst of %d", len(p.opened), openBurst)
	}
	if want := []string{"og-open: suppressed 2 URL opens from devbox (rate limit)"}; !reflect.DeepEqual(p.notified, want) {
		t.Errorf("notified %q, want %q", p.notified, want)
	}

	p.opened, p.notified = nil, nil
	p.clock = p.clock.Add(20 * time.Second)
	o.handle("$1", openLog(33, 11))
	if len(p.opened) != 10 {
		t.Errorf("20s refilled %d opens, want 10", len(p.opened))
	}
	if want := []string{"og-open: suppressed 1 URL opens from devbox (rate limit)"}; !reflect.DeepEqual(p.notified, want) {
		t.Errorf("notified %q, want %q", p.notified, want)
	}
}

func TestURLOpenerInvalidTakesNoToken(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	var b strings.Builder
	for i := range openBurst {
		fmt.Fprintf(&b, " 1-%d|file:///x", i)
	}
	o.handle("$1", b.String())
	o.handle("$1", openLog(100, 1))
	if len(p.opened) != 1 || len(p.notified) != 0 {
		t.Errorf("opened %v notified %q: invalid URLs must not spend tokens", p.opened, p.notified)
	}
}

func TestURLOpenerIgnoresOversizedValue(t *testing.T) {
	big := " 1-5|https://x" + strings.Repeat(" 1-6|https://y", openValueMaxLen/14)
	if len(big) <= openValueMaxLen {
		t.Fatalf("test value is %d bytes, not over openValueMaxLen", len(big))
	}
	for _, v := range []string{big, openOversized} {
		p := &openerProbe{}
		o := p.opener("$1")
		o.seen["1-1"] = true
		o.handle("$1", v)
		if len(p.opened) != 0 {
			t.Errorf("%.20q: an oversized value opened %v", v, p.opened)
		}
		if want := map[string]bool{"1-1": true}; !reflect.DeepEqual(o.seen, want) {
			t.Errorf("%.20q: seen = %v, want it untouched by an oversized value", v, o.seen)
		}
		if want := []string{"og-open: ignored an oversized URL log from devbox"}; !reflect.DeepEqual(p.notified, want) {
			t.Errorf("%.20q: notified %q, want %q", v, p.notified, want)
		}
		o.handle("$1", " 1-5|https://x")
		if want := []string{"https://x"}; !reflect.DeepEqual(p.opened, want) {
			t.Errorf("%.20q: opened %v, want the record the oversized value carried", v, p.opened)
		}
	}
}

func TestParseOpenRecords(t *testing.T) {
	cases := []struct {
		in   string
		want []openRecord
	}{
		{" 1-2|https://a/x 1-3|https://b/y", []openRecord{{"1-2", "https://a/x"}, {"1-3", "https://b/y"}}},
		{"garbage 1-4|https://c", []openRecord{{"1-4", "https://c"}}},
		{"x-1|https://a", nil},
		{"1-|https://a", nil},
		{"|https://a", nil},
		{"12|https://a", nil},
		{"1-5|https://a/b|c", []openRecord{{"1-5", "https://a/b|c"}}},
		{"", nil},
	}
	for _, c := range cases {
		if got := parseOpenRecords(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseOpenRecords(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestValidOpenURL(t *testing.T) {
	for _, u := range []string{
		"http://a",
		"https://github.com/o/r/pull/1#issuecomment-2",
		"https://x/%20y",
		"https://例え.jp/",
	} {
		if !validOpenURL(u) {
			t.Errorf("validOpenURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"HTTPS://a",
		"-https://a",
		"https://",
		"http:///x",
		"https://a b",
		"https://a\tb",
		"https://a\x00",
		"https://a\x7f",
		"https://a/" + strings.Repeat("x", 4097-len("https://a/")),
		"",
	} {
		if validOpenURL(u) {
			t.Errorf("validOpenURL(%q) = true, want false", u)
		}
	}
	if long := "https://a/" + strings.Repeat("x", 4096-len("https://a/")); !validOpenURL(long) {
		t.Error("a 4096-byte URL is within the cap")
	}
}

func TestURLOpenerOpensOnlyUnseen(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	steps := []struct {
		value string
		want  []string
	}{
		{" 1-1|https://a 1-2|https://b", []string{"https://a", "https://b"}},
		{" 1-1|https://a 1-2|https://b 1-3|https://c", []string{"https://c"}},
		// og-open's cap reset: the log restarts from its newest record.
		{" 1-3|https://c", nil},
		{" 1-3|https://c 1-4|https://d", []string{"https://d"}},
	}
	for i, s := range steps {
		p.opened = nil
		o.handle("$1", s.value)
		if !reflect.DeepEqual(p.opened, s.want) {
			t.Errorf("step %d: opened %v, want %v", i, p.opened, s.want)
		}
	}
	if want := map[string]bool{"1-3": true, "1-4": true}; !reflect.DeepEqual(o.seen, want) {
		t.Errorf("seen = %v, want exactly the last value's nonces %v", o.seen, want)
	}
}

func TestURLOpenerSkipsInvalid(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	o.handle("$1", " 1-1|file:///etc/passwd junk 1-2|https://good")
	if want := []string{"https://good"}; !reflect.DeepEqual(p.opened, want) {
		t.Errorf("opened %v, want %v", p.opened, want)
	}
}

func TestURLOpenerIgnoresForeignSession(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	o.seen["1-1"] = true
	o.handle("$2", " 1-2|https://elsewhere")
	if len(p.opened) != 0 {
		t.Errorf("a foreign session's record opened: %v", p.opened)
	}
	if want := map[string]bool{"1-1": true}; !reflect.DeepEqual(o.seen, want) {
		t.Errorf("seen = %v, want it untouched by a foreign report", o.seen)
	}

	p = &openerProbe{}
	o = p.opener("")
	o.handle("$2", " 1-2|https://any")
	if want := []string{"https://any"}; !reflect.DeepEqual(p.opened, want) {
		t.Errorf("an unknown pin must accept every session: opened %v", p.opened)
	}
}

func connectCmds(target string) []string {
	return []string{
		"display-message -p" + target + " '#{?#{e|<=:#{n:@og_open_url},12288},#{@og_open_url},!oversized}'",
		"refresh-client -B 'og_open::#{?#{e|<=:#{n:@og_open_url},12288},#{@og_open_url},!oversized}'",
		"set-option -F" + target + " @og_open_client '#{client_name}'",
	}
}

func TestURLOpenerConnectOrder(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	var issued []string
	o.connect(scriptRT(&issued, okReply(body(" 1-1|https://old")), okReply(body("")), okReply(body(""))))
	if want := connectCmds(" -t '$1'"); !reflect.DeepEqual(issued, want) {
		t.Fatalf("issued = %q, want %q", issued, want)
	}
	if len(p.notified) != 0 {
		t.Errorf("a clean connect notified: %v", p.notified)
	}
	// The subscribe-time report: the seeded record is replayed, and one
	// appended between the seed and the subscribe rides along.
	o.handle("$1", " 1-1|https://old 1-2|https://new")
	if want := []string{"https://new"}; !reflect.DeepEqual(p.opened, want) {
		t.Errorf("opened %v, want only the record appended after the seed", p.opened)
	}
}

func TestURLOpenerReconnectReplaysNothing(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	var issued []string
	o.connect(scriptRT(&issued, okReply(body(" 1-1|https://old")), okReply(body("")), okReply(body(""))))
	o.handle("$1", " 1-1|https://old 1-2|https://new")

	p.opened = nil
	issued = nil
	o.connect(scriptRT(&issued, okReply(body(" 1-1|https://old 1-2|https://new")), okReply(body("")), okReply(body(""))))
	o.handle("$1", " 1-1|https://old 1-2|https://new")
	if len(p.opened) != 0 {
		t.Errorf("a reconnect replayed %v", p.opened)
	}
}

// connectWith runs a clean connect whose seed reply is seed.
func connectWith(o *urlOpener, seed string) []string {
	var issued []string
	o.connect(scriptRT(&issued, okReply(body(seed)), okReply(body("")), okReply(body(""))))
	return issued
}

func TestURLOpenerSeedReplacesSeen(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	o.seen["9-9"] = true
	connectWith(o, " 1-1|https://a")
	if want := map[string]bool{"1-1": true}; !reflect.DeepEqual(o.seen, want) {
		t.Errorf("seen = %v, want %v", o.seen, want)
	}
}

// A remote that forces reconnects, each seeded with a fresh log, must not
// grow seen past one log's worth.
func TestURLOpenerReconnectSeenStaysBounded(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("$1")
	for i := range 5 {
		connectWith(o, openLog(i*1000+1, 700))
		if len(o.seen) != 700 {
			t.Fatalf("reconnect %d: seen %d, want 700 (replace, not merge)", i, len(o.seen))
		}
	}
	connectWith(o, openOversized)
	if len(o.seen) != 700 {
		t.Errorf("an oversized seed changed seen to %d", len(o.seen))
	}
}

func TestURLOpenerOversizedSeedKeepsSeen(t *testing.T) {
	big := strings.Repeat(" 1-7|https://y", openValueMaxLen/14+1)
	if len(big) <= openValueMaxLen {
		t.Fatalf("test value is %d bytes, not over openValueMaxLen", len(big))
	}
	for _, seed := range []string{openOversized, big} {
		p := &openerProbe{}
		o := p.opener("$1")
		o.seen["1-1"] = true
		if issued := connectWith(o, seed); len(issued) != 3 {
			t.Errorf("%.20q: issued %q, an oversized seed must still subscribe and register", seed, issued)
		}
		if want := map[string]bool{"1-1": true}; !reflect.DeepEqual(o.seen, want) {
			t.Errorf("%.20q: seen = %v, want it untouched", seed, o.seen)
		}
		if len(p.notified) != 0 {
			t.Errorf("%.20q: an oversized seed notified %q", seed, p.notified)
		}
		o.handle("$1", openOversized)
		o.handle("$1", " 1-1|https://old 1-2|https://new")
		if want := []string{"https://new"}; !reflect.DeepEqual(p.opened, want) {
			t.Errorf("%.20q: opened %v, want %v", seed, p.opened, want)
		}
	}
}

func TestURLOpenerNoticesAreGated(t *testing.T) {
	p := &openerProbe{clock: time.Unix(1000, 0)}
	o := p.opener("$1")
	for range 100 {
		o.handle("$1", openOversized)
		var issued []string
		o.connect(scriptRT(&issued, errReply))
	}
	if len(p.notified) != 1 {
		t.Fatalf("notified %d times within openNoticeEvery, want 1", len(p.notified))
	}
	p.clock = p.clock.Add(openNoticeEvery)
	o.handle("$1", openOversized)
	if len(p.notified) != 2 {
		t.Fatalf("notified %d times, want a second notice once openNoticeEvery passed", len(p.notified))
	}
	var held []func()
	o.launch = func(f func()) { held = append(held, f) }
	p.clock = p.clock.Add(time.Hour)
	o.handle("$1", openOversized)
	p.clock = p.clock.Add(time.Hour)
	o.handle("$1", openOversized)
	if len(held) != 1 {
		t.Errorf("launched %d notices while one was in flight, want 1", len(held))
	}
}

func TestURLOpenerConnectFailures(t *testing.T) {
	cases := []struct {
		name   string
		script []scriptedReply
		issued int
		step   string
	}{
		{"seed error", []scriptedReply{errReply}, 1, "seed"},
		{"seed lost", nil, 1, "seed"},
		{"subscribe error", []scriptedReply{okReply(body("")), errReply}, 2, "subscribe"},
		{"register error", []scriptedReply{okReply(body("")), okReply(body("")), errReply}, 3, "register"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &openerProbe{}
			o := p.opener("$1")
			var issued []string
			o.connect(scriptRT(&issued, c.script...))
			if len(issued) != c.issued {
				t.Errorf("issued %q, want %d commands", issued, c.issued)
			}
			if len(p.notified) != 1 {
				t.Fatalf("notified %q, want exactly one notice", p.notified)
			}
			n := p.notified[0]
			if !strings.Contains(n, "("+c.step+" failed") || !strings.Contains(n, "devbox") {
				t.Errorf("notice %q must name the host and the %s step", n, c.step)
			}
		})
	}
}

func TestURLOpenerUnknownPinUntargeted(t *testing.T) {
	p := &openerProbe{}
	o := p.opener("")
	var issued []string
	o.connect(scriptRT(&issued, okReply(body("")), okReply(body("")), okReply(body(""))))
	if want := connectCmds(""); !reflect.DeepEqual(issued, want) {
		t.Errorf("issued = %q, want %q", issued, want)
	}
}

func TestURLOpenerNilOpenURLIsOff(t *testing.T) {
	o := newURLOpener(Config{}, "$1", nil)
	if o != nil {
		t.Fatalf("no opener must mean no urlOpener, got %+v", o)
	}
	var issued []string
	o.connect(scriptRT(&issued, okReply(body(" 1-1|https://a"))))
	o.handle("$1", " 1-1|https://a")
	if len(issued) != 0 {
		t.Errorf("a disabled opener issued %q", issued)
	}
	if newURLOpener(Config{}, "$1", func(string) error { return nil }) == nil {
		t.Error("an opener must build a urlOpener")
	}
}

func TestURLOpenerOpenErrorNotifies(t *testing.T) {
	p := &openerProbe{openErr: errors.New("exit status 3")}
	o := p.opener("$1")
	o.handle("$1", " 1-1|https://a/x")
	if len(p.notified) != 1 || !strings.Contains(p.notified[0], "https://a/x") || !strings.Contains(p.notified[0], "exit status 3") {
		t.Errorf("notified %q, want one notice naming the URL and the error", p.notified)
	}
}

func TestOpenSubscriptionSurvivesParseLine(t *testing.T) {
	l := controlmode.ParseLine("%subscription-changed og_open $1 - - - :  1-1|https://a 1-2|https://b|c")
	v, ok := subscriptionValue(l, openSubName)
	if !ok || len(l.Args) < 2 || l.Args[1] != "$1" {
		t.Fatalf("value %q ok=%v args=%q", v, ok, l.Args)
	}
	want := []openRecord{{"1-1", "https://a"}, {"1-2", "https://b|c"}}
	if got := parseOpenRecords(v); !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v, want %v", got, want)
	}
}

func TestBrowserOpenerArgv(t *testing.T) {
	if got := browserOpenerName("darwin"); got != "open" {
		t.Errorf("darwin opener = %q", got)
	}
	if got := browserOpenerName("linux"); got != "xdg-open" {
		t.Errorf("linux opener = %q", got)
	}
	got := envWithout([]string{"A=1", "BROWSER=x", "BROWSERX=2"}, "BROWSER")
	if want := []string{"A=1", "BROWSERX=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envWithout = %q, want %q", got, want)
	}
}

func TestRunBounded(t *testing.T) {
	if err := runBounded(exec.Command("sh", "-c", "exit 3"), time.Second); err == nil {
		t.Error("a non-zero exit within the bound must be an error")
	}

	start := time.Now()
	slow := exec.Command("sh", "-c", "sleep 5")
	// The child inherits the test binary's stderr, which go test waits on.
	t.Cleanup(func() { _ = slow.Process.Kill() })
	if err := runBounded(slow, 100*time.Millisecond); err != nil {
		t.Errorf("a child outliving the bound must be nil, got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("runBounded waited %v past a 100ms bound", d)
	}

	if err := runBounded(exec.Command("/nonexistent/og-open-opener"), time.Second); err == nil {
		t.Error("a missing binary must be an error")
	}

	t.Setenv("BROWSER", "x")
	if err := runBounded(exec.Command("sh", "-c", `test -z "${BROWSER+x}"`), time.Second); err != nil {
		t.Errorf("BROWSER must be stripped from the child's env: %v", err)
	}
}

// waitReplyLine returns the next command reply (End or Error) off lines,
// silently draining any notification riding inside the same guarded block —
// og_open's own initial subscription report, seen here as a bystander of the
// subscribe command's reply.
func waitReplyLine(t *testing.T, lines <-chan controlmode.Line, timeout time.Duration) controlmode.Line {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case l, more := <-lines:
			if !more {
				t.Fatal("control client closed its stream before a reply arrived")
			}
			if l.Kind == controlmode.End || l.Kind == controlmode.Error {
				return l
			}
		case <-deadline:
			t.Fatal("timed out waiting for a command reply")
		}
	}
}

// waitOpenRecords blocks until an og_open subscription report parses to want.
func waitOpenRecords(t *testing.T, lines <-chan controlmode.Line, want []openRecord) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, more := <-lines:
			if !more {
				t.Fatal("control client closed its stream before the subscription report arrived")
			}
			if v, ok := subscriptionValue(l, openSubName); ok && reflect.DeepEqual(parseOpenRecords(v), want) {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the og_open subscription report")
		}
	}
}

// TestOpenURLCommandsAgainstLiveTmux is the live-tmux counterpart to
// TestURLOpenerConnectOrder: a scripted roundTrip proves connect's command
// order, but only a real server can say the session-scoped subscribe spelling
// and the -F client-name expansion are what tmux actually does with them —
// same reasoning as TestSessionResSubscriptionIsSessionScoped, which this is
// modelled on.
func TestOpenURLCommandsAgainstLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also puts tmux in nativeBuildInputs — so a missing tmux there
		// means that input was pruned, not that this is a dev machine.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())

	if out, err := tmux("set-option", "-t", "w", openURLOpt, " 1-1|https://old").CombinedOutput(); err != nil {
		t.Fatalf("seed %s: %v\n%s", openURLOpt, err, out)
	}

	ctl := tmux("-C", "attach-session", "-t", "w")
	stdin, err := ctl.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := ctl.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.Start(); err != nil {
		t.Fatalf("control client: %v", err)
	}
	t.Cleanup(func() {
		ctl.Process.Kill()
		ctl.Wait()
	})

	lines := make(chan controlmode.Line, 256)
	go func() {
		defer close(lines)
		rd := controlmode.NewReader(stdout)
		for {
			l, ok := rd.Next()
			if !ok {
				return
			}
			lines <- l
		}
	}()

	// The implicit reply to the attach-session the transport itself ran —
	// nobody's command, but still the first guarded block on the stream.
	if l := waitReplyLine(t, lines, 5*time.Second); l.Kind == controlmode.Error {
		t.Fatalf("attach refused: %s", l.Data)
	}

	out, err := tmux("display", "-p", "-t", "w", "#{session_id}").CombinedOutput()
	if err != nil {
		t.Fatalf("session id: %v\n%s", err, out)
	}
	sessID := strings.TrimSpace(string(out))

	cmds := connectCmds(" -t " + tmuxQuote(sessID))
	for i, cmd := range cmds {
		if _, err := fmt.Fprintln(stdin, cmd); err != nil {
			t.Fatalf("send %q: %v", cmd, err)
		}
		l := waitReplyLine(t, lines, 5*time.Second)
		if l.Kind == controlmode.Error {
			t.Fatalf("cmd %d %q: %s", i, cmd, l.Data)
		}
		// The seed command is first, per connectCmds' order.
		if i == 0 {
			if got := string(l.Data); got != " 1-1|https://old" {
				t.Fatalf("seed reply = %q, want %q", got, " 1-1|https://old")
			}
		}
	}

	if out, err := tmux("set-option", "-a", "-t", "w", openURLOpt, " 1-2|https://new").CombinedOutput(); err != nil {
		t.Fatalf("append %s: %v\n%s", openURLOpt, err, out)
	}
	waitOpenRecords(t, lines, []openRecord{{"1-1", "https://old"}, {"1-2", "https://new"}})

	clientOpt, err := tmux("show-options", "-v", "-t", "w", openClientOpt).CombinedOutput()
	if err != nil {
		t.Fatalf("show-options %s: %v\n%s", openClientOpt, err, clientOpt)
	}
	clientName, err := tmux("list-clients", "-t", "w", "-F", "#{client_name}").CombinedOutput()
	if err != nil {
		t.Fatalf("list-clients: %v\n%s", err, clientName)
	}
	if got, want := strings.TrimSpace(string(clientOpt)), strings.TrimSpace(string(clientName)); got != want {
		t.Errorf("%s = %q, want the control client's name %q", openClientOpt, got, want)
	}
}

// TestOpenURLBoundAgainstLiveTmux proves openURLFormat's bound is what the
// remote enforces: in bytes, not display width, exact at openValueMaxLen, and
// with the value's own %-escapes, commas and braces passed through verbatim.
func TestOpenURLBoundAgainstLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		out, err := tmux(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	sessID := strings.TrimSpace(run("display-message", "-p", "-t", "w", "#{session_id}"))
	seed := func() string {
		t.Helper()
		return strings.TrimSuffix(run("display-message", "-p", "-t", sessID, openURLFormat), "\n")
	}

	verbatim := " 1-1|https://a/%41%2F%Y%%,#{session_id}}"
	run("set-option", "-t", "w", openURLOpt, verbatim)
	if got := seed(); got != verbatim {
		t.Errorf("seed = %q, want %q verbatim", got, verbatim)
	}

	// One display column, 30001 bytes: a width-based bound would pass it.
	run("set-option", "-t", "w", openURLOpt, "a")
	for range 5 {
		run("set-option", "-a", "-t", "w", openURLOpt, strings.Repeat("\u0301", 3000))
	}
	if got := seed(); got != openOversized {
		t.Errorf("a 1-column 30001-byte value: seed = %.40q, want %q", got, openOversized)
	}

	exact := strings.Repeat("x", openValueMaxLen)
	run("set-option", "-t", "w", openURLOpt, exact)
	if got := seed(); got != exact {
		t.Errorf("a value of exactly openValueMaxLen bytes must pass, got %d bytes", len(got))
	}
	run("set-option", "-a", "-t", "w", openURLOpt, "x")
	if got := seed(); got != openOversized {
		t.Errorf("openValueMaxLen+1 bytes: seed = %d bytes, want %q", len(got), openOversized)
	}
}
