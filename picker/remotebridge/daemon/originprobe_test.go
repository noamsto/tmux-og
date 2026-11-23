package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestGithubSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"github.com/o/n", "o/n"},
		{"github.com:o/n.git", "o/n"},
		{"github.com/o/n.git", "o/n"},
		{"GitHub.com/O/N/", "O/N"},
		{"user:tok@github.com/o/n", "o/n"},
		{"https://tok@github.com/o/n.git", "o/n"},
		{"ssh://git@github.com/o/n", "o/n"},
		{"git@github.com:o/n", "o/n"},
		{"github.com/o/a.b_c-d", "o/a.b_c-d"},
		{"notgithub.com.example/o/n", ""},
		{"github.com.evil/o/n", ""},
		{"xgithub.com/o/n", ""},
		{"gitlab.com/o/n", ""},
		{"gitlab.com/o/github.com", ""},
		{"github.com/o", ""},
		{"github.com/o/n/extra", ""},
		{"github.com:22/o/n", ""},
		{"github.com/-o/n", ""},
		{"github.com/" + strings.Repeat("a", 40) + "/n", ""},
		{"github.com/o/..", ""},
		{"github.com/o/.", ""},
		{"github.com/o/n#x", ""},
		{"github.com/o/n x", ""},
		{"github.com/o/", ""},
		{"github.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := githubSlug(c.in); got != c.want {
			t.Errorf("githubSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOriginProbeCmdShape(t *testing.T) {
	for _, bad := range []string{"", "1", "@", "@a", "@1;x", "%1", "@1 ", "sess:@1"} {
		if _, ok := originProbeCmd(bad); ok {
			t.Errorf("originProbeCmd(%q) accepted a non-window-id", bad)
		}
	}
	cmd, ok := originProbeCmd("@12")
	if !ok {
		t.Fatal("originProbeCmd rejected @12")
	}
	want := "run-shell -b -t @12 " + tmuxQuote("exec /bin/sh -c "+tmuxQuote(originProbeScript("@12")))
	if cmd != want {
		t.Errorf("command\n got %q\nwant %q", cmd, want)
	}

	script := originProbeScript("@12")
	if strings.ContainsAny(script, "'\\") {
		t.Errorf("script holds a single quote or backslash: %q", script)
	}
	for _, bad := range []string{"##", "$#", "#{", "#(", "#P", "#S", "#W", "#T", "#D", "#F", "#I", "#H", "#["} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains %q, which run-shell would rewrite: %q", bad, script)
		}
	}
	rest := script
	for _, ok := range []string{"${p#*=}", "${u#*://}", "${u#*@}"} {
		if strings.Count(rest, ok) != 1 {
			t.Errorf("script lacks exactly one %q: %q", ok, script)
		}
		rest = strings.Replace(rest, ok, "", 1)
	}
	if strings.Contains(rest, "#") {
		t.Errorf("script holds a # outside the sanctioned trims: %q", script)
	}
}

// runOriginProbe runs the script body under /bin/sh with stub tmux and git, and
// returns the argv of each tmux call that was a set-option.
func runOriginProbe(t *testing.T, worktree, gitRoot, origin string) []string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "sets")
	tmux := `#!/bin/sh
case "$1" in
show-environment) echo "PATH=/nonexistent" ;;
show-options)
	case "$*" in
	*@worktree) printf %s "$STUB_WORKTREE" ;;
	*@git_root) printf %s "$STUB_GIT_ROOT" ;;
	esac ;;
set-option) printf '%s\n' "$*" >> "$STUB_LOG" ;;
esac
`
	git := `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG.git"
printf '%s\n' "$STUB_ORIGIN"
`
	for name, body := range map[string]string{"tmux": tmux, "git": git} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // stub executables must be executable
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", originProbeScript("@7"))
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"STUB_LOG=" + log,
		"STUB_WORKTREE=" + worktree,
		"STUB_GIT_ROOT=" + gitRoot,
		"STUB_ORIGIN=" + origin,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v: %s", err, out)
	}
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestOriginProbeScriptStamps(t *testing.T) {
	cases := []struct {
		name, worktree, gitRoot, origin string
		want                            []string
	}{
		{
			name: "https with a token as userinfo", worktree: "/w/a", origin: "https://tok@github.com/o/n.git",
			want: []string{"set-option -w -t @7 @og_bridge_origin github.com/o/n.git /w/a"},
		},
		{
			name: "scp", worktree: "/w/a", origin: "git@github.com:o/n",
			want: []string{"set-option -w -t @7 @og_bridge_origin github.com:o/n /w/a"},
		},
		{
			name: "git_root fallback", gitRoot: "/r/b", origin: "ssh://git@github.com/o/n",
			want: []string{"set-option -w -t @7 @og_bridge_origin github.com/o/n /r/b"},
		},
		{
			name: "no origin still records the dir", worktree: "/w/a", origin: "",
			want: []string{"set-option -w -t @7 @og_bridge_origin  /w/a"},
		},
		{name: "no dir stamps nothing", origin: "https://github.com/o/n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runOriginProbe(t, c.worktree, c.gitRoot, c.origin)
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("stamps = %q, want %q", got, c.want)
			}
		})
	}
}

// batchRT records every command and how many round-trips carried them.
type batchRT struct {
	cmds    []string
	batches int
}

func (b *batchRT) rt(cmds ...string) replies {
	b.cmds = append(b.cmds, cmds...)
	b.batches++
	n := 0
	return func() (controlmode.Line, bool) {
		n++
		return controlmode.Line{}, n <= len(cmds)
	}
}

func TestLabelShipperProbesOriginOncePerDir(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	s := newLabelShipper()
	var calls [][]string
	cfg := mirrorCfg(&calls)
	b := &batchRT{}

	rows := []labelRow{{id: "@1", dir: "/w/a"}, {id: "@2", dir: "/w/b"}, {id: "@3", dir: "/w/c"}, {id: "@4"}}
	s.apply(cfg, reg, rows)
	s.probeOrigin(reg, b.rt, rows)
	if len(b.cmds) != 2 || b.batches != 1 {
		t.Fatalf("first pass sent %d probes in %d round-trips, want 2 in 1 (unmirrored and dir-less rows skipped): %v", len(b.cmds), b.batches, b.cmds)
	}
	for i, win := range []string{"@1", "@2"} {
		want, _ := originProbeCmd(win)
		if b.cmds[i] != want {
			t.Errorf("probe %d = %q, want %q", i, b.cmds[i], want)
		}
	}

	s.probeOrigin(reg, b.rt, rows)
	if len(b.cmds) != 2 {
		t.Errorf("an unchanged row was probed again: %v", b.cmds[2:])
	}

	rows[0].dir = "/w/a2"
	s.probeOrigin(reg, b.rt, rows)
	if len(b.cmds) != 3 {
		t.Fatalf("a dir change sent %d new probes, want 1", len(b.cmds)-2)
	}

	rows[0].originDir = "/w/a2"
	rows[1].dir = "/w/b2"
	rows[1].originDir = "/w/b2"
	rows[0].dir = "/w/a3"
	s.probeOrigin(reg, b.rt, rows)
	if len(b.cmds) != 4 {
		t.Errorf("a row whose stamp matches its dir was probed: %v", b.cmds[3:])
	}

	reg.remove("@2")
	s.apply(cfg, reg, nil)
	if _, ok := s.probed["@2"]; ok {
		t.Errorf("probed = %v, want the departed window forgotten", s.probed)
	}
}

func TestWindowLabelRepoDerivation(t *testing.T) {
	row := func(dir, stamp string) labelRow {
		fields := make([]string, windowLabelFields)
		fields[0], fields[14], fields[22] = "@1", dir, stamp
		rows := parseWindowLabels(strings.Join(fields, "|"))
		if len(rows) != 1 {
			t.Fatalf("got %d rows", len(rows))
		}
		return rows[0]
	}
	cases := []struct {
		name, dir, stamp, repo, originDir string
	}{
		{"matching stamp", "/w/a", "github.com/o/n /w/a", "o/n", "/w/a"},
		{"stale stamp for another dir", "/w/b", "github.com/o/n /w/a", "", "/w/a"},
		{"garbage origin", "/w/a", "evil.example/o/n /w/a", "", "/w/a"},
		{"origin-less stamp", "/w/a", " /w/a", "", "/w/a"},
		{"no stamp", "/w/a", "", "", ""},
		{"no dir", "", "github.com/o/n /w/a", "", "/w/a"},
	}
	for _, c := range cases {
		got := row(c.dir, c.stamp)
		if got.repo != c.repo || got.originDir != c.originDir {
			t.Errorf("%s: repo=%q originDir=%q, want %q %q", c.name, got.repo, got.originDir, c.repo, c.originDir)
		}
	}

	var calls [][]string
	reg := newRegistry()
	reg.add("@1", "@101")
	newLabelShipper().apply(mirrorCfg(&calls), reg, []labelRow{row("/w/a", "github.com/o/n /w/a")})
	if len(calls) != 1 {
		t.Fatalf("apply issued %d tmux calls, want 1: %v", len(calls), calls)
	}
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "set-option -w -t @101 @bridge_repo o/n") {
		t.Errorf("@bridge_repo not stamped: %q", got)
	}
}
