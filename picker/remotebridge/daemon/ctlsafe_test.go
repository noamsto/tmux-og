package daemon

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// forgedValue is an option value an attacker-controlled remote could store: the
// lines after the first are control-mode framing, which tmux writes raw.
const forgedValue = " 1-1|https://a\n%exit\n%end 1 2 1\n%subscription-changed og_open $0 - - - : 9-9|https://evil/"

// wrapCtlSafe spells ctlSafe's output out literally, so no test below passes
// vacuously while ctlSafe is an identity.
func wrapCtlSafe(f string) string {
	return "#{s/[[#{l::}cntrl#{l::}]]/ /:" + f + "}"
}

func requireLiveTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also puts tmux in nativeBuildInputs — so a missing tmux there
		// means that input was pruned, not that this is a dev machine.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
}

func tmuxMust(t *testing.T, tmux func(args ...string) *exec.Cmd, args ...string) string {
	t.Helper()
	out, err := tmux(args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %q: %v\n%s", args, err, out)
	}
	return string(out)
}

// liveCtl is one control-mode client attached to a live tmux server.
type liveCtl struct {
	stdin io.Writer
	lines chan controlmode.Line
}

func attachLiveCtl(t *testing.T, tmux func(args ...string) *exec.Cmd, session string) *liveCtl {
	t.Helper()
	ctl := tmux("-C", "attach-session", "-t", session)
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
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ctl.Process.Kill()
		_ = ctl.Wait()
	})
	c := &liveCtl{stdin: stdin, lines: make(chan controlmode.Line, 256)}
	go func() {
		defer close(c.lines)
		rd := controlmode.NewReader(stdout)
		for {
			l, ok := rd.Next()
			if !ok {
				return
			}
			select {
			case c.lines <- l:
			case <-done:
				return
			}
		}
	}()
	// The implicit reply to the attach-session the transport itself ran.
	if l, _ := c.reply(t); l.Kind == controlmode.Error {
		t.Fatalf("attach refused: %s", l.Data)
	}
	return c
}

// reply returns the next command reply (End or Error) and every other line that
// arrived before it.
func (c *liveCtl) reply(t *testing.T) (controlmode.Line, []controlmode.Line) {
	t.Helper()
	var side []controlmode.Line
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, more := <-c.lines:
			if !more {
				t.Fatal("control client closed its stream before a reply arrived")
			}
			if l.Kind == controlmode.End || l.Kind == controlmode.Error {
				return l, side
			}
			side = append(side, l)
		case <-deadline:
			t.Fatal("timed out waiting for a command reply")
		}
	}
}

func (c *liveCtl) do(t *testing.T, cmd string) (controlmode.Line, []controlmode.Line) {
	t.Helper()
	if _, err := fmt.Fprintln(c.stdin, cmd); err != nil {
		t.Fatalf("send %q: %v", cmd, err)
	}
	return c.reply(t)
}

// forgedLines picks out the lines a value must never be able to put on the
// stream: a connection exit, or a subscription report.
func forgedLines(lines []controlmode.Line) []controlmode.Line {
	var out []controlmode.Line
	for _, l := range lines {
		if l.Kind == controlmode.Exit || l.Kind == controlmode.SubscriptionChanged {
			out = append(out, l)
		}
	}
	return out
}

// ctlSafe wraps the whole format in the control-byte-to-space modifier (#860).
func TestCtlSafeWrapsWholeFormat(t *testing.T) {
	want := "#{s/[[#{l::}cntrl#{l::}]]/ /:#{@x}}"
	if got := ctlSafe("#{@x}"); got != want {
		t.Errorf("ctlSafe = %q, want %q", got, want)
	}
}

// Every remote-derived read the daemon issues carries its format wrapped by
// ctlSafe (#860).
func TestRemoteFormatsAreCtlSafe(t *testing.T) {
	// No wrapped format contains a quote, so the literal survives tmuxQuote and
	// the single-quoted name:what:format subscribe token.
	t.Run("subscribeFormats", func(t *testing.T) {
		var issued []string
		subscribeFormats(replyRT(&issued, body("")))
		formats := []string{windowLabelFormat, agentStatusFormat, sessionResFormat, agentUsageFormat}
		if len(issued) != len(formats) {
			t.Fatalf("issued %d commands, want %d: %q", len(issued), len(formats), issued)
		}
		for i, f := range formats {
			if want := wrapCtlSafe(f); !strings.Contains(issued[i], want) {
				t.Errorf("subscription %d = %q, want it to carry %q", i, issued[i], want)
			}
		}
	})

	t.Run("urlOpener.connect", func(t *testing.T) {
		p := &openerProbe{}
		o := p.opener("$1")
		var issued []string
		o.connect(scriptRT(&issued, okReply(body("")), okReply(body("")), okReply(body(""))))
		if len(issued) < 2 {
			t.Fatalf("issued %q, want a seed and a subscribe", issued)
		}
		want := wrapCtlSafe(openURLFormat)
		for i, what := range []string{"seed display-message", "og_open subscribe"} {
			if !strings.Contains(issued[i], want) {
				t.Errorf("%s = %q, want it to carry %q", what, issued[i], want)
			}
		}
	})

	t.Run("labelShipper.flush", func(t *testing.T) {
		s := newLabelShipper()
		reg := newRegistry()
		var issued []string
		s.flush(Config{RemoteSession: "sess"}, reg, replyRT(&issued, body("")), reg.gen(), true)
		if len(issued) != 1 || !strings.Contains(issued[0], "list-windows") {
			t.Fatalf("issued %q, want one list-windows poll", issued)
		}
		if want := wrapCtlSafe(windowLabelFormat); !strings.Contains(issued[0], want) {
			t.Errorf("poll = %q, want it to carry %q", issued[0], want)
		}
	})

	t.Run("agentShipper.flush", func(t *testing.T) {
		a := newAgentShipper("lab-mono", 0)
		a.dir = privateDir(t)
		var issued []string
		a.flush(Config{RemoteSession: "sess"}, replyRT(&issued, body("")), 0, true)
		if len(issued) != 1 || !strings.Contains(issued[0], "list-panes") {
			t.Fatalf("issued %q, want one list-panes poll", issued)
		}
		if want := wrapCtlSafe(agentStatusFormat); !strings.Contains(issued[0], want) {
			t.Errorf("poll = %q, want it to carry %q", issued[0], want)
		}
	})

	t.Run("readSessionPath", func(t *testing.T) {
		var issued []string
		readSessionPath(replyRT(&issued, body("/srv/proj")), "sess")
		if len(issued) != 1 {
			t.Fatalf("issued %q, want one display-message", issued)
		}
		if want := wrapCtlSafe("#{session_path}"); !strings.Contains(issued[0], want) {
			t.Errorf("read = %q, want it to carry %q", issued[0], want)
		}
	})
}

// A wrapped format turns every control byte of an option value, newline
// included, into a space on a real tmux, where a raw one forges stream lines
// (#860).
func TestCtlSafeAgainstLiveTmux(t *testing.T) {
	requireLiveTmux(t)
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))
	tmuxMust(t, tmux, "set-option", "-t", "w", "@og860", forgedValue)
	spaced := strings.ReplaceAll(forgedValue, "\n", " ")

	t.Run("wrapped read is one line", func(t *testing.T) {
		c := attachLiveCtl(t, tmux, "w")
		l, side := c.do(t, "display-message -p -t w "+tmuxQuote(ctlSafe("#{@og860}")))
		if l.Kind != controlmode.End {
			t.Fatalf("reply kind = %v, want End: %s", l.Kind, l.Data)
		}
		if got := string(l.Data); got != spaced {
			t.Errorf("Data = %q, want %q", got, spaced)
		}
		if f := forgedLines(side); len(f) != 0 {
			t.Errorf("a wrapped read put forged lines on the stream: %+v", f)
		}
	})

	t.Run("raw read stays one reply body", func(t *testing.T) {
		c := attachLiveCtl(t, tmux, "w")
		l, side := c.do(t, "display-message -p -t w '#{@og860}'")
		if f := forgedLines(side); len(f) != 0 {
			t.Errorf("a raw read leaked %d forged lines before its reply: %+v", len(f), f)
		}
		if l.Kind != controlmode.End {
			t.Fatalf("reply kind = %v, want End: %s", l.Kind, l.Data)
		}
		for line := range strings.SplitSeq(forgedValue, "\n") {
			if !strings.Contains(string(l.Data), line) {
				t.Errorf("Data = %q, want it to hold the line %q as body", l.Data, line)
			}
		}
	})

	t.Run("wrapped subscription reports one line", func(t *testing.T) {
		c := attachLiveCtl(t, tmux, "w")
		l, side := c.do(t, subscribeCmd("og860t", "", ctlSafe("#{@og860}")))
		if l.Kind != controlmode.End {
			t.Fatalf("subscribe reply kind = %v, want End: %s", l.Kind, l.Data)
		}
		var seen []controlmode.Line
		seen = append(seen, side...)
		deadline := time.After(5 * time.Second)
		found := false
		for !found {
			for _, s := range seen {
				if _, ok := subscriptionValue(s, "og860t"); ok {
					found = true
				}
			}
			if found {
				break
			}
			select {
			case ln, more := <-c.lines:
				if !more {
					t.Fatal("control client closed before the subscription reported")
				}
				seen = append(seen, ln)
			case <-deadline:
				t.Fatal("timed out waiting for the og860t report")
			}
		}
		// The rest of a forged report arrives in the same write as the first
		// line; give it a moment to show up.
		grace := time.After(500 * time.Millisecond)
	drain:
		for {
			select {
			case ln, more := <-c.lines:
				if !more {
					break drain
				}
				seen = append(seen, ln)
			case <-grace:
				break drain
			}
		}
		for _, s := range seen {
			switch s.Kind {
			case controlmode.Exit:
				t.Errorf("the report put an Exit on the stream: %+v", s)
			case controlmode.SubscriptionChanged:
				v, ok := subscriptionValue(s, "og860t")
				if !ok {
					t.Errorf("a foreign subscription arrived: %+v", s)
					continue
				}
				if v != spaced {
					t.Errorf("og860t value = %q, want %q", v, spaced)
				}
			default:
			}
		}
	})
}

// The production command shapes read the same on a real tmux whether wrapped or
// not, for state that holds no control bytes (#860).
func TestCtlSafeRealFormatsAgainstLiveTmux(t *testing.T) {
	requireLiveTmux(t)
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))

	dir := t.TempDir()
	tmuxMust(t, tmux, "new-session", "-d", "-s", "rs", "-c", dir, "-x", "80", "-y", "24")
	tmuxMust(t, tmux, "new-window", "-d", "-t", "rs:")
	wins := strings.Fields(tmuxMust(t, tmux, "list-windows", "-t", "rs", "-F", "#{window_id}"))
	if len(wins) != 2 {
		t.Fatalf("windows = %q, want two", wins)
	}
	tmuxMust(t, tmux, "split-window", "-d", "-t", wins[1])
	panes := strings.Fields(tmuxMust(t, tmux, "list-panes", "-s", "-t", "rs", "-F", "#{pane_id}"))
	if len(panes) != 3 {
		t.Fatalf("panes = %q, want three", panes)
	}
	statusPane := strings.TrimSpace(tmuxMust(t, tmux, "list-panes", "-t", wins[0], "-F", "#{pane_id}"))

	for _, o := range [][]string{
		{"-w", "-t", wins[0], "@crew_name", "nova"},
		{"-w", "-t", wins[0], "@crew_color", "#89b4fa"},
		{"-w", "-t", wins[0], "@pr_number", "42"},
		{"-w", "-t", wins[0], "@pr_state", "open"},
		{"-w", "-t", wins[0], "@window_pr_plain", " #42"},
		{"-w", "-t", wins[1], "@crew_name", "orbit"},
		{"-p", "-t", statusPane, "@claude_status", "processing 1700000000 0"},
		{"-t", "rs", "@og_session_res", "12.5 340 8 1700000000 claude,pi"},
		{"-t", "rs", "@og_agent_usage", `{"claude":{"used":1}}`},
		{"-t", "rs", openURLOpt, " 1-1|https://a"},
	} {
		tmuxMust(t, tmux, append([]string{"set-option"}, o...)...)
	}

	c := attachLiveCtl(t, tmux, "rs")
	// same runs cmdPrefix against f raw and wrapped, requires both to answer
	// alike, and returns the shared reply body.
	same := func(name, cmdPrefix, f string) string {
		t.Helper()
		raw, _ := c.do(t, cmdPrefix+tmuxQuote(f))
		wrapped, _ := c.do(t, cmdPrefix+tmuxQuote(wrapCtlSafe(f)))
		if raw.Kind != controlmode.End || wrapped.Kind != controlmode.End {
			t.Fatalf("%s: raw kind %v (%s), wrapped kind %v (%s)", name, raw.Kind, raw.Data, wrapped.Kind, wrapped.Data)
		}
		if string(raw.Data) != string(wrapped.Data) {
			t.Errorf("%s: wrapped = %q, raw = %q", name, wrapped.Data, raw.Data)
		}
		return string(raw.Data)
	}

	labels := parseWindowLabels(same("list-windows", "list-windows -t rs -F ", windowLabelFormat))
	wantLabels := []labelRow{
		{id: wins[0], crewName: "nova", crewColor: "#89b4fa", prNumber: "42", prState: "open", prPlain: " #42"},
		{id: wins[1], crewName: "orbit"},
	}
	if !reflect.DeepEqual(labels, wantLabels) {
		t.Errorf("window labels = %+v, want %+v", labels, wantLabels)
	}

	rows := parseAgentStatus(same("list-panes", "list-panes -s -t rs -F ", agentStatusFormat))
	if len(rows) != len(panes) {
		t.Errorf("agent rows = %+v, want one per pane %q", rows, panes)
	}
	var stamped *paneStatus
	for i := range rows {
		if rows[i].pane == statusPane {
			stamped = &rows[i]
		}
	}
	if stamped == nil || stamped.state != "processing" || stamped.ts != 1700000000 || stamped.proc == "" {
		t.Errorf("stamped pane row = %+v, want state processing at 1700000000 with a command", stamped)
	}

	res := same("session res", "display-message -p -t rs ", sessionResFormat)
	if !sessionResRe.MatchString(res) {
		t.Errorf("session res = %q, want a value sessionResRe accepts", res)
	}
	if usage := same("agent usage", "display-message -p -t rs ", agentUsageFormat); !strings.Contains(usage, `{"claude":{"used":1}}`) {
		t.Errorf("agent usage = %q, want the stamped JSON", usage)
	}
	if got := same("open url", "display-message -p -t rs ", openURLFormat); got != " 1-1|https://a" {
		t.Errorf("open url = %q, want the stamped log", got)
	}
	path := same("session path", "display-message -p -t rs -F ", "#{session_path}")
	if !sessionPathRe.MatchString(path) {
		t.Errorf("session path = %q, want an absolute path", path)
	}

	// The subscription must report what a read does.
	l, side := c.do(t, subscribeCmd(resSubName, "", wrapCtlSafe(sessionResFormat)))
	if l.Kind != controlmode.End {
		t.Fatalf("subscribe reply kind = %v, want End: %s", l.Kind, l.Data)
	}
	pending := side
	deadline := time.After(5 * time.Second)
	for {
		for _, s := range pending {
			if v, ok := subscriptionValue(s, resSubName); ok {
				if v != res {
					t.Errorf("subscription reported %q, want the read's %q", v, res)
				}
				return
			}
		}
		pending = nil
		select {
		case ln, more := <-c.lines:
			if !more {
				t.Fatal("control client closed before the subscription reported")
			}
			pending = append(pending, ln)
		case <-deadline:
			t.Fatal("timed out waiting for the og_res report")
		}
	}
}

// A reply body that spells out framing lines must not close its own block, take
// an ordinal, or surface as a notification (#860).
func TestForgedReplyBodyKeepsOrdinals(t *testing.T) {
	stream := strings.Join([]string{
		"%begin 1 1 1",
		"one",
		"%end 1 1 1",
		"%begin 1 2 1",
		"body",
		"%end 1 9 1",
		"%exit",
		"%subscription-changed og_open $0 - - - : 9-9|https://evil/",
		"%end 1 2 1",
		"%begin 1 3 1",
		"three",
		"%end 1 3 1",
		"",
	}, "\n")
	rd := rawTestReader(stream)
	st := newStream(io.Discard)

	var ordinals []uint64
	for {
		l, ok := rd.Next()
		if !ok {
			break
		}
		if l.Kind == controlmode.Exit || l.Kind == controlmode.SubscriptionChanged {
			t.Errorf("a reply body surfaced as a notification: %+v", l)
		}
		if seq := claimSeq(l, st); seq != 0 {
			ordinals = append(ordinals, seq)
		}
	}
	if want := []uint64{1, 2, 3}; !reflect.DeepEqual(ordinals, want) {
		t.Errorf("ordinals = %v, want %v", ordinals, want)
	}
}
