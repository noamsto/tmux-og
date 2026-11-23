package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

const (
	hoPR     = "enter\t" + `{"repo":"o/n","kind":"pr","target":"feat/x","pr":7,"args":["switch","feat/x"],"key":"enter"}`
	hoFork   = "enter\t" + `{"repo":"o/n","kind":"pr","target":"pr:7","pr":7,"args":["switch","pr:7"],"key":"enter"}`
	hoIssue  = "enter\t" + `{"repo":"o/n","kind":"issue","target":"feat/9-x","args":["switch","-c","feat/9-x"],"key":"enter"}`
	hoOdd    = "enter\t" + `{"repo":"o/n","kind":"pr","target":"feat/a+b@c.d-1","pr":7,"args":["switch","feat/a+b@c.d-1"],"key":"enter"}`
	hoOddRef = "feat/a+b@c.d-1"
)

// hoBranch is a W line for a pr entry whose target and last arg are both t, so
// a rejection names the target rule rather than the args shape.
func hoBranch(t string) string {
	return "W\t" + `{"repo":"o/n","kind":"pr","target":"` + t + `","pr":7,"args":["switch","` + t + `"],"key":"W"}`
}

func TestParseHandoffAccepts(t *testing.T) {
	long := strings.Repeat("a", 255)
	cases := []struct {
		name  string
		lines []string
		want  []handoffEntry
	}{
		{"same-repo pr", []string{hoPR}, []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}}},
		{"fork pr", []string{hoFork}, []handoffEntry{{repo: "o/n", mode: "switch", target: "pr:7", pr: 7}}},
		{"issue creates", []string{hoIssue}, []handoffEntry{{repo: "o/n", mode: "create", target: "feat/9-x"}}},
		{"unusual in-charset branch", []string{hoOdd}, []handoffEntry{{repo: "o/n", mode: "switch", target: hoOddRef, pr: 7}}},
		{"255-byte branch", []string{hoBranch(long)}, []handoffEntry{{repo: "o/n", mode: "switch", target: long, pr: 7}}},
		{
			"bulk of three",
			[]string{hoBranch("a"), hoBranch("b/c"), hoBranch("d")},
			[]handoffEntry{
				{repo: "o/n", mode: "switch", target: "a", pr: 7},
				{repo: "o/n", mode: "switch", target: "b/c", pr: 7},
				{repo: "o/n", mode: "switch", target: "d", pr: 7},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseHandoff(c.lines)
			if err != nil {
				t.Fatalf("parseHandoff: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("entries = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseHandoffRejects(t *testing.T) {
	pr := func(repo, kind, target, prField, args, key string) string {
		return "enter\t" + `{"repo":"` + repo + `","kind":"` + kind + `","target":"` + target + `"` + prField +
			`,"args":` + args + `,"key":"` + key + `"}`
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = hoPR
	}
	cases := []struct {
		name  string
		lines []string
		want  string // substring of the error
	}{
		{"zero lines", nil, "1..32"},
		{"33 lines", many, "1..32"},
		{"not json", []string{"enter\tnot json"}, "entry 1"},
		{"no tab", []string{"enter"}, "entry 1"},
		{"argv form", []string{"enter\t" + `["wt","-C","/x","switch","feat/x"]`}, "entry 1"},
		{"key mismatch", []string{"W\t" + strings.TrimPrefix(hoPR, "enter\t")}, "entry 1"},
		{"key with a space", []string{"en ter\t" + `{"repo":"o/n","kind":"pr","target":"feat/x","pr":7,"args":["switch","feat/x"],"key":"en ter"}`}, "entry 1"},
		{"repo without name", []string{pr("o", "pr", "feat/x", `,"pr":7`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"repo with extra segment", []string{pr("o/n/x", "pr", "feat/x", `,"pr":7`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"repo owner leading dash", []string{pr("-o/n", "pr", "feat/x", `,"pr":7`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"repo name dotdot", []string{pr("o/..", "pr", "feat/x", `,"pr":7`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"unknown kind", []string{pr("o/n", "foo", "feat/x", `,"pr":7`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"pr kind without pr", []string{pr("o/n", "pr", "feat/x", ``, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"pr kind with pr 0", []string{pr("o/n", "pr", "feat/x", `,"pr":0`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"pr out of range", []string{pr("o/n", "pr", "feat/x", `,"pr":1000000000`, `["switch","feat/x"]`, "enter")}, "entry 1"},
		{"issue with a pr", []string{pr("o/n", "issue", "feat/x", `,"pr":3`, `["switch","-c","feat/x"]`, "enter")}, "entry 1"},
		{"args too short", []string{pr("o/n", "pr", "feat/x", `,"pr":7`, `["switch"]`, "enter")}, `["switch"]`},
		{"args with extra flags", []string{pr("o/n", "pr", "feat/x", `,"pr":7`, `["switch","-x","claude","feat/x"]`, "enter")}, `["switch" "-x" "claude" "feat/x"]`},
		{"args not a switch", []string{pr("o/n", "pr", "feat/x", `,"pr":7`, `["remove","feat/x"]`, "enter")}, `["remove" "feat/x"]`},
		{"create on a pr kind", []string{pr("o/n", "pr", "x", `,"pr":7`, `["switch","-c","x"]`, "enter")}, `["switch" "-c" "x"]`},
		{"target is not the last arg", []string{pr("o/n", "pr", "feat/x", `,"pr":7`, `["switch","feat/y"]`, "enter")}, `["switch" "feat/y"]`},
		{"command substitution", []string{hoBranch("$(touch pwned)")}, "entry 1"},
		{"semicolon", []string{hoBranch("a;b")}, "entry 1"},
		{"single quote", []string{hoBranch("a'b")}, "entry 1"},
		{"space", []string{hoBranch("a b")}, "entry 1"},
		{"tmux format", []string{hoBranch("#{x}")}, "entry 1"},
		{"leading dash", []string{hoBranch("-rf")}, "entry 1"},
		{"dotdot", []string{hoBranch("a..b")}, "entry 1"},
		{"double slash", []string{hoBranch("a//b")}, "entry 1"},
		{"trailing slash", []string{hoBranch("a/")}, "entry 1"},
		{"dot lock", []string{hoBranch("a.lock")}, "entry 1"},
		{"trailing dot", []string{hoBranch("a.")}, "entry 1"},
		{"256-byte branch", []string{hoBranch(strings.Repeat("a", 256))}, "entry 1"},
		{"pr target names another pr", []string{pr("o/n", "pr", "pr:7", `,"pr":8`, `["switch","pr:7"]`, "enter")}, "entry 1"},
		{"pr target on an issue", []string{pr("o/n", "issue", "pr:7", ``, `["switch","pr:7"]`, "enter")}, "entry 1"},
		{"line over 4096 bytes", []string{strings.Replace(hoPR, `"key"`, `"pad":"`+strings.Repeat("x", 4096)+`","key"`, 1)}, "entry 1"},
		{"later entry is named", []string{hoPR, hoIssue, hoBranch("a b")}, "entry 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseHandoff(c.lines)
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if got != nil {
				t.Errorf("returned entries alongside the error: %+v", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestHandoffScriptIsQuoteSafe(t *testing.T) {
	if strings.ContainsAny(handoffScript, "'\\") {
		t.Errorf("script holds a single quote or backslash: %q", handoffScript)
	}
	for _, bad := range []string{"##", "$#", "#{", "#(", "#P", "#S", "#W", "#T", "#D", "#F", "#I", "#H", "#["} {
		if strings.Contains(handoffScript, bad) {
			t.Errorf("script contains %q: %q", bad, handoffScript)
		}
	}
	rest := strings.NewReplacer("${p#*=}", "", "${b#*=}", "").Replace(handoffScript)
	if strings.Contains(rest, "#") {
		t.Errorf("script holds a # outside the PATH/BROWSER trims: %q", handoffScript)
	}
}

func TestHandoffCommandShape(t *testing.T) {
	got := handoffCommand("%3", "@1", "'/d'", []handoffEntry{
		{repo: "o/n", mode: "switch", target: "feat/x", pr: 7},
		{repo: "o/n", mode: "create", target: "feat/9-x"},
	})
	want := "new-pane -t %3 -c '/d' -x 96% -y 94% -X 2% -Y 3% -B heavy -A " +
		`'exec /bin/sh -c '\''` + handoffScript + `'\'' sh` +
		` '\''o/n'\'' '\''switch'\'' '\''feat/x'\'' '\''7'\''` +
		` '\''o/n'\'' '\''create'\'' '\''feat/9-x'\'' '\''0'\'''` +
		" ; set -p -t @1 @pane_label prdash-handoff"
	if got != want {
		t.Errorf("command\n got %q\nwant %q", got, want)
	}
}

// The validator is the trust boundary, but even an entry that bypassed it
// reaches the remote only as a quoted positional parameter: the script body is
// a constant, so a hostile target never lands in code sh parses.
func TestHandoffCommandKeepsDataPositional(t *testing.T) {
	const evil = "$(touch pwned)"
	got := handoffCommand("%3", "@1", "'/d'", []handoffEntry{{repo: "o/n", mode: "switch", target: evil, pr: 7}})
	if strings.Count(got, evil) != 1 {
		t.Fatalf("target appears %d times: %q", strings.Count(got, evil), got)
	}
	body := `'\''` + handoffScript + `'\'' sh `
	end := strings.Index(got, body)
	if end < 0 {
		t.Fatalf("script body not found intact: %q", got)
	}
	end += len(body)
	at := strings.Index(got, evil)
	if at < end {
		t.Errorf("target sits inside the script body: %q", got)
	}
	if !strings.Contains(got[end:], `'\''`+evil+`'\''`) {
		t.Errorf("target is not single-quoted in the positional section: %q", got[end:])
	}
}

func TestParseCtlHandoff(t *testing.T) {
	c := newCtlStateWith("@1", "%3")
	req, err := c.parseCtl([]string{wire.CtlProtocolVersion, "handoff", "%3", "/d", hoPR}, "rem")
	if err != nil {
		t.Fatalf("parseCtl: %v", err)
	}
	want := handoffCommand("%3", "@1", "'/d'", []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}})
	if len(req.cmds) != 1 || req.cmds[0] != want {
		t.Errorf("cmds = %q, want [%q]", req.cmds, want)
	}
	if req.wantLayout != "@1" || req.invalidate != "@1" {
		t.Errorf("wantLayout = %q, invalidate = %q, want @1 for both", req.wantLayout, req.invalidate)
	}

	req, err = c.parseCtl([]string{wire.CtlProtocolVersion, "handoff", "%3", "/a#b", hoPR}, "rem")
	if err != nil {
		t.Fatalf("parseCtl with an unusable dir: %v", err)
	}
	if !strings.HasPrefix(req.cmds[0], "new-pane -t %3 -c '#{pane_current_path}' ") {
		t.Errorf("an unusable dir did not fall back to the pane's cwd: %q", req.cmds[0])
	}

	req, err = c.parseCtl([]string{wire.CtlProtocolVersion, "handoff", "%3", "/d", hoPR, hoBranch("a;b")}, "rem")
	if err == nil || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("invalid line: err = %v, want one naming entry 2", err)
	}
	if req.cmds != nil {
		t.Errorf("an invalid batch still built commands: %q", req.cmds)
	}

	if _, err := c.parseCtl([]string{wire.CtlProtocolVersion, "handoff", "%3", "/d"}, "rem"); err == nil || !strings.Contains(err.Error(), "wants 2 argument") {
		t.Errorf("no lines: err = %v, want an arity error", err)
	}
	many := []string{wire.CtlProtocolVersion, "handoff", "%3", "/d"}
	for range 33 {
		many = append(many, hoPR)
	}
	if _, err := c.parseCtl(many, "rem"); err == nil || !strings.Contains(err.Error(), "wants 2 argument") {
		t.Errorf("33 lines: err = %v, want an arity error", err)
	}

	_, err = c.parseCtl([]string{"10", "handoff", "%3", "/d", hoPR}, "rem")
	if err == nil || !strings.Contains(err.Error(), `ctl protocol version "10", this daemon speaks "11" — reopen the bridge`) {
		t.Errorf("v10 ctl: err = %v, want the reopen message", err)
	}
}

// handoffRig is a temp HOME holding a cwd "checkout", a repos root and stub
// tmux/git/wt/sleep. PATH holds only the stub dir (plus a tr link), so no real
// tmux, git or wt can run.
type handoffRig struct {
	bin, home, root, cwd string
	origins, refs, wtLog string
	sleepLog             string
}

func newHandoffRig(t *testing.T, withWt bool) *handoffRig {
	t.Helper()
	d := t.TempDir()
	r := &handoffRig{
		bin:      filepath.Join(d, "bin"),
		home:     filepath.Join(d, "home"),
		root:     filepath.Join(d, "root"),
		cwd:      filepath.Join(d, "cwd"),
		origins:  filepath.Join(d, "origins"),
		refs:     filepath.Join(d, "refs"),
		wtLog:    filepath.Join(d, "wt.log"),
		sleepLog: filepath.Join(d, "sleep.log"),
	}
	for _, p := range []string{r.bin, r.home, r.root, r.cwd} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := exec.LookPath("tr")
	if err != nil {
		t.Skipf("no tr: %v", err)
	}
	if err := os.Symlink(tr, filepath.Join(r.bin, "tr")); err != nil {
		t.Fatal(err)
	}
	// git emulates `-C <dir> remote get-url origin` from the origins map and
	// `-C <dir> rev-parse --verify --quiet <ref>` from the refs list, both
	// tab-separated and keyed by the -C argument exactly as passed.
	git := `#!/bin/sh
tab=$(printf '\t')
dir=$2
case "$3" in
remote)
	while IFS="$tab" read -r d o; do
		if [ "$d" = "$dir" ]; then printf '%s\n' "$o"; exit 0; fi
	done < "$STUB_ORIGINS"
	echo "fatal: not a git repository" >&2; exit 128 ;;
rev-parse)
	while IFS="$tab" read -r d r; do
		if [ "$d" = "$dir" ] && [ "$r" = "$6" ]; then echo deadbeef; exit 0; fi
	done < "$STUB_REFS"
	exit 1 ;;
esac
exit 2
`
	stubs := map[string]string{
		"git":   git,
		"tmux":  "#!/bin/sh\nexit 0\n",
		"sleep": "#!/bin/sh\nprintf 'sleep %s\\n' \"$*\" >> \"$STUB_SLEEP\"\n",
	}
	if withWt {
		stubs["wt"] = "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"$STUB_WT\"\necho -- >> \"$STUB_WT\"\n[ \"$*\" != \"$STUB_WT_FAIL\" ]\n"
	}
	for name, body := range stubs {
		writeStub(t, filepath.Join(r.bin, name), body)
	}
	for _, p := range []string{r.origins, r.refs} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *handoffRig) add(t *testing.T, file, dir, value string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(b, dir+"\t"+value+"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run executes /bin/sh with args in the rig's cwd and returns stdout, the wt invocations
// (one []string per call) and whether the failure sleep ran.
func (r *handoffRig) run(t *testing.T, env []string, args ...string) (string, [][]string, bool) {
	t.Helper()
	cmd := exec.Command("/bin/sh", args...)
	cmd.Dir = r.cwd
	cmd.Env = append([]string{
		"PATH=" + r.bin,
		"HOME=" + r.home,
		"STUB_ORIGINS=" + r.origins,
		"STUB_REFS=" + r.refs,
		"STUB_WT=" + r.wtLog,
		"STUB_SLEEP=" + r.sleepLog,
	}, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	var calls [][]string
	b, err := os.ReadFile(r.wtLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var cur []string
	for l := range strings.Lines(string(b)) {
		if l == "--\n" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		cur = append(cur, strings.TrimSuffix(l, "\n"))
	}
	_, serr := os.Stat(r.sleepLog)
	return string(out), calls, serr == nil
}

func handoffArgs(es ...handoffEntry) []string {
	args := []string{"-c", handoffScript, "sh"}
	for _, e := range es {
		args = append(args, e.repo, e.mode, e.target, strconv.Itoa(e.pr))
	}
	return args
}

func TestHandoffScriptRuns(t *testing.T) {
	cases := []struct {
		name     string
		origins  [][2]string // -C dir ("." or a path under root/home) → origin
		refs     [][2]string
		rootless bool // leave PRDASH_REPOS_ROOT unset: the root is $HOME/git
		entries  []handoffEntry
		want     [][]string
		stdout   string
		slept    bool
	}{
		{
			name:    "same-repo pr with a local branch",
			origins: [][2]string{{".", "git@github.com:O/N.git"}},
			refs:    [][2]string{{".", "refs/heads/feat/x"}},
			entries: []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}},
			want:    [][]string{{"-C", ".", "switch", "feat/x"}},
		},
		{
			name:    "same-repo pr with only the remote branch",
			origins: [][2]string{{".", "https://github.com/o/n/"}},
			refs:    [][2]string{{".", "refs/remotes/origin/feat/x"}},
			entries: []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}},
			want:    [][]string{{"-C", ".", "switch", "feat/x"}},
		},
		{
			name:    "unfetched branch falls back to the pr",
			origins: [][2]string{{".", "git@github.com:o/n.git"}},
			entries: []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}},
			want:    [][]string{{"-C", ".", "switch", "pr:7"}},
		},
		{
			name:    "fork pr target passes through",
			origins: [][2]string{{".", "git@github.com:o/n.git"}},
			entries: []handoffEntry{{repo: "o/n", mode: "switch", target: "pr:7", pr: 7}},
			want:    [][]string{{"-C", ".", "switch", "pr:7"}},
		},
		{
			name:    "issue creates",
			origins: [][2]string{{".", "git@github.com:o/n.git"}},
			entries: []handoffEntry{{repo: "o/n", mode: "create", target: "feat/9-x"}},
			want:    [][]string{{"-C", ".", "switch", "-c", "feat/9-x"}},
		},
		{
			name:    "other repo under the root",
			origins: [][2]string{{".", "git@github.com:o/n.git"}, {"ROOT/o2/n2", "ssh://git@github.com/o2/n2.git"}},
			refs:    [][2]string{{"ROOT/o2/n2", "refs/heads/feat/y"}},
			entries: []handoffEntry{{repo: "o2/n2", mode: "switch", target: "feat/y", pr: 3}},
			want:    [][]string{{"-C", "ROOT/o2/n2", "switch", "feat/y"}},
		},
		{
			name:     "root defaults to HOME/git",
			origins:  [][2]string{{"HOME/git/o2/n2", "https://github.com/o2/n2"}},
			rootless: true,
			entries:  []handoffEntry{{repo: "o2/n2", mode: "create", target: "feat/1-z"}},
			want:     [][]string{{"-C", "HOME/git/o2/n2", "switch", "-c", "feat/1-z"}},
		},
		{
			name:    "origin mismatch runs nothing",
			origins: [][2]string{{".", "git@github.com:other/n.git"}, {"ROOT/o/n", "git@github.com.evil:o/n.git"}},
			entries: []handoffEntry{{repo: "o/n", mode: "switch", target: "feat/x", pr: 7}},
			stdout:  "tmux-og: no checkout of o/n here or at ROOT/o/n\n",
			slept:   true,
		},
		{
			name:    "bulk runs in order",
			origins: [][2]string{{".", "git@github.com:o/n.git"}},
			refs:    [][2]string{{".", "refs/heads/a"}, {".", "refs/heads/c"}},
			entries: []handoffEntry{
				{repo: "o/n", mode: "switch", target: "a", pr: 1},
				{repo: "o/n", mode: "switch", target: "b", pr: 2},
				{repo: "o/n", mode: "create", target: "c"},
			},
			want: [][]string{
				{"-C", ".", "switch", "a"},
				{"-C", ".", "switch", "pr:2"},
				{"-C", ".", "switch", "-c", "c"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHandoffRig(t, true)
			root := r.root
			env := []string{"PRDASH_REPOS_ROOT=" + r.root}
			if c.rootless {
				root = filepath.Join(r.home, "git")
				env = nil
			}
			expand := strings.NewReplacer("ROOT", root, "HOME", r.home).Replace
			for _, o := range c.origins {
				r.add(t, r.origins, expand(o[0]), o[1])
			}
			for _, ref := range c.refs {
				r.add(t, r.refs, expand(ref[0]), ref[1])
			}
			out, calls, slept := r.run(t, env, handoffArgs(c.entries...)...)
			var want [][]string
			for _, w := range c.want {
				var e []string
				for _, a := range w {
					e = append(e, expand(a))
				}
				want = append(want, e)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Errorf("wt calls = %q, want %q", calls, want)
			}
			if out != expand(c.stdout) {
				t.Errorf("stdout = %q, want %q", out, expand(c.stdout))
			}
			if slept != c.slept {
				t.Errorf("slept = %v, want %v", slept, c.slept)
			}
		})
	}
}

func TestHandoffScriptKeepsGoingAfterAFailure(t *testing.T) {
	r := newHandoffRig(t, true)
	r.add(t, r.origins, ".", "git@github.com:o/n.git")
	_, calls, slept := r.run(t, []string{"PRDASH_REPOS_ROOT=" + r.root, "STUB_WT_FAIL=-C . switch -c a"},
		handoffArgs(
			handoffEntry{repo: "o/n", mode: "create", target: "a"},
			handoffEntry{repo: "o/n", mode: "create", target: "b"},
		)...)
	want := [][]string{{"-C", ".", "switch", "-c", "a"}, {"-C", ".", "switch", "-c", "b"}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("wt calls = %q, want %q", calls, want)
	}
	if !slept {
		t.Error("a failed wt did not hold the float open")
	}
}

func TestHandoffScriptWithoutWt(t *testing.T) {
	r := newHandoffRig(t, false)
	r.add(t, r.origins, ".", "git@github.com:o/n.git")
	out, calls, slept := r.run(t, []string{"PRDASH_REPOS_ROOT=" + r.root},
		handoffArgs(handoffEntry{repo: "o/n", mode: "create", target: "a"})...)
	if calls != nil {
		t.Errorf("wt ran: %q", calls)
	}
	if out != "tmux-og: wt is not on PATH on this host\n" || !slept {
		t.Errorf("stdout = %q, slept = %v; want the missing-wt message held open", out, slept)
	}
}

// tmuxUnquote undoes tmuxQuote on one argument: tmux reads a single-quoted
// run literally and a backslash-escaped quote between two runs as one quote.
func tmuxUnquote(t *testing.T, s string) string {
	t.Helper()
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		t.Fatalf("not a tmux-quoted argument: %q", s)
	}
	return strings.ReplaceAll(s[1:len(s)-1], `'\''`, `'`)
}

// The whole built command, not just the script: the shell-command argument is
// pulled out of the new-pane line, tmux-unquoted, and run under sh as the
// remote shell would, so the branch crosses every quoting layer the real path
// has before reaching wt.
func TestHandoffCommandDeliversTargetsByteIdentical(t *testing.T) {
	r := newHandoffRig(t, true)
	r.add(t, r.origins, ".", "git@github.com:o/n.git")
	r.add(t, r.refs, ".", "refs/heads/"+hoOddRef)
	es, err := parseHandoff([]string{hoOdd, hoIssue})
	if err != nil {
		t.Fatal(err)
	}
	cmd := handoffCommand("%3", "@1", "'/d'", es)
	const pre = "new-pane -t %3 -c '/d' " + remoteFloat + " "
	const post = " ; set -p -t @1 @pane_label prdash-handoff"
	if !strings.HasPrefix(cmd, pre) || !strings.HasSuffix(cmd, post) {
		t.Fatalf("unexpected command frame: %q", cmd)
	}
	shell := tmuxUnquote(t, strings.TrimSuffix(strings.TrimPrefix(cmd, pre), post))
	_, calls, _ := r.run(t, []string{"PRDASH_REPOS_ROOT=" + r.root}, "-c", shell)
	want := [][]string{{"-C", ".", "switch", hoOddRef}, {"-C", ".", "switch", "-c", "feat/9-x"}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("wt calls = %q, want %q", calls, want)
	}
}
