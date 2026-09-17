package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// fakeCapture replays stdout (with %s substituted by the marker read out of
// argv) plus an optional error, and records how it was called.
type fakeCapture struct {
	stdout string
	err    error
	calls  int
	argv   []string
	marker string
}

func (f *fakeCapture) run(args ...string) ([]byte, error) {
	f.calls++
	f.argv = args
	f.marker = args[len(args)-1]
	return []byte(strings.ReplaceAll(f.stdout, "%M", f.marker)), f.err
}

func TestCaptureTargetsBatch(t *testing.T) {
	f := &fakeCapture{stdout: "alpha row\n%M\nbeta row\n%M\n"}
	got, err := captureTargets([]string{"%1", "%2"}, f.run)
	if err != nil {
		t.Fatalf("captureTargets: %v", err)
	}
	if f.calls != 1 {
		t.Errorf("runner called %d times, want 1", f.calls)
	}
	want := map[string]string{
		"%1": "alpha row" + captureBGReset,
		"%2": "beta row" + captureBGReset,
	}
	for target, w := range want {
		if got[target] != w {
			t.Errorf("content[%s] = %q, want %q", target, got[target], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d (%+v)", len(got), len(want), got)
	}

	seps := 0
	for _, a := range f.argv {
		if a == ";" {
			seps++
		}
	}
	if seps != 3 {
		t.Errorf("argv has %d %q separators, want 3: %q", seps, ";", f.argv)
	}
	wantMarker := fmt.Sprintf("@@og-wall-%d-", os.Getpid())
	if !strings.HasPrefix(f.marker, wantMarker) {
		t.Errorf("marker %q, want prefix %q", f.marker, wantMarker)
	}
	markers := 0
	for _, a := range f.argv {
		if a == f.marker {
			markers++
		}
	}
	if markers != 2 {
		t.Errorf("marker appears %d times in argv, want 2: %q", markers, f.argv)
	}
}

func TestCaptureTargetsContent(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		stdout  string
		want    map[string]string
	}{
		{
			name:    "empty capture keeps the key",
			targets: []string{"%1", "%2"},
			stdout:  "%M\nbeta\n%M\n",
			want: map[string]string{
				"%1": "",
				"%2": "beta" + captureBGReset,
			},
		},
		{
			name:    "marker lookalikes stay content",
			targets: []string{"%1"},
			stdout:  "@@og-wall- is not a marker\nlog: %M trailing\n%M\n",
			want: map[string]string{
				"%1": "@@og-wall- is not a marker" + captureBGReset + "\n" +
					"log: %M trailing" + captureBGReset,
			},
		},
		{
			name:    "trailing blank rows trimmed",
			targets: []string{"%1"},
			stdout:  "top\n\n   \n" + captureBGReset + "    \n%M\n",
			want: map[string]string{
				"%1": "top" + captureBGReset,
			},
		},
		{
			name:    "duplicate target captured once",
			targets: []string{"%1", "%1"},
			stdout:  "solo\n%M\n",
			want: map[string]string{
				"%1": "solo" + captureBGReset,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeCapture{stdout: c.stdout}
			got, err := captureTargets(c.targets, f.run)
			if err != nil {
				t.Fatalf("captureTargets: %v", err)
			}
			if len(got) != len(c.want) {
				t.Errorf("got %d keys, want %d (%+v)", len(got), len(c.want), got)
			}
			for target, w := range c.want {
				w = strings.ReplaceAll(w, "%M", f.marker)
				if got[target] != w {
					t.Errorf("content[%s] = %q, want %q", target, got[target], w)
				}
			}
		})
	}
}

func TestCaptureTargetsAbort(t *testing.T) {
	cases := []struct {
		name       string
		targets    []string
		stdout     string
		wantKeys   []string
		wantTarget string
	}{
		{
			name:       "bad target mid-batch",
			targets:    []string{"%1", "%2", "%3"},
			stdout:     "alpha\n%M\n",
			wantKeys:   []string{"%1"},
			wantTarget: "%2",
		},
		{
			name:       "bad target first",
			targets:    []string{"%9", "%1"},
			stdout:     "",
			wantKeys:   nil,
			wantTarget: "%9",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Only tmux's own "can't find …" is attributable; see captureTargetGone.
			exit1 := exitErrWithStderr(t, "can't find pane: "+c.wantTarget+"\n")
			f := &fakeCapture{stdout: c.stdout, err: exit1}
			got, err := captureTargets(c.targets, f.run)
			var cErr *captureErr
			if !errors.As(err, &cErr) {
				t.Fatalf("err = %v, want *captureErr", err)
			}
			if cErr.Target != c.wantTarget {
				t.Errorf("Target = %q, want %q", cErr.Target, c.wantTarget)
			}
			if !errors.Is(err, exit1) {
				t.Errorf("err does not unwrap to the runner error: %v", err)
			}
			if len(got) != len(c.wantKeys) {
				t.Errorf("got %d keys, want %d (%+v)", len(got), len(c.wantKeys), got)
			}
			for _, k := range c.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("content missing key %s (%+v)", k, got)
				}
			}
		})
	}
}

// OSC dies at the capture boundary; SGR, which is what -e is for, survives.
func TestCaptureTargetsStripsStringEscapes(t *testing.T) {
	link := "link: \033]8;;http://evil\033\\CLICK\033]8;;\033\\ end"
	f := &fakeCapture{stdout: "\033[31mred\033[0m " + link + "\n%M\n"}
	got, err := captureTargets([]string{"%1"}, f.run)
	if err != nil {
		t.Fatalf("captureTargets: %v", err)
	}
	content := got["%1"]
	if strings.Contains(content, "\033]") {
		t.Errorf("an OSC sequence reached the wall: %q", content)
	}
	if !strings.Contains(content, "link: CLICK end") {
		t.Errorf("visible text did not survive the strip: %q", content)
	}
	// SGR is the whole point of capture-pane -e; the strip must not touch it.
	if !strings.Contains(content, "\033[31m") {
		t.Errorf("SGR color did not survive the strip: %q", content)
	}
}

func TestStripStringEscapes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"osc 8 hyperlink", "a\033]8;;http://evil\033\\b\033]8;;\033\\c", "abc"},
		{"osc terminated by bel", "a\033]0;title\ab", "ab"},
		{"dcs", "a\033Pq#0;2;0;0;0\033\\b", "ab"},
		{"csi sgr survives", "\033[1;31mred\033[0m", "\033[1;31mred\033[0m"},
		// An unterminated string must not swallow the rows after it: the whole
		// capture goes through this, and a run of ESC ] with no ST is one pane's
		// row, not the pane below's.
		{"unterminated stops at the newline", "a\033]8;;evil\nnext row", "a\nnext row"},
		{"unterminated at end of string", "a\033]8;;evil", "a"},
		{"nothing to strip is returned as is", "plain \033[32mtext\033[0m", "plain \033[32mtext\033[0m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripStringEscapes(c.in); got != c.want {
				t.Errorf("stripStringEscapes(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A truncated OSC would leave hyperlink mode open past the tile it belongs to.
// The strip is what makes that unreachable — the crop itself still only knows CSI.
func TestCapturedOSCSurvivesNarrowCrop(t *testing.T) {
	payload := strings.Repeat("x", 200)
	f := &fakeCapture{stdout: "\033]8;;http://evil/" + payload + "\033\\text\n%M\n"}
	got, err := captureTargets([]string{"%1"}, f.run)
	if err != nil {
		t.Fatalf("captureTargets: %v", err)
	}
	cropped := fitVisibleWidth(got["%1"], 12)
	if strings.Contains(cropped, "\033]") {
		t.Errorf("crop emitted an unterminated OSC: %q", cropped)
	}
	if !strings.Contains(cropped, "text") {
		t.Errorf("the row's real content was crowded out by the OSC payload: %q", cropped)
	}
}

// Blaming a target for a failure that isn't its fault freezes a healthy tile at
// (gone) for the rest of the session — wallPageTargets never retries one.
func TestCaptureTargetsAttributesOnlyGoneTargets(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		wantTarget string // "" = nothing may be blamed
	}{
		{"missing session", "can't find session: nosuch\n", "%2"},
		{"missing window", "can't find window: 0\n", "%2"},
		{"missing pane", "can't find pane: %99\n", "%2"},
		{"server trouble", "server exited unexpectedly\n", ""},
		{"no stderr at all", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeCapture{stdout: "alpha\n%M\n", err: exitErrWithStderr(t, c.stderr)}
			got, err := captureTargets([]string{"%1", "%2", "%3"}, f.run)
			if err == nil {
				t.Fatal("want an error")
			}
			// The good tile refreshes either way.
			if _, ok := got["%1"]; !ok {
				t.Errorf("content missing the captured target (%+v)", got)
			}
			target := ""
			var cErr *captureErr
			if errors.As(err, &cErr) {
				target = cErr.Target
			}
			if target != c.wantTarget {
				t.Errorf("attributed %q, want %q (err: %v)", target, c.wantTarget, err)
			}
		})
	}
}

// exitErrWithStderr builds the error shape exec.Output() returns: an ExitError
// carrying the command's stderr, which is the only place tmux's message lands.
func exitErrWithStderr(t *testing.T, stderr string) error {
	t.Helper()
	_, err := exec.Command("false").Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError from `false`, got %v", err)
	}
	exitErr.Stderr = []byte(stderr)
	return exitErr
}

func TestCaptureTargetsNoTargets(t *testing.T) {
	f := &fakeCapture{stdout: "unused"}
	got, err := captureTargets(nil, f.run)
	if err != nil {
		t.Fatalf("captureTargets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty map", got)
	}
	if f.calls != 0 {
		t.Errorf("runner called %d times, want 0", f.calls)
	}
}

func TestRelayKeyArgs(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want []string
		ok   bool
	}{
		{"lowercase letter", "a", []string{"-l", "--", "a"}, true},
		{"digit", "5", []string{"-l", "--", "5"}, true},
		{"space", " ", []string{"-l", "--", " "}, true},
		// Relaying the name verbatim would type the word "space" (#689).
		{"space by key name", "space", []string{"-l", "--", " "}, true},
		{"punctuation", ";", []string{"-l", "--", ";"}, true},
		{"enter", "enter", []string{"Enter"}, true},
		{"escape", "esc", []string{"Escape"}, true},
		{"backspace", "backspace", []string{"BSpace"}, true},
		{"left arrow", "left", nil, false},
		{"up arrow", "up", nil, false},
		{"ctrl+c", "ctrl+c", nil, false},
		{"ctrl+a", "ctrl+a", nil, false},
		{"function key", "f1", nil, false},
		{"tab", "tab", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, ok := relayKeyArgs(c.key)
			if ok != c.ok {
				t.Fatalf("relayKeyArgs(%q) ok = %v, want %v", c.key, ok, c.ok)
			}
			if ok && !slices.Equal(args, c.want) {
				t.Errorf("relayKeyArgs(%q) = %v, want %v", c.key, args, c.want)
			}
		})
	}
}

func TestSendKeys(t *testing.T) {
	f := &fakeCapture{}
	if err := sendKeys("%3", []string{"Enter"}, f.run); err != nil {
		t.Fatalf("sendKeys: %v", err)
	}
	want := []string{"send-keys", "-t", "%3", "Enter"}
	if !slices.Equal(f.argv, want) {
		t.Errorf("argv = %v, want %v", f.argv, want)
	}

	f = &fakeCapture{}
	if err := sendKeys("%3", []string{"-l", "--", "q"}, f.run); err != nil {
		t.Fatalf("sendKeys: %v", err)
	}
	want = []string{"send-keys", "-t", "%3", "-l", "--", "q"}
	if !slices.Equal(f.argv, want) {
		t.Errorf("argv = %v, want %v", f.argv, want)
	}
}

func TestSendKeysPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	f := &fakeCapture{err: wantErr}
	if err := sendKeys("%3", []string{"Enter"}, f.run); !errors.Is(err, wantErr) {
		t.Errorf("sendKeys err = %v, want %v", err, wantErr)
	}
}
