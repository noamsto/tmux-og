package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/probekit"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
	shaC = "3333333333333333333333333333333333333333"
	shaD = "4444444444444444444444444444444444444444"
	shaE = "5555555555555555555555555555555555555555"
	shaF = "6666666666666666666666666666666666666666"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProbeScriptQuoting(t *testing.T) {
	for name, script := range map[string]string{"ProbeScript": ProbeScript, "HeadsScript": HeadsScript} {
		if strings.ContainsAny(script, "'\\") {
			t.Errorf("%s holds a single quote or backslash; a fish login shell would mangle it", name)
		}
		if strings.Contains(script, "\n") {
			t.Errorf("%s spans lines; keep it one line", name)
		}
	}
}

func TestProbeScriptAvoidsNewGitOnlyFlags(t *testing.T) {
	if strings.Contains(ProbeScript, "--path-format") {
		t.Error("ProbeScript uses rev-parse --path-format, which git before 2.31 echoes back")
	}
}

func TestHeadsScriptRefusesPrompts(t *testing.T) {
	git := strings.Index(HeadsScript, "$t git -C")
	for _, assign := range []string{"GIT_TERMINAL_PROMPT=0 ", "GIT_ASKPASS=true ", "SSH_ASKPASS=true ", "SSH_ASKPASS_REQUIRE=never ", "GCM_INTERACTIVE=never "} {
		if i := strings.Index(HeadsScript, assign); i < 0 || git < 0 || i > git {
			t.Errorf("HeadsScript does not prefix git with %q", assign)
		}
	}
	if strings.Contains(HeadsScript, "| head") {
		t.Error("HeadsScript pipes into head, whose buffer a deadline kill loses")
	}
}

func TestProbeArgvDropsUnsafePaths(t *testing.T) {
	paths := []string{"/srv/a", "/x y", "rel/p", "/a;rm -rf", "/b/../etc", "/q'uote", "/ok/b"}
	want := []string{"sh", "-c", ProbeScript, "_", "/srv/a", "/ok/b"}
	if got := ProbeArgv(paths); !slices.Equal(got, want) {
		t.Errorf("ProbeArgv = %q, want %q", got, want)
	}
	wantCmd := "sh -c '" + ProbeScript + "' _ '/srv/a' '/ok/b'"
	if got := ProbeRemoteCommand(paths); got != wantCmd {
		t.Errorf("ProbeRemoteCommand = %q, want %q", got, wantCmd)
	}
	if got, want := ProbeRemoteCommand(nil), "sh -c '"+ProbeScript+"' _"; got != want {
		t.Errorf("ProbeRemoteCommand(nil) = %q, want %q", got, want)
	}
}

func TestHeadsArgvDropsUnsafePairs(t *testing.T) {
	reqs := []HeadReq{
		{"/srv/a", "main"},
		{"/srv/b", "ma;in"},
		{"/x y", "main"},
		{"/srv/c", "-rf"},
		{"/srv/d", "feat/x"},
	}
	want := []string{"sh", "-c", HeadsScript, "_", "/srv/a", "main", "/srv/d", "feat/x"}
	if got := HeadsArgv(reqs); !slices.Equal(got, want) {
		t.Errorf("HeadsArgv = %q, want %q", got, want)
	}
	wantCmd := "sh -c '" + HeadsScript + "' _ '/srv/a' 'main' '/srv/d' 'feat/x'"
	if got := HeadsRemoteCommand(reqs); got != wantCmd {
		t.Errorf("HeadsRemoteCommand = %q, want %q", got, wantCmd)
	}
}

func TestParseProbeWellFormed(t *testing.T) {
	got := ParseProbe("lab", readFixture(t, "probe-wellformed.txt"))
	want := HostProbe{
		Host:     "lab",
		Identity: "0123456789abcdef|noams",
		Snapshots: []Snapshot{
			{
				Host: "lab", Key: "github.com/noamsto/tmux-og", RepoPath: "/home/noams/src/og", DefaultBranch: "main",
				Worktrees: []Worktree{
					{Path: "/home/noams/src/og", Branch: "main", Head: shaA, Windows: []Window{{Session: "main", WindowID: "@1"}}},
					{Path: "/home/noams/src/og-feat", Branch: "feat/x", Head: shaB, Windows: []Window{
						{Session: "main", WindowID: "@2"}, {Session: "work", WindowID: "@3"},
					}},
					{Path: "/home/noams/src/og-det", Head: shaC},
				},
				Drift: Drift{Known: true, Behind: 3, Tracked: shaD, Stale: true},
			},
			{
				Host: "lab", Key: "gitlab.com/Org/Bare", RepoPath: "/srv/bare.git", DefaultBranch: "trunk",
				Worktrees: []Worktree{{Path: "/srv/bare-wt", Branch: "trunk", Head: shaE}},
				Drift:     Drift{Stale: true},
			},
			{
				Host: "lab", Key: "github.com/noamsto/nowt", RepoPath: "/home/noams/src/nowt", DefaultBranch: "main",
				Drift: Drift{Known: true, Ahead: 2, Tracked: shaF, Stale: true},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProbe =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseProbeIdentityOnly(t *testing.T) {
	got := ParseProbe("lab", readFixture(t, "probe-identity-only.txt"))
	want := HostProbe{Host: "lab", Identity: "0123456789abcdef|noams"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProbe = %+v, want %+v", got, want)
	}
	if got := ParseProbe("lab", ""); !reflect.DeepEqual(got, HostProbe{Host: "lab"}) {
		t.Errorf("ParseProbe(empty) = %+v", got)
	}
}

func TestParseProbeHostile(t *testing.T) {
	out := readFixture(t, "probe-hostile.txt") + "R|/home/u/long/.git|main|https://github.com/o/" + strings.Repeat("a", 4096) + "\n"
	got := ParseProbe("lab", out)
	want := HostProbe{
		Host:     "lab",
		Identity: "id|u",
		Snapshots: []Snapshot{
			{
				Host: "lab", Key: "github.com/o/ok", RepoPath: "/home/u/ok", DefaultBranch: "main",
				Worktrees: []Worktree{
					{Path: "/home/u/ok", Branch: "main", Head: shaA, Windows: []Window{{Session: "sess|with|pipes", WindowID: "@8"}}},
					{Path: "/home/u/ok-a", Head: shaA},
					{Path: "/home/u/ok-b", Head: shaA},
					{Path: "/home/u/ok-c", Branch: "feat", Head: shaA},
					{Path: "/home/u/ok-e", Head: shaA},
					{Path: "/home/u/ok-f", Head: shaA},
				},
				Drift: Drift{Known: true, Ahead: 1, Behind: 2, Stale: true},
			},
			{Host: "lab", Key: "github.com/o/badbr", RepoPath: "/home/u/badbr", Drift: Drift{Stale: true}},
		},
		Skipped: 22,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProbe =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseProbeLocalPathOriginIsDroppedSilently(t *testing.T) {
	out := "R|/srv/x/.git|main|/srv/x.git\n" +
		"W|/srv/x/.git|" + shaA + "|/srv/x|main\n" +
		"D|/srv/x/.git|1|2|" + shaB + "\n" +
		"R|/home/u/rel/.git|main|../x\n" +
		"W|/home/u/rel/.git|" + shaA + "|/home/u/rel|main\n" +
		"R|/home/u/dot/.git|main|./x\n" +
		"R|/home/u/file/.git|main|file:///srv/x.git\n" +
		"D|/home/u/file/.git|1|2|" + shaB + "\n" +
		"R|/home/u/ok/.git|main|https://github.com/o/ok\n"
	got := ParseProbe("lab", out)
	if got.Skipped != 0 || len(got.Snapshots) != 1 || got.Snapshots[0].Key != "github.com/o/ok" {
		t.Errorf("ParseProbe = %+v, want only github.com/o/ok and Skipped 0", got)
	}
}

func TestParseProbeUnkeyedRemoteOriginCountsOnce(t *testing.T) {
	out := "R|/srv/hut/.git|main|https://git.sr.ht/~user/repo\n" +
		"W|/srv/hut/.git|" + shaA + "|/srv/hut|main\n" +
		"W|/srv/hut/.git|" + shaA + "|/srv/hut-x|feat\n" +
		"D|/srv/hut/.git|1|2|" + shaB + "\n" +
		"R|/srv/evil/.git|main|#(touch x)\n" +
		"R|/home/u/ok/.git|main|git@gh_work:o/ok.git\n"
	got := ParseProbe("lab", out)
	if got.Skipped != 2 || len(got.Snapshots) != 1 || got.Snapshots[0].Key != "gh_work/o/ok" {
		t.Errorf("ParseProbe = %+v, want only gh_work/o/ok and Skipped 2", got)
	}
}

func TestParseProbeUnsafeBranchKeepsWorktree(t *testing.T) {
	out := "R|/srv/x/.git|main|https://github.com/o/x\n" +
		"W|/srv/x/.git|" + shaA + "|/srv/x|feat/a+b\n" +
		"W|/srv/x/.git|" + shaB + "|/srv/x-wt|main\n" +
		"W|/srv/x/.git|NOTHEX|/srv/x-bad|feat/a+b\n" +
		"W|/srv/x/.git|" + shaB + "|/srv/x y|feat/a+b\n"
	got := ParseProbe("lab", out)
	want := []Worktree{{Path: "/srv/x", Head: shaA}, {Path: "/srv/x-wt", Branch: "main", Head: shaB}}
	if got.Skipped != 3 || len(got.Snapshots) != 1 || got.Snapshots[0].RepoPath != "/srv/x" ||
		!reflect.DeepEqual(got.Snapshots[0].Worktrees, want) {
		t.Errorf("ParseProbe = %+v, want worktrees %+v and Skipped 3", got, want)
	}
}

func TestParseProbeEmptyMachineIDIsUnknown(t *testing.T) {
	got := ParseProbe("lab", "I||u\nI|other|u\n")
	if got.Identity != "" || got.Skipped != 0 {
		t.Errorf("ParseProbe = %+v, want no identity and nothing skipped", got)
	}
}

func TestParseProbeIdentityRejectsControlBytes(t *testing.T) {
	got := ParseProbe("lab", "I|id\x1b[31m|u\n")
	if got.Identity != "" || got.Skipped != 1 {
		t.Errorf("ParseProbe = %+v, want no identity and one skipped row", got)
	}
}

func TestDriftChip(t *testing.T) {
	tests := []struct {
		name  string
		snap  Snapshot
		want  string
		wantN string
	}{
		{"no default branch", Snapshot{Drift: Drift{Known: true, Behind: 2}}, "", ""},
		{"unknown", Snapshot{DefaultBranch: "main"}, "main ?", ""},
		{"diverged", Snapshot{DefaultBranch: "main", Drift: Drift{Known: true, Ahead: 1, Behind: 4}}, "main ≠", ""},
		{"behind", Snapshot{DefaultBranch: "main", Drift: Drift{Known: true, Behind: 3}}, "main ↓3", ""},
		{"ahead", Snapshot{DefaultBranch: "trunk", Drift: Drift{Known: true, Ahead: 1}}, "trunk ↑1", ""},
		{"up to date", Snapshot{DefaultBranch: "main", Drift: Drift{Known: true}}, "", ""},
	}
	for _, tt := range tests {
		if got := tt.snap.DriftChip(); got != tt.want {
			t.Errorf("%s: DriftChip = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestApplyHeadsAndDriftNote(t *testing.T) {
	snaps := ParseProbe("lab", readFixture(t, "probe-wellformed.txt")).Snapshots
	for _, s := range snaps {
		if got := s.DriftNote(); got != "remote head unchecked" {
			t.Errorf("%s before heads: DriftNote = %q, want %q", s.Key, got, "remote head unchecked")
		}
	}
	ApplyHeads(snaps, map[string]string{
		"/home/noams/src/og":   shaD, // matches the tracked ref
		"/home/noams/src/nowt": shaA, // origin moved
	})
	want := map[string]string{
		"github.com/noamsto/tmux-og": "",
		"github.com/noamsto/nowt":    "origin moved since last fetch",
		"gitlab.com/Org/Bare":        "remote head unchecked",
	}
	for _, s := range snaps {
		if got := s.DriftNote(); got != want[s.Key] {
			t.Errorf("%s: DriftNote = %q, want %q", s.Key, got, want[s.Key])
		}
	}
	if snaps[0].Drift.RemoteHead != shaD || snaps[0].Drift.Stale {
		t.Errorf("ApplyHeads left %+v", snaps[0].Drift)
	}
}

func TestParseHeads(t *testing.T) {
	out := strings.Join([]string{
		"H|/srv/a|" + shaA,
		"H|/srv/b|" + strings.ToUpper("abcdef1234567890abcdef1234567890abcdef12"),
		"H|/srv/c|abc",
		"H|/x y|" + shaB,
		"H|/srv/d",
		"X|/srv/e|" + shaC,
		"H|/srv/f|" + shaD + "|extra",
		"H|/srv/g|" + strings.Repeat("e", 64),
		"",
	}, "\n")
	want := map[string]string{"/srv/a": shaA, "/srv/g": strings.Repeat("e", 64)}
	if got := ParseHeads(out); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseHeads = %v, want %v", got, want)
	}
}

type fakeRunner struct {
	calls  [][]string
	stdout string
	stderr string
	err    error
	block  bool
}

func (f *fakeRunner) Run(ctx context.Context, argv []string) (string, string, error) {
	f.calls = append(f.calls, argv)
	if f.block {
		<-ctx.Done()
		return f.stdout, f.stderr, ctx.Err()
	}
	return f.stdout, f.stderr, f.err
}

func exitErr(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	return err
}

func TestProbeHostArgv(t *testing.T) {
	paths := []string{"/srv/a", "/bad path"}
	r := &fakeRunner{stdout: readFixture(t, "probe-wellformed.txt")}
	got := ProbeHost(context.Background(), r, LocalHost, paths)
	if got.Err != nil || len(got.Snapshots) != 3 || got.Host != LocalHost {
		t.Errorf("local ProbeHost = %+v", got)
	}
	r.stdout = ""
	ProbeHost(context.Background(), r, "lab", paths)
	want := [][]string{
		{"sh", "-c", ProbeScript, "_", "/srv/a"},
		append([]string{"ssh"}, probekit.SSHArgv("lab", ProbeRemoteCommand(paths))...),
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}
}

func TestProbeHostRefusesBadAlias(t *testing.T) {
	for _, host := range []string{"", "-oProxyCommand=x", "lab host", "lab;id", "lab\n", "lab$(id)", "lab/x"} {
		r := &fakeRunner{}
		got := ProbeHost(context.Background(), r, host, nil)
		if !errors.Is(got.Err, ErrInvalidHost) {
			t.Errorf("ProbeHost(%q).Err = %v, want ErrInvalidHost", host, got.Err)
		}
		if len(r.calls) != 0 {
			t.Errorf("ProbeHost(%q) ran %q", host, r.calls)
		}
		if heads := FetchHeads(context.Background(), r, host, []HeadReq{{"/srv/a", "main"}}); len(heads) != 0 || len(r.calls) != 0 {
			t.Errorf("FetchHeads(%q) ran %q", host, r.calls)
		}
	}
	for _, host := range []string{"lab", "user@lab.tail.net", "lab-2_x", "10.0.0.1", "lab:22", "a+b"} {
		r := &fakeRunner{}
		if got := ProbeHost(context.Background(), r, host, nil); got.Err != nil {
			t.Errorf("ProbeHost(%q).Err = %v, want nil", host, got.Err)
		}
	}
}

func TestProbeHostClassifiesErrors(t *testing.T) {
	r := &fakeRunner{stdout: "I|id|u\n", err: exitErr(t, "1")}
	got := ProbeHost(context.Background(), r, "lab", nil)
	if !errors.Is(got.Err, probekit.ErrRemoteNoServer) || got.Identity != "id|u" {
		t.Errorf("non-zero exit: %+v, want ErrRemoteNoServer with the stdout parsed", got)
	}

	r = &fakeRunner{stderr: "Permission denied (publickey).", err: exitErr(t, "255")}
	if got := ProbeHost(context.Background(), r, "lab", nil); !errors.Is(got.Err, probekit.ErrRemoteNeedsAuth) {
		t.Errorf("auth failure: Err = %v, want ErrRemoteNeedsAuth", got.Err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r = &fakeRunner{block: true}
	got = ProbeHost(ctx, r, "lab", nil)
	if !errors.Is(got.Err, probekit.ErrRemoteUnreachable) || !strings.Contains(got.Err.Error(), "timed out") {
		t.Errorf("timeout: Err = %v, want a timed-out ErrRemoteUnreachable", got.Err)
	}
}

func TestFetchHeads(t *testing.T) {
	reqs := []HeadReq{{"/srv/a", "main"}, {"/srv/b", "bad branch"}, {"/srv/c", "trunk"}}
	r := &fakeRunner{stdout: "H|/srv/a|" + shaA + "\n", err: exitErr(t, "1")}
	got := FetchHeads(context.Background(), r, "lab", reqs)
	if !reflect.DeepEqual(got, map[string]string{"/srv/a": shaA}) {
		t.Errorf("FetchHeads = %v, want the partial answer", got)
	}
	want := [][]string{append([]string{"ssh"}, probekit.SSHArgv("lab", HeadsRemoteCommand(reqs))...)}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}

	r = &fakeRunner{}
	FetchHeads(context.Background(), r, LocalHost, reqs)
	if want := [][]string{HeadsArgv(reqs)}; !reflect.DeepEqual(r.calls, want) {
		t.Errorf("local calls = %q, want %q", r.calls, want)
	}
}

// lookTool finds a tool the script tests execute. Under OG_REQUIRE_GIT a missing
// tool fails instead of skipping, so a dropped nativeBuildInputs entry cannot
// silently skip every script test.
func lookTool(t *testing.T, tool string) string {
	t.Helper()
	p, err := exec.LookPath(tool)
	if err == nil {
		return p
	}
	if os.Getenv("OG_REQUIRE_GIT") != "" {
		t.Fatalf("%s is required (OG_REQUIRE_GIT set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix", tool)
	}
	t.Skipf("%s not on PATH", tool)
	return ""
}

// scriptEnv runs the probe scripts against throwaway repos with fake tmux and
// zoxide first on PATH and a git that never reads the user's config.
type scriptEnv struct {
	t    *testing.T
	root string
	bin  string
	env  []string
}

func newScriptEnv(t *testing.T) *scriptEnv {
	t.Helper()
	for _, tool := range []string{"git", "sh"} {
		lookTool(t, tool)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{home, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e := &scriptEnv{t: t, root: root, bin: bin, env: []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=file",
		"LC_ALL=C",
	}}
	// Silent until a test fakes them: the real tmux server and zoxide database never answer.
	e.fake("tmux", "exit 0\n")
	e.fake("zoxide", "exit 0\n")
	return e
}

func (e *scriptEnv) run(argv []string) string {
	e.t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: test-built argv
	cmd.Env = e.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("%q: %v\n%s", argv, err, stderr.String())
	}
	return string(out)
}

func (e *scriptEnv) git(args ...string) string {
	e.t.Helper()
	return strings.TrimSpace(e.run(append([]string{"git"}, args...)))
}

func (e *scriptEnv) path(name string) string { return filepath.Join(e.root, name) }

// repo makes a repo on main with origin set and n empty commits, returning
// their shas.
func (e *scriptEnv) repo(name, origin string, n int) []string {
	e.t.Helper()
	dir := e.path(name)
	e.git("init", "-q", "-b", "main", dir)
	e.git("-C", dir, "config", "user.email", "t@t")
	e.git("-C", dir, "config", "user.name", "t")
	e.git("-C", dir, "config", "commit.gpgsign", "false")
	e.git("-C", dir, "remote", "add", "origin", origin)
	shas := make([]string, 0, n)
	for i := range n {
		e.git("-C", dir, "commit", "-q", "--allow-empty", "-m", "c"+string(rune('0'+i)))
		shas = append(shas, e.git("-C", dir, "rev-parse", "HEAD"))
	}
	return shas
}

func (e *scriptEnv) fake(name, body string) {
	e.t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bin, name), []byte("#!"+sh+"\n"+body), 0o700); err != nil { //nolint:gosec // G306: an executable fake
		e.t.Fatal(err)
	}
}

func fixtureOrigin(name string) string { return "https://github.com/og-fixture/" + name + ".git" }

func TestProbeScriptExecution(t *testing.T) {
	e := newScriptEnv(t)

	tm := e.repo("tmuxrepo", fixtureOrigin("tmuxrepo"), 2)
	tree := e.git("-C", e.path("tmuxrepo"), "rev-parse", tm[0]+"^{tree}")
	side := e.git("-C", e.path("tmuxrepo"), "commit-tree", "-p", tm[0], "-m", "side", tree)
	e.git("-C", e.path("tmuxrepo"), "update-ref", "refs/remotes/origin/main", side)
	e.git("-C", e.path("tmuxrepo"), "worktree", "add", "-q", "-b", "feat/x", e.path("tmuxrepo-wt"))

	z := e.repo("zrepo", fixtureOrigin("zrepo"), 2)
	e.git("-C", e.path("zrepo"), "update-ref", "refs/remotes/origin/main", z[1])
	e.git("-C", e.path("zrepo"), "update-ref", "refs/heads/main", z[0])
	e.git("-C", e.path("zrepo"), "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	cr := e.repo("cred", "https://user:secret@host.example/og-fixture/cred.git", 1)
	e.git("-C", e.path("cred"), "update-ref", "refs/remotes/origin/main", cr[0])
	if err := os.Mkdir(e.path("cred/sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	rg := e.repo("regrepo", fixtureOrigin("regrepo"), 2)
	e.git("-C", e.path("regrepo"), "update-ref", "refs/remotes/origin/main", rg[0])

	e.repo("bridgeonly", fixtureOrigin("bridgeonly"), 1)

	e.fake("tmux", `[ "$1" = list-windows ] || exit 1
printf "%s\n" "|@5|`+e.path("tmuxrepo")+`|main-sess" "|@7|`+e.path("tmuxrepo-wt")+`|other" "1|@9|`+e.path("bridgeonly")+`|mirror"
`)
	e.fake("zoxide", `[ "$1 $2" = "query -l" ] || exit 1
printf "%s\n" "`+e.path("zrepo")+`" "`+e.path("cred/sub")+`" "`+e.path("missing")+`" "relative/dir"
`)

	paths := []string{e.path("regrepo")}
	out := e.run(ProbeArgv(paths))
	if strings.Contains(out, "secret") {
		t.Errorf("credential userinfo crossed the wire:\n%s", out)
	}
	if !strings.HasPrefix(out, "I|") {
		t.Errorf("output does not start with the I row:\n%s", out)
	}

	got := ParseProbe("lab", out)
	if got.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0\n%s", got.Skipped, out)
	}
	snaps := map[string]Snapshot{}
	for _, s := range got.Snapshots {
		snaps[s.Key] = s
	}
	wantKeys := []string{
		"github.com/og-fixture/regrepo", "github.com/og-fixture/tmuxrepo",
		"github.com/og-fixture/zrepo", "host.example/og-fixture/cred",
	}
	gotKeys := make([]string, 0, len(snaps))
	for k := range snaps {
		gotKeys = append(gotKeys, k)
	}
	slices.Sort(gotKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("keys = %q, want %q\n%s", gotKeys, wantKeys, out)
	}

	tmux := snaps["github.com/og-fixture/tmuxrepo"]
	wantWT := []Worktree{
		{Path: e.path("tmuxrepo"), Branch: "main", Head: tm[1], Windows: []Window{{Session: "main-sess", WindowID: "@5"}}},
		{Path: e.path("tmuxrepo-wt"), Branch: "feat/x", Head: tm[1], Windows: []Window{{Session: "other", WindowID: "@7"}}},
	}
	if tmux.RepoPath != e.path("tmuxrepo") || !reflect.DeepEqual(tmux.Worktrees, wantWT) {
		t.Errorf("tmuxrepo = %+v, want RepoPath %s and worktrees %+v", tmux, e.path("tmuxrepo"), wantWT)
	}
	wantDrift := map[string]Drift{
		"github.com/og-fixture/tmuxrepo": {Known: true, Ahead: 1, Behind: 1, Tracked: side, Stale: true},
		"github.com/og-fixture/zrepo":    {Known: true, Behind: 1, Tracked: z[1], Stale: true},
		"host.example/og-fixture/cred":   {Known: true, Tracked: cr[0], Stale: true},
		"github.com/og-fixture/regrepo":  {Known: true, Ahead: 1, Tracked: rg[0], Stale: true},
	}
	for k, want := range wantDrift {
		if s := snaps[k]; s.Drift != want || s.DefaultBranch != "main" {
			t.Errorf("%s: drift %+v branch %q, want %+v on main", k, s.Drift, s.DefaultBranch, want)
		}
	}
	if s := snaps["host.example/og-fixture/cred"]; s.RepoPath != e.path("cred") {
		t.Errorf("cred RepoPath = %q, want %q (found through a subdirectory)", s.RepoPath, e.path("cred"))
	}

	if os.Getenv("OG_REQUIRE_GIT") != "" {
		lookTool(t, "dash")
	}
	if dash, err := exec.LookPath("dash"); err == nil {
		argv := ProbeArgv(paths)
		argv[0] = dash
		if dashOut := e.run(argv); dashOut != out {
			t.Errorf("dash output differs:\n%s\nsh:\n%s", dashOut, out)
		}
	} else {
		t.Log("dash not on PATH; dash run skipped")
	}

	if fish, err := exec.LookPath("fish"); err == nil {
		if fout := e.run([]string{fish, "--no-config", "-c", ProbeRemoteCommand(paths)}); fout != out {
			t.Errorf("fish login shell output differs:\n%s\nsh:\n%s", fout, out)
		}
	} else {
		t.Log("fish not on PATH; fish run skipped")
	}
}

func TestProbeScriptWithoutGit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "grep", "uname", "id", "head"} {
		if err := os.Symlink(lookTool(t, tool), filepath.Join(root, tool)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(root, "sh"), "-c", ProbeScript, "_", "/srv/a") //nolint:gosec // G204: test-built argv
	cmd.Env = []string{"PATH=" + root, "HOME=" + root}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "I|") {
		t.Errorf("output = %q, want only the I row", out)
	}
}

func TestProbeScriptEmptyMachineIDFallsBack(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, tool := range []string{"sh", "grep", "id", "head"} {
		tools[tool] = lookTool(t, tool)
	}
	for _, tool := range []string{"sh", "id", "head"} {
		if err := os.Symlink(tools[tool], filepath.Join(root, tool)); err != nil {
			t.Fatal(err)
		}
	}
	empty := filepath.Join(root, "machine-id")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]string{
		"grep":  `[ "$2" = /etc/machine-id ] && set -- "$1" ` + empty + "\nexec " + tools["grep"] + ` "$@"` + "\n",
		"uname": `[ "$1" = -n ] && echo fallback-host` + "\n",
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(root, name), []byte("#!"+tools["sh"]+"\n"+body), 0o700); err != nil { //nolint:gosec // G306: an executable fake
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(root, "sh"), "-c", ProbeScript, "_") //nolint:gosec // G204: test-built argv
	cmd.Env = []string{"PATH=" + root, "HOME=" + root}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	if !strings.HasPrefix(string(out), "I|fallback-host|") {
		t.Errorf("output = %q, want the I row to fall back to uname -n over an empty machine-id", out)
	}
}

func TestProbeScriptRepeatedWindowRootsDoNotStarveZoxide(t *testing.T) {
	e := newScriptEnv(t)
	e.repo("dup", fixtureOrigin("dup"), 1)
	e.repo("zlate", fixtureOrigin("zlate"), 1)
	e.fake("tmux", `[ "$1" = list-windows ] || exit 1
i=0
while [ $i -lt 200 ]; do printf "|@%s||s\n|@%s|`+e.path("dup")+`|s\n" $i $((i+1000)); i=$((i+1)); done
`)
	e.fake("zoxide", `[ "$1 $2" = "query -l" ] || exit 1
echo `+e.path("zlate")+`
`)
	out := e.run(ProbeArgv(nil))
	got := ParseProbe("lab", out)
	keys := make([]string, 0, len(got.Snapshots))
	for _, s := range got.Snapshots {
		keys = append(keys, s.Key)
	}
	slices.Sort(keys)
	if want := []string{"github.com/og-fixture/dup", "github.com/og-fixture/zlate"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}

func TestProbeScriptOnOldGit(t *testing.T) {
	e := newScriptEnv(t)
	e.repo("old", fixtureOrigin("old"), 1)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// git before 2.31 echoes the unknown --path-format option back as a line of output.
	e.fake("git", `case " $* " in *" --path-format="*) echo --path-format=absolute;; esac
exec `+realGit+` "$@"
`)
	got := ParseProbe("lab", e.run(ProbeArgv([]string{e.path("old")})))
	if len(got.Snapshots) != 1 || got.Snapshots[0].RepoPath != e.path("old") {
		t.Errorf("ParseProbe = %+v, want the repo at %s", got, e.path("old"))
	}
}

// A symlinked candidate inside a nested repo must resolve to that repo: git
// prints the common dir relative to the physical cwd, so a logical cd lands on
// the enclosing repo or nowhere. CDPATH would hijack the plain `cd .git`.
func TestProbeScriptResolvesSymlinkedCandidates(t *testing.T) {
	e := newScriptEnv(t)
	e.repo("outer", fixtureOrigin("outer"), 1)
	e.repo("outer/inner", fixtureOrigin("inner"), 1)
	for _, d := range []string{"outer/inner/sub/deep", "outer/a", "x/y/z"} {
		if err := os.MkdirAll(e.path(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"outer/a/link": e.path("outer/inner/sub/deep"),
		"outer/a/rel":  "../inner/sub/deep",
		"x/y/z/link":   e.path("outer/inner/sub"),
	} {
		if err := os.Symlink(target, e.path(link)); err != nil {
			t.Fatal(err)
		}
	}
	e.env = append(e.env, "CDPATH="+e.path("outer"))

	shells := map[string][]string{"sh": {lookTool(t, "sh")}}
	if os.Getenv("OG_REQUIRE_GIT") != "" {
		lookTool(t, "dash")
	}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells["dash"] = []string{dash}
	} else {
		t.Log("dash not on PATH; dash run skipped")
	}
	if bb, err := exec.LookPath("busybox"); err == nil {
		shells["busybox sh"] = []string{bb, "sh"}
	} else {
		t.Log("busybox not on PATH; busybox run skipped")
	}

	want := e.path("outer/inner")
	for name, shell := range shells {
		for _, c := range []string{"outer/a/link", "outer/a/rel", "x/y/z/link", "outer/inner"} {
			argv := ProbeArgv([]string{e.path(c)})
			got := ParseProbe("lab", e.run(append(slices.Clone(shell), argv[1:]...)))
			if len(got.Snapshots) != 1 || got.Snapshots[0].Key != "github.com/og-fixture/inner" || got.Snapshots[0].RepoPath != want {
				t.Errorf("%s %s: snapshots = %+v, want only github.com/og-fixture/inner at %s", name, c, got.Snapshots, want)
			}
		}
	}
}

func TestHeadsScriptKeepsAnswersPastDeadline(t *testing.T) {
	e := newScriptEnv(t)
	up := e.repo("upstream", fixtureOrigin("upstream"), 1)
	e.repo("fast", e.path("upstream"), 0)
	e.repo("slow", e.path("upstream"), 0)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	e.fake("git", `case " $* " in *" `+e.path("slow")+` "*) exec sleep 30;; esac
exec `+realGit+` "$@"
`)
	reqs := []HeadReq{{e.path("fast"), "main"}, {e.path("slow"), "main"}}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	argv := append(append([]string{"env", "-i"}, e.env...), HeadsArgv(reqs)...)
	stdout, _, err := ExecRunner{}.Run(ctx, argv)
	if err == nil {
		t.Fatal("err = nil, want the deadline to kill the hung ls-remote")
	}
	if got, want := ParseHeads(stdout), map[string]string{e.path("fast"): up[0]}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseHeads = %v, want %v (stdout %q)", got, want, stdout)
	}
}

func TestHeadsScriptExecution(t *testing.T) {
	e := newScriptEnv(t)
	up := e.repo("upstream", fixtureOrigin("upstream"), 1)
	e.repo("clone", e.path("upstream"), 0)
	reqs := []HeadReq{
		{e.path("clone"), "main"},
		{e.path("upstream"), "nosuch"},
		{e.path("missing"), "main"},
	}
	want := map[string]string{e.path("clone"): up[0]}
	if got := ParseHeads(e.run(HeadsArgv(reqs))); !reflect.DeepEqual(got, want) {
		t.Errorf("sh: ParseHeads = %v, want %v", got, want)
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		if got := ParseHeads(e.run([]string{fish, "--no-config", "-c", HeadsRemoteCommand(reqs)})); !reflect.DeepEqual(got, want) {
			t.Errorf("fish: ParseHeads = %v, want %v", got, want)
		}
	}
}

func TestRemoteCommandsParseUnderFish(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not on PATH")
	}
	for _, c := range []string{
		ProbeRemoteCommand([]string{"/srv/a", "/home/u/src/og"}),
		HeadsRemoteCommand([]HeadReq{{"/srv/a", "main"}}),
	} {
		if out, err := exec.Command(fish, "--no-config", "-n", "-c", c).CombinedOutput(); err != nil { //nolint:gosec // G204: test-built argv
			t.Errorf("fish -n rejected %q: %v\n%s", c, err, out)
		}
	}
}
