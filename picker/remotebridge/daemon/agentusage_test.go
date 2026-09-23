package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func usd(v float64) *float64 { return &v }

func decodeUsage(t *testing.T, out string) map[string]usageCache {
	t.Helper()
	if out == "" {
		return nil
	}
	var m map[string]usageCache
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output %q is not a usage map: %v", out, err)
	}
	return m
}

func TestSanitizeUsage(t *testing.T) {
	const codex = `"codex":{"windows":[{"label":"week","pct":3}]}`
	codexWant := usageCache{Windows: []usageWindow{{Label: "week", Pct: 3}}}
	// claudeWith builds a report where claude carries the given cache and a
	// valid codex sits beside it, both open — so a case that drops claude
	// proves it dropped only claude.
	claudeWith := func(cache string) string {
		return "claude codex |{\"claude\":" + cache + "," + codex + "}"
	}
	nineWindows := strings.TrimSuffix(strings.Repeat(`{"label":"w","pct":1},`, 9), ",")

	tests := []struct {
		name string
		v    string
		skew int64
		want map[string]usageCache
		// absent is text the output must not carry.
		absent []string
	}{
		{
			name: "open agents are kept",
			v:    claudeWith(`{"windows":[{"label":"5h","pct":42,"reset_at":1700000000}],"monthly":{"label":"mo","pct":10}}`),
			want: map[string]usageCache{
				"claude": {Windows: []usageWindow{{Label: "5h", Pct: 42, ResetAt: 1700000000}}, Monthly: &usageWindow{Label: "mo", Pct: 10}},
				"codex":  codexWant,
			},
		},
		{
			// The remote poller deletes a closed agent's cache only on its next
			// pass; the live gate is what hides it within a second.
			name: "a cached agent with no open pane is gated out",
			v:    `claude |{"claude":{"windows":[{"label":"5h","pct":1}]},` + codex + `}`,
			want: map[string]usageCache{"claude": {Windows: []usageWindow{{Label: "5h", Pct: 1}}}},
		},
		{
			name: "wrapped, store-path and cursor-agent commands normalise",
			v: `.claude-wrapped /nix/store/abc-pi/bin/pi cursor-agent |{` +
				`"claude":{"windows":[{"label":"5h","pct":1}]},` +
				`"pi":{"windows":[],"spend":{"usd":2.5,"limit_usd":10}},` +
				`"cursor":{"windows":[{"label":"mo","pct":9}]}}`,
			want: map[string]usageCache{
				"claude": {Windows: []usageWindow{{Label: "5h", Pct: 1}}},
				"pi":     {Windows: []usageWindow{}, Spend: &usageSpend{USD: 2.5, LimitUSD: usd(10)}},
				"cursor": {Windows: []usageWindow{{Label: "mo", Pct: 9}}},
			},
		},
		{name: "a #() label drops its agent", v: claudeWith(`{"windows":[{"label":"#(touch /tmp/x)","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}, absent: []string{"#", "touch"}},
		{name: "a #{} label drops its agent", v: claudeWith(`{"windows":[{"label":"#{pane_id}","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}, absent: []string{"#", "pane_id"}},
		{name: "a #[] label drops its agent", v: claudeWith(`{"windows":[{"label":"#[fg=red]","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}, absent: []string{"#", "fg=red"}},
		{name: "a piped label drops its agent", v: claudeWith(`{"windows":[{"label":"a|b","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}, absent: []string{"|"}},
		{name: "a monthly #() label drops its agent", v: claudeWith(`{"windows":[],"monthly":{"label":"#(id)","pct":1}}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "a 13-char label drops its agent", v: claudeWith(`{"windows":[{"label":"abcdefghijklm","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "an empty label drops its agent", v: claudeWith(`{"windows":[{"label":"","pct":1}]}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "nine windows drop the agent", v: claudeWith(`{"windows":[` + nineWindows + `]}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "pct over 1000 drops the agent", v: claudeWith(`{"windows":[{"label":"5h","pct":1001}]}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "negative usd drops the agent", v: claudeWith(`{"windows":[],"spend":{"usd":-1}}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "an over-large limit drops the agent", v: claudeWith(`{"windows":[],"spend":{"usd":1,"limit_usd":1e8}}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "a negative reset_at drops the agent", v: claudeWith(`{"windows":[{"label":"5h","pct":1,"reset_at":-5}]}`), want: map[string]usageCache{"codex": codexWant}},
		{name: "a mistyped field drops the agent", v: claudeWith(`{"windows":[{"label":"5h","pct":"7"}]}`), want: map[string]usageCache{"codex": codexWant}},
		{
			name:   "an unknown agent key is dropped",
			v:      `claude gemini |{"gemini":{"windows":[{"label":"5h","pct":1}]},"claude":{"windows":[{"label":"5h","pct":2}]}}`,
			want:   map[string]usageCache{"claude": {Windows: []usageWindow{{Label: "5h", Pct: 2}}}},
			absent: []string{"gemini"},
		},
		{
			// A provider adding a field later must not drop the agent.
			name:   "an unknown field keeps the agent and is not carried",
			v:      `claude |{"claude":{"extra":"#(id)","windows":[{"label":"5h","pct":2,"extra2":1}]}}`,
			want:   map[string]usageCache{"claude": {Windows: []usageWindow{{Label: "5h", Pct: 2}}}},
			absent: []string{"extra", "#"},
		},
		{
			name:   "spend label and period are not carried",
			v:      `pi |{"pi":{"windows":[],"spend":{"label":"spendlbl","usd":3,"period":"month"}}}`,
			want:   map[string]usageCache{"pi": {Windows: []usageWindow{}, Spend: &usageSpend{USD: 3}}},
			absent: []string{"spendlbl", "period", "month"},
		},
		{
			name: "skew shifts reset_at and leaves 0 alone",
			v:    `claude |{"claude":{"windows":[{"label":"5h","pct":1,"reset_at":1000},{"label":"wk","pct":2}],"monthly":{"label":"mo","pct":3,"reset_at":2000}}}`,
			skew: 50,
			want: map[string]usageCache{"claude": {
				Windows: []usageWindow{{Label: "5h", Pct: 1, ResetAt: 1050}, {Label: "wk", Pct: 2}},
				Monthly: &usageWindow{Label: "mo", Pct: 3, ResetAt: 2050},
			}},
		},
		{name: "over the cap is empty", v: "claude |{\"claude\":{\"windows\":[]},\"x\":\"" + strings.Repeat("a", usageRawMaxLen) + "\"}"},
		{name: "no separator is empty", v: `{"claude":{"windows":[{"label":"5h","pct":1}]}}`},
		{name: "bad JSON is empty", v: `claude |{"claude":`},
		{name: "a not-rebuilt remote is empty", v: "claude |"},
		{name: "nothing open is empty", v: `|{"claude":{"windows":[{"label":"5h","pct":1}]}}`},
		{name: "every open agent invalid is empty", v: `claude |{"claude":{"windows":[{"label":"#(id)","pct":1}]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeUsage(tc.v, tc.skew)
			if got := decodeUsage(t, out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sanitizeUsage = %s\nwant %+v", out, tc.want)
			}
			// The value is read out of a '|'-delimited row and rendered into a
			// tmux format: neither character may survive any payload.
			if strings.ContainsAny(out, "|#") {
				t.Errorf("output carries '|' or '#': %s", out)
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Errorf("output carries %q: %s", a, out)
				}
			}
		})
	}
}

// TestUsageShipperFlush walks one shipper through a sequence: its rules are
// about what the previous report left behind.
func TestUsageShipperFlush(t *testing.T) {
	const skew = 100
	row := `claude |{"claude":{"windows":[{"label":"5h","pct":7,"reset_at":1000}]}}`
	stamped := sanitizeUsage(row, skew)
	if !strings.Contains(stamped, `"reset_at":1100`) {
		t.Fatalf("fixture must carry the shipper's skew: %s", stamped)
	}

	steps := []struct {
		name   string
		queue  string
		silent bool
		reset  bool
		// stamp is the value the pass must write; unset is the -u form. Neither
		// means it must write nothing.
		stamp string
		unset bool
	}{
		{
			// A previous daemon on the reused session may have left a value.
			name:  "a first-ever empty report still unsets",
			queue: "claude |",
			unset: true,
		},
		{name: "an identical empty report writes nothing", queue: "claude |"},
		{name: "the first data report writes", queue: row, stamp: stamped},
		{name: "an identical re-report writes nothing", queue: row},
		{name: "nothing pending writes nothing", silent: true},
		{name: "reset makes an identical report write again", queue: row, reset: true, stamp: stamped},
		{name: "empty JSON unsets", queue: "claude |{}", unset: true},
		{name: "data after an unset writes", queue: row, stamp: stamped},
		{
			// Unlike @bridge_res's keep-previous: a figure the daemon cannot
			// vouch for must not stand in for the remote's.
			name:  "a malformed report fails closed",
			queue: `claude |{"claude":`,
			unset: true,
		},
		{name: "and a valid one writes again", queue: row, stamp: stamped},
	}

	var calls [][]string
	cfg := Config{
		LocalSess: "mirror",
		LocalTmux: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
	}
	u := newUsageShipper(skew)
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.reset {
				u.reset()
			}
			if !s.silent {
				u.queue(s.queue)
			}
			calls = nil
			u.flush(cfg)

			var want [][]string
			switch {
			case s.unset:
				want = [][]string{{"set-option", "-u", "-t", "mirror", "@bridge_usage"}}
			case s.stamp != "":
				want = [][]string{{"set-option", "-t", "mirror", "@bridge_usage", s.stamp}}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("wrote %v, want %v", calls, want)
			}
		})
	}
}

// TestAgentUsageSubscriptionReportsOpenAgents is the live half of the
// sanitizer's contract: a spec tmux cannot parse is dropped silently, and this
// shipper has no poll backstop, so only a real server can say the format's
// open-pane loop and the session-scoped spelling both work.
func TestAgentUsageSubscriptionReportsOpenAgents(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// See TestSessionResSubscriptionIsSessionScoped.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	// A pane whose command is named claude: a copy of the real bash binary, not
	// a symlink, so the process name tmux reads is the copy's own.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if bash, err = filepath.EvalSymlinks(bash); err != nil {
		t.Fatalf("resolve bash: %v", err)
	}
	bin, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, bin, 0o755); err != nil {
		t.Fatal(err)
	}

	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())
	// A second session: the loop must walk every session on the server, not
	// only the control client's own.
	if out, err := tmux("new-session", "-d", "-s", "agents", claude, "-c", "sleep 600; :").CombinedOutput(); err != nil {
		t.Fatalf("new-session agents: %v\n%s", err, out)
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

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	if _, err := fmt.Fprintln(stdin, subscribeCmd(usageSubName, "", agentUsageFormat)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	prefix := "%subscription-changed " + usageSubName + " "
	waitForLine(t, lines, func(l string) bool { return strings.HasPrefix(l, prefix) })

	if out, err := tmux("set-option", "-g", "@og_agent_usage", `{"claude":{"windows":[{"label":"5h","pct":7}]}}`).CombinedOutput(); err != nil {
		t.Fatalf("set @og_agent_usage: %v\n%s", err, out)
	}

	got := waitForLine(t, lines, func(l string) bool {
		if !strings.HasPrefix(l, prefix) {
			return false
		}
		v, ok := subscriptionValue(controlmode.ParseLine(l), usageSubName)
		if !ok {
			return false
		}
		c, ok := decodeUsage(t, sanitizeUsage(v, 0))["claude"]
		return ok && len(c.Windows) == 1 && c.Windows[0].Pct == 7
	})
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `\$\d+ - - - : `).MatchString(got) {
		t.Errorf("line = %q, want a session-scoped '%s$N - - - : ' report", got, prefix)
	}
}
