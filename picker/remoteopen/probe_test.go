package remoteopen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeScriptGolden(t *testing.T) {
	tmp := Env{Tmpdir: "/run/user/1000"}
	cases := []struct {
		name string
		argv []string
		env  Env
	}{
		{"default", []string{"h"}, Env{}},
		{"tmpdir", []string{"h"}, tmp},
		{"sess", []string{"h", "work"}, Env{}},
		{"sess-quote", []string{"h", "it's"}, Env{}},
		{"want-index", []string{"h", "work", "2"}, Env{}},
		{"want-id", []string{"h", "work", "@5"}, Env{}},
		{"restore", []string{"h", "work"}, Env{Restore: "1"}},
		{"restore-win", []string{"h", "work", "3"}, Env{Restore: "1"}},
		{"newdir", []string{"h", "proj"}, Env{NewDir: "/srv/p"}},
		{"nosess-win", []string{"h", "", "2"}, Env{}},
		{"placed", []string{"h", "work", "--into", "dispatch"}, Env{}},
		{"placed-tmpdir", []string{"h", "work", "--into", "dispatch"}, tmp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := ParseArgs(c.argv, c.env)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "probe", c.name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			got := buildProbeScript(o) + "\n"
			if got == string(want) {
				return
			}
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			t.Fatalf("differs at offset %d (got %d bytes, want %d)\ngot:  %q\nwant: %q", i, len(got), len(want), tail(got, i), tail(string(want), i))
		})
	}
}

func tail(s string, i int) string {
	end := min(len(s), i+60)
	return s[i:end]
}

func TestParseProbe(t *testing.T) {
	out := "os=Linux\nuid=1000\ntmux=/usr/bin/tmux\ntmpdir=/run/user/1000\nsess=a=b\nwin=1\nwant=\n" +
		"Welcome to fish\nsess=first\nsess=x\r\n"
	got := parseProbe(out)
	want := probeResult{OS: "Linux", UID: "1000", Tmux: "/usr/bin/tmux", Tmpdir: "/run/user/1000", Sess: "x\r", Win: "1", Want: ""}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got := parseProbe("sess=last"); got.Sess != "last" {
		t.Fatalf("unterminated final line: %+v", got)
	}
	if got := parseProbe(""); got != (probeResult{}) {
		t.Fatalf("empty: %+v", got)
	}
}
