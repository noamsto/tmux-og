package remoteopen

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineSanitizerSplitWrites(t *testing.T) {
	var dst bytes.Buffer
	w := &lineSanitizer{dst: &dst}
	for _, chunk := range []string{"a\x1b[3", "1mb\n", "c\x1b]0;t"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if got := dst.String(); got != "ab\nc" {
		t.Errorf("got %q, want %q", got, "ab\nc")
	}
}

func TestSafeText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "héllo wörld", "héllo wörld"},
		{"newline tab kept", "a\n\tb", "a\n\tb"},
		{"csi", "a\x1b[31mb\x1b[0m", "ab"},
		{"osc bel", "a\x1b]0;title\x07b", "ab"},
		{"osc st", "a\x1b]0;title\x1b\\b", "ab"},
		{"dcs", "a\x1bPq#0;2\x1b\\b", "ab"},
		{"esc other", "a\x1bcb", "ab"},
		{"c1 rune", "a\u009bb", "ab"},
		{"raw 0x9b", "a\x9bb", "ab"},
		{"del and cr", "a\x7fb\rc", "abc"},
		{"bidi", "a\u202eb\u2066c", "abc"},
		{"trailing esc", "a\x1b", "a"},
	}
	for _, tc := range cases {
		if got := safeText(tc.in); got != tc.want {
			t.Errorf("%s: safeText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestLauncherTmpdirRefusalSanitized(t *testing.T) {
	h := newHarness(t)
	evil := "/x\x1b[31m\x1b]0;x\x07\u009b"
	h.answer = func(c cmdSpec, _ string) (string, int, bool) {
		if c.Name == "ssh" && c.Stdin != nil && strings.Contains(*c.Stdin, ": og-probe;") {
			return strings.Replace(probeOut, "/run/user/1000", evil, 1), 0, true
		}
		return "", 0, false
	}
	if code := h.run("h", "work"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	got := h.stderr.String()
	if want := "og-remote-open: unusable remote tmpdir: /x\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
}

func TestLauncherDaemonLogSanitized(t *testing.T) {
	h := newHarness(t)
	h.onLaunch = func(sock string) *daemonProc {
		writeFile(t, sock+".log", "one\x1b[31m\ntwo\x1b]0;x\x07\u009b\nthree\n")
		d := &daemonProc{Pid: 777, done: make(chan struct{})}
		close(d.done)
		return d
	}
	if code := h.run("h", "work", "--into", "dispatch"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	want := "og-remote-open: the bridge daemon for h:work exited before listening:\none\ntwo\nthree\n"
	if got := h.stderr.String(); got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
}
