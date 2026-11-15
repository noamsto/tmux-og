package remoteopen

import (
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testdata/quote.tsv: one row per line, three TAB-separated fields — the
// lowercase hex of the input bytes (empty for empty input), bash's
// `printf %q` output under LC_ALL=C, and a "." sentinel that is ignored.
func TestBashQuote(t *testing.T) {
	data, err := os.ReadFile("testdata/quote.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("bad row %q", line)
		}
		in, err := hex.DecodeString(fields[0])
		if err != nil {
			t.Fatal(err)
		}
		rows++
		if got := bashQuote(string(in)); got != fields[1] {
			t.Errorf("bashQuote(%q) = %q, want %q", in, got, fields[1])
		}
	}
	if rows != 207 {
		t.Errorf("read %d rows, want 207", rows)
	}
}

func TestShellQuote(t *testing.T) {
	// A backslash passes through unchanged, never doubled: backslash-bearing
	// values are refused before they reach shellQuote.
	for in, want := range map[string]string{"it's": `'it'\''s'`, "": "''", "a b": "'a b'", `a\b`: `'a\b'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
	// Round-tripped under sh only: the nix sandbox has no fish.
	in := "it's/a path"
	out, err := exec.Command("/bin/sh", "-c", "printf '%s' "+shellQuote(in)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("sh round-trip = %q, want %q", out, in)
	}
}

func TestShellQuotable(t *testing.T) {
	for in, want := range map[string]bool{`a\b`: false, "/srv/my project (v2)": true, "it's": true, "": true} {
		if got := shellQuotable(in); got != want {
			t.Errorf("shellQuotable(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidRemotePath(t *testing.T) {
	for in, want := range map[string]bool{
		"/run/user/1000": true, "/tmp/tmux-1000": true, "/": true, "/a@b+c:d._-": true,
		"bin/x": false, "/run/user/1000 x": false, "/run/user/$(id -u)": false, "": false, "/a\nb": false,
	} {
		if got := validRemotePath(in); got != want {
			t.Errorf("validRemotePath(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRequireSessionName(t *testing.T) {
	for _, in := range []string{"a\nb", "a\x01b", "a\x7fb"} {
		want := "og-remote-open: session name contains a control character, which no tmux session name can: " + bashQuote(in) + " — pass an explicit session name instead"
		err := requireSessionName(in)
		var usageErr *UsageError
		if !errors.As(err, &usageErr) || usageErr.Error() != want {
			t.Errorf("requireSessionName(%q) = %v, want %q", in, err, want)
		}
	}
	want := `og-remote-open: session name contains a backslash, which no remote shell dialect can quote safely: a\b — pass an explicit session name instead`
	if err := requireSessionName(`a\b`); err == nil || err.Error() != want {
		t.Errorf("backslash: got %v", err)
	}
	for _, in := range []string{"work", ""} {
		if err := requireSessionName(in); err != nil {
			t.Errorf("requireSessionName(%q) = %v", in, err)
		}
	}
}

func TestPosixDirname(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/tmux": "/usr/bin", "/usr/bin/": "/usr", "tmux": ".", "/": "/", "//": "/",
		"/tmux": "/", "": ".", "a/": ".", "/a/b//": "/a", "a//b": "a",
	} {
		if got := posixDirname(in); got != want {
			t.Errorf("posixDirname(%q) = %q, want %q", in, got, want)
		}
	}
}
