package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/probekit"
)

const (
	shaLocal = "1111111111111111111111111111111111111111"
	shaLab   = "2222222222222222222222222222222222222222"
	shaMoved = "3333333333333333333333333333333333333333"
)

// statusRunner answers by argv: the local sh, ssh to a named host (a probe, or the heads call when
// the body mentions ls-remote), ssh -G and tailscale. A host with no entry fails like ssh does.
type statusRunner struct {
	mu        sync.Mutex
	calls     [][]string
	probes    map[string]string // host → ProbeScript stdout
	heads     map[string]string // host → HeadsScript stdout
	failures  map[string]hostFailure
	tailscale string
	tsErr     error
	sshG      map[string]string // alias → hostname
}

type hostFailure struct {
	stdout, stderr string
	err            error
}

func (s *statusRunner) Run(_ context.Context, argv []string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, argv)
	switch argv[0] {
	case "tailscale":
		return s.tailscale, "", s.tsErr
	case "sh":
		if argv[2] == HeadsScript {
			return s.heads[LocalHost], "", nil
		}
		return s.probes[LocalHost], "", nil
	case "ssh":
		if argv[1] == "-G" {
			return "hostname " + s.sshG[argv[2]] + "\n", "", nil
		}
		sep := 0
		for i, a := range argv {
			if a == "--" {
				sep = i
			}
		}
		host, body := argv[sep-1], argv[sep+1]
		if f, ok := s.failures[host]; ok {
			return f.stdout, f.stderr, f.err
		}
		if strings.Contains(body, "ls-remote") {
			return s.heads[host], "", nil
		}
		if out, ok := s.probes[host]; ok {
			return out, "", nil
		}
	}
	return "", "ssh: connect to host: Connection refused", statusExitErr(255)
}

func (s *statusRunner) called(prog string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c[0] == prog && (prog != "ssh" || c[1] == "-G") {
			n++
		}
	}
	return n
}

func statusExitErr(code int) error {
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		panic("expected a non-zero exit")
	}
	return err
}

func localProbe(identity string) string {
	return "I|" + identity + "|u\n" +
		"R|/home/u/git/widget/.git|main|git@github.com:acme/widget.git\n" +
		"W|/home/u/git/widget/.git|" + shaLocal + "|/home/u/git/widget|main\n" +
		"W|/home/u/git/widget/.git|" + shaLocal + "|/home/u/git/widget-x|feat/x\n" +
		"W|/home/u/git/widget/.git|" + shaLocal + "|/home/u/git/widget-y|feat/y\n" +
		"D|/home/u/git/widget/.git|0|2|" + shaLocal + "\n"
}

func labProbe(identity string) string {
	return "I|" + identity + "|u\n" +
		"R|/srv/widget/.git|main|https://github.com/acme/widget\n" +
		"W|/srv/widget/.git|" + shaLab + "|/srv/widget|feat/x\n" +
		"D|/srv/widget/.git|1|1|" + shaLab + "\n"
}

func twoHosts() *statusRunner {
	return &statusRunner{
		probes: map[string]string{LocalHost: localProbe("m-local"), "lab": labProbe("m-lab")},
		heads: map[string]string{
			LocalHost: "H|/home/u/git/widget|" + shaLocal + "\n",
			"lab":     "H|/srv/widget|" + shaMoved + "\n",
		},
	}
}

func runStatus(t *testing.T, c Config, r Runner, q string) (out, errOut string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	code = Status(context.Background(), &so, &se, c, r, StatusOptions{Query: q})
	return so.String(), se.String(), code
}

func TestStatusTwoHostsOneProjectDuplicateBranch(t *testing.T) {
	out, errOut, code := runStatus(t, Config{BridgeHosts: []string{"lab"}}, twoHosts(), "")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	want := `Hosts
  local  reachable
  lab    reachable

github.com/acme/widget
  local  /home/u/git/widget  3 worktrees  main ↓2
  lab    /srv/widget         1 worktree   main ≠ · origin moved since last fetch
  ⚠ feat/x is checked out on local and lab (heads differ)
`
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	if n := strings.Count(out, "github.com/acme/widget"); n != 1 {
		t.Errorf("project key printed %d times, want 1", n)
	}
}

func TestStatusDriftNotes(t *testing.T) {
	r := twoHosts()
	r.heads = map[string]string{} // ls-remote answered nothing: heads stay unchecked
	out, _, _ := runStatus(t, Config{BridgeHosts: []string{"lab"}}, r, "")
	if got := strings.Count(out, "remote head unchecked"); got != 2 {
		t.Errorf("unchecked note printed %d times, want 2:\n%s", got, out)
	}
	if strings.Contains(out, "origin moved") {
		t.Errorf("origin moved without a confirmed head:\n%s", out)
	}
}

func TestStatusFetchesHeadsOnlyForDisplayedProjects(t *testing.T) {
	r := twoHosts()
	r.probes["lab"] += "R|/srv/other/.git|main|https://github.com/acme/other\n" +
		"W|/srv/other/.git|" + shaLab + "|/srv/other|main\n"
	cfg := Config{BridgeHosts: []string{"lab"}}
	runStatus(t, cfg, r, "github.com/acme/widget")
	r.mu.Lock()
	defer r.mu.Unlock()
	fetched := 0
	for _, c := range r.calls {
		if body := c[len(c)-1]; strings.Contains(body, "ls-remote") || (c[0] == "sh" && c[2] == HeadsScript) {
			fetched++
			if strings.Contains(strings.Join(c, " "), "/srv/other") {
				t.Errorf("heads fetched for an undisplayed project: %q", c)
			}
		}
	}
	if fetched != 2 {
		t.Errorf("heads fetched %d times, want once per host", fetched)
	}
}

func TestStatusCapsHeadsPerHost(t *testing.T) {
	var b strings.Builder
	b.WriteString("I|m|u\n")
	for i := range 40 {
		n := string(rune('a'+i/26)) + string(rune('a'+i%26))
		b.WriteString("R|/srv/" + n + "/.git|main|https://github.com/acme/" + n + "\n")
		b.WriteString("W|/srv/" + n + "/.git|" + shaLab + "|/srv/" + n + "|main\n")
	}
	r := &statusRunner{probes: map[string]string{LocalHost: b.String()}}
	runStatus(t, Config{}, r, "")
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c[0] == "sh" && c[2] == HeadsScript {
			if got := len(c) - 4; got != 64 {
				t.Errorf("heads call carried %d args, want 64 (32 requests)", got)
			}
		}
	}
}

func TestStatusHostStates(t *testing.T) {
	r := twoHosts()
	r.probes["same"] = localProbe("m-local")
	r.probes["flaky"] = ""
	r.failures = map[string]hostFailure{
		"down":   {stderr: "ssh: connect to host down port 22: Connection refused", err: statusExitErr(255)},
		"noauth": {stderr: "user@noauth: Permission denied (publickey).", err: statusExitErr(255)},
		"keyed":  {stderr: "REMOTE HOST IDENTIFICATION HAS CHANGED", err: statusExitErr(255)},
		"broken": {stderr: "sh: git: not found", err: statusExitErr(1)},
	}
	cfg := Config{BridgeHosts: []string{"lab", "same", "down", "noauth", "keyed", "broken"}}
	out, _, code := runStatus(t, cfg, r, "")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	hosts := out[:strings.Index(out, "\n\n")]
	for _, want := range []string{
		"  same    same machine as local, merged",
		"  down    unreachable — ",
		"  noauth  needs authentication — run og remote auth noauth",
		"  keyed   host key changed",
		"  broken  probe failed — ",
		"  lab     reachable",
	} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts block lacks %q:\n%s", want, hosts)
		}
	}
	if strings.Count(out, "/home/u/git/widget  ") != 1 {
		t.Errorf("the self machine's checkouts were merged twice:\n%s", out)
	}
}

func TestStatusAliasesOfOneMachineAreMerged(t *testing.T) {
	r := twoHosts()
	r.probes["lab-ts"] = labProbe("m-lab")
	cfg := Config{BridgeHosts: []string{"lab"}, Projects: map[string]Declared{
		"widget": {Remote: "github.com/acme/widget", Hosts: map[string]DeclaredHost{"lab-ts": {Path: "/srv/widget"}}},
	}}
	out, errOut, code := runStatus(t, cfg, r, "")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "  lab-ts  same machine as lab, merged\n") {
		t.Errorf("alias not reported as the same machine:\n%s", out)
	}
	if n := strings.Count(out, "/srv/widget  "); n != 1 {
		t.Errorf("the aliased machine's checkout printed %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "lab and lab-ts") || strings.Contains(out, "checked out on lab and") && strings.Contains(out, "lab-ts") {
		t.Errorf("false duplicate across aliases:\n%s", out)
	}
}

func TestStatusUnreachableHostDoesNotClaimIdentity(t *testing.T) {
	states, merged := hostStates([]HostProbe{
		{Host: LocalHost, Identity: "m"},
		{Host: "down", Identity: "m", Err: errors.New("down")},
		{Host: "up", Identity: "m"},
	}, nil)
	if states[1].text != "unreachable — down" || states[2].text != "same machine as local, merged" || len(merged) != 2 {
		t.Errorf("states %+v, merged %d", states, len(merged))
	}
	if got := merged[0].Aliases; len(got) != 1 || got[0] != "up" {
		t.Errorf("local aliases = %v, want [up]", got)
	}
}

// srvRepo is the R and W rows of a checkout of github.com/acme/<name> at /srv/<name>.
func srvRepo(name string) string {
	return "R|/srv/" + name + "/.git|main|https://github.com/acme/" + name + "\n" +
		"W|/srv/" + name + "/.git|" + shaLab + "|/srv/" + name + "|main\n"
}

func TestStatusClaimOnAliasWhoseTwinLacksTheRepo(t *testing.T) {
	// Only lab-ts was asked about the declared path, so only it found the checkout.
	r := twoHosts()
	r.probes["lab"] = "I|m-lab|u\n"
	r.probes["lab-ts"] = labProbe("m-lab")
	cfg := Config{BridgeHosts: []string{"lab"}, Projects: map[string]Declared{
		"widget": {Remote: "github.com/acme/widget", Hosts: map[string]DeclaredHost{"lab-ts": {Path: "/srv/widget"}}},
	}}
	out, errOut, code := runStatus(t, cfg, r, "")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if n := strings.Count(out, "/srv/widget  "); n != 1 || !strings.Contains(out, "  lab     /srv/widget  ") {
		t.Errorf("the alias's declared checkout printed %d times, want once under lab:\n%s", n, out)
	}
	if strings.Contains(out, "no checkout") {
		t.Errorf("declared checkout reported missing:\n%s", out)
	}
}

func TestStatusProjectsOnDifferentAliasesOfOneMachine(t *testing.T) {
	cfg := Config{BridgeHosts: []string{"lab"}, Projects: map[string]Declared{
		"a": {Remote: "github.com/acme/a", Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/a"}}},
		"b": {Remote: "github.com/acme/b", Hosts: map[string]DeclaredHost{"lab-ts": {Path: "/srv/b"}}},
	}}
	for name, probes := range map[string][2]string{
		"each found its own": {srvRepo("a"), srvRepo("b")},
		"both found both":    {srvRepo("a") + srvRepo("b"), srvRepo("b") + srvRepo("a")},
	} {
		r := &statusRunner{probes: map[string]string{
			LocalHost: "I|m-local|u\n",
			"lab":     "I|m-lab|u\n" + probes[0],
			"lab-ts":  "I|m-lab|u\n" + probes[1],
		}}
		out, errOut, code := runStatus(t, cfg, r, "")
		if code != 0 || errOut != "" {
			t.Fatalf("%s: code %d, stderr %q", name, code, errOut)
		}
		for _, path := range []string{"/srv/a  ", "/srv/b  "} {
			if n := strings.Count(out, path); n != 1 {
				t.Errorf("%s: %q printed %d times, want 1:\n%s", name, path, n, out)
			}
		}
		if strings.Contains(out, "⚠") || strings.Contains(out, "no checkout") {
			t.Errorf("%s: false duplicate or missing checkout:\n%s", name, out)
		}
	}
}

func TestStatusRemoteAliasOfLocalKeepsItsClaims(t *testing.T) {
	r := twoHosts()
	r.probes["self"] = localProbe("m-local") +
		"R|/home/u/git/gadget/.git|main|https://github.com/acme/gadget\n" +
		"W|/home/u/git/gadget/.git|" + shaLocal + "|/home/u/git/gadget|main\n"
	cfg := Config{Projects: map[string]Declared{
		"gadget": {Remote: "github.com/acme/gadget", Hosts: map[string]DeclaredHost{"self": {Path: "/home/u/git/gadget"}}},
	}}
	out, errOut, code := runStatus(t, cfg, r, "")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "  local  /home/u/git/gadget  1 worktree") {
		t.Errorf("the alias's declared checkout is missing under local:\n%s", out)
	}
	if n := strings.Count(out, "/home/u/git/widget  "); n != 1 || strings.Contains(out, "no checkout") {
		t.Errorf("local's own checkout printed %d times, want 1:\n%s", n, out)
	}
}

func TestStatusFoldLeavesTheProbesUntouched(t *testing.T) {
	probes := []HostProbe{
		{Host: LocalHost, Identity: "m", Snapshots: make([]Snapshot, 1, 4)},
		{Host: "self", Identity: "m", Snapshots: []Snapshot{{Host: "self", RepoPath: "/x"}}},
	}
	_, merged := hostStates(probes, nil)
	if len(merged) != 1 || len(merged[0].Snapshots) != 2 || merged[0].Snapshots[1].Host != LocalHost {
		t.Fatalf("merged = %+v", merged)
	}
	if spare := probes[0].Snapshots[:2]; spare[1].Host != "" || probes[1].Snapshots[0].Host != "self" {
		t.Errorf("fold wrote through to the caller's probes: %+v", probes)
	}
}

func TestStatusNoServerErrorShowsExitCode(t *testing.T) {
	r := twoHosts()
	r.failures = map[string]hostFailure{"broken": {stderr: "sh: git: not found", err: statusExitErr(127)}}
	out, _, _ := runStatus(t, Config{BridgeHosts: []string{"lab", "broken"}}, r, "")
	if !strings.Contains(out, "  broken  probe failed — remote command exited 127\n") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "tmux server") {
		t.Errorf("snapshot probe blamed a tmux server:\n%s", out)
	}
	if got := probeState(HostProbe{Host: "x", Err: fmt.Errorf("%w: boom", probekit.ErrRemoteNoServer)}); got != "probe failed — "+probekit.ErrRemoteNoServer.Error()+": boom" {
		t.Errorf("fallback = %q", got)
	}
}

func TestStatusTailscaleCheckHostShowsURL(t *testing.T) {
	r := &statusRunner{
		probes: map[string]string{LocalHost: "I|m|u\n"},
		failures: map[string]hostFailure{
			"ts": {stdout: "# Tailscale SSH requires an additional check.\n# To authenticate, visit: https://login.tailscale.com/a/abc123\n", err: context.DeadlineExceeded},
		},
	}
	// The probe's own deadline classifies a check host; an already-expired context stands in for it.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var so, se bytes.Buffer
	Status(ctx, &so, &se, Config{BridgeHosts: []string{"ts"}}, r, StatusOptions{})
	want := "  ts     needs a Tailscale SSH check — https://login.tailscale.com/a/abc123"
	if !strings.Contains(so.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, so.String())
	}
}

func TestStatusNoProjects(t *testing.T) {
	r := &statusRunner{probes: map[string]string{LocalHost: "I|m|u\n"}}
	out, _, code := runStatus(t, Config{}, r, "")
	if code != 0 || out != "Hosts\n  local  reachable\n\nNo projects found.\n" {
		t.Errorf("code %d, output %q", code, out)
	}
}

func TestStatusSkippedRowsAreCounted(t *testing.T) {
	r := &statusRunner{probes: map[string]string{LocalHost: "I|m|u\nbogus\nW|x\n"}}
	out, _, _ := runStatus(t, Config{}, r, "")
	if !strings.Contains(out, "  local  reachable (2 unsafe rows skipped)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestStatusQuery(t *testing.T) {
	cfg := Config{
		BridgeHosts: []string{"lab"},
		Projects: map[string]Declared{
			"widget": {Remote: "https://github.com/acme/widget", Remotes: []string{"git@mirror.example.com:acme/widget.git"}},
		},
	}
	for name, q := range map[string]string{
		"key":       "github.com/acme/widget",
		"name":      "widget",
		"alias":     "mirror.example.com/acme/widget",
		"url":       "git@github.com:acme/widget.git",
		"alias url": "ssh://git@mirror.example.com/acme/widget",
	} {
		out, errOut, code := runStatus(t, cfg, twoHosts(), q)
		if code != 0 || errOut != "" || !strings.Contains(out, "github.com/acme/widget (widget)\n") {
			t.Errorf("%s: code %d stderr %q out:\n%s", name, code, errOut, out)
		}
	}
}

func TestStatusQueryMatchingNothing(t *testing.T) {
	out, errOut, code := runStatus(t, Config{}, twoHosts(), "nope/\x1b[31mnothing")
	if code != 1 || out != "" || errOut != "og project: no project matches nope/nothing\n" {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestStatusReportsInvalidDeclarations(t *testing.T) {
	cfg := Config{Projects: map[string]Declared{"bad": {Remote: "/not/a/key"}}}
	_, errOut, code := runStatus(t, cfg, twoHosts(), "")
	if code != 0 || !strings.HasPrefix(errOut, "og project: project ") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestStatusDeclaredWithoutCheckout(t *testing.T) {
	cfg := Config{
		BridgeHosts: []string{"lab"},
		Projects: map[string]Declared{"widget": {
			Remote: "github.com/acme/widget",
			Hosts:  map[string]DeclaredHost{"lab": {Path: "/srv/elsewhere"}},
		}},
	}
	r := twoHosts()
	r.probes["lab"] = "I|m-lab|u\n"
	out, _, _ := runStatus(t, cfg, r, "")
	if !strings.Contains(out, "  lab    no checkout of github.com/acme/widget on lab at /srv/elsewhere") {
		t.Errorf("output:\n%s", out)
	}
}

func TestStatusTailscaleDisabledNeverExecs(t *testing.T) {
	r := twoHosts()
	r.tailscale = `{"BackendState":"Running"}`
	runStatus(t, Config{BridgeHosts: []string{"lab"}}, r, "")
	if n := r.called("tailscale"); n != 0 {
		t.Errorf("tailscale ran %d times while disabled", n)
	}
}

const tailscaleJSON = `{"BackendState":"Running","Peer":{
 "a":{"DNSName":"lab.t.ts.net.","OS":"linux","Online":true},
 "b":{"DNSName":"argon.t.ts.net.","OS":"linux","Online":false},
 "c":{"DNSName":"nas.t.ts.net.","OS":"linux","Online":true},
 "d":{"DNSName":"mbp.t.ts.net.","OS":"macOS","Online":true}}}`

func TestStatusTailscaleOfflineAndSuggestions(t *testing.T) {
	r := twoHosts()
	r.tailscale = tailscaleJSON
	r.sshG = map[string]string{"lab": "lab.t.ts.net", "argon": "argon.t.ts.net"}
	cfg := Config{Tailscale: true, BridgeHosts: []string{"lab", "argon"}}
	out, _, code := runStatus(t, cfg, r, "")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "  argon  unreachable — ") || !strings.Contains(out, " (Tailscale reports it offline)\n") {
		t.Errorf("offline annotation missing:\n%s", out)
	}
	if strings.Contains(out, "  lab    reachable (Tailscale") {
		t.Errorf("online host annotated offline:\n%s", out)
	}
	if !strings.HasSuffix(out, "\nSuggested hosts from Tailscale (not configured): mbp, nas\n") {
		t.Errorf("suggestions line missing:\n%s", out)
	}
}

func TestStatusTailscaleFailureYieldsNoSuggestions(t *testing.T) {
	r := twoHosts()
	r.tsErr = errors.New("failed to connect to local tailscaled")
	r.tailscale = tailscaleJSON
	out, errOut, code := runStatus(t, Config{Tailscale: true, BridgeHosts: []string{"lab"}}, r, "")
	if code != 0 || errOut != "" || strings.Contains(out, "Suggested hosts") {
		t.Errorf("code %d, stderr %q, out:\n%s", code, errOut, out)
	}
	if r.called("tailscale") != 1 {
		t.Errorf("tailscale ran %d times, want 1", r.called("tailscale"))
	}
}

func TestStatusSanitizesHostileRemoteStrings(t *testing.T) {
	hostile := "\x1b[31m#(touch /tmp/pwn)'q'\"d\"a|b\nline2\x1b]0;title\x07"
	dns, err := json.Marshal(hostile + ".t.ts.net.")
	if err != nil {
		t.Fatal(err)
	}
	r := &statusRunner{
		probes: map[string]string{
			LocalHost: "I|" + hostile + "|u\n" +
				"R|/home/u/ok/.git|main|https://github.com/o/ok\n" +
				"W|/home/u/ok/.git|" + shaLocal + "|/home/u/ok|main\n" +
				"W|/home/u/ok/.git|" + shaLocal + "|/home/u/ok-a|feat" + hostile + "\n" +
				"W|/home/u/ok/.git|" + shaLocal + "|/home/u/ok" + hostile + "|dev\n" +
				"R|/home/u/evil/.git|main|" + hostile + "\n" +
				"R|/home/u/evil2/.git|" + hostile + "|https://github.com/o/evil2\n" +
				"T||@1|/home/u/ok|" + hostile + "\n" +
				"D|/home/u/ok/.git|1|1|zz" + hostile + "\n",
			"lab": "I|" + hostile + "|u\nR|/srv/" + hostile + "/.git|main|" + hostile + "\n",
		},
		failures: map[string]hostFailure{
			"down": {stdout: hostile, stderr: "Permission denied (" + hostile, err: statusExitErr(255)},
			"gone": {stdout: hostile, stderr: hostile, err: statusExitErr(255)},
		},
		heads:     map[string]string{LocalHost: "H|/home/u/ok|" + hostile + "\n"},
		tailscale: `{"BackendState":"Running","Peer":{"x":{"DNSName":` + string(dns) + `,"OS":"linux"}}}`,
		sshG:      map[string]string{},
	}
	cfg := Config{Tailscale: true, BridgeHosts: []string{"lab", "down", "gone"}}
	var so, se bytes.Buffer
	Status(context.Background(), &so, &se, cfg, r, StatusOptions{})
	assertClean(t, so.String()+se.String())

	// A Tailscale check host's URL and banner come from the remote's stdout.
	r = &statusRunner{
		probes: map[string]string{LocalHost: "I|m|u\n"},
		failures: map[string]hostFailure{
			"ts": {stdout: "Tailscale SSH requires an additional check\nTo authenticate, visit: https://x.example/a\x1b[31m'\"|#(x)\n" + hostile, err: context.DeadlineExceeded},
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	so.Reset()
	se.Reset()
	Status(ctx, &so, &se, Config{BridgeHosts: []string{"ts"}}, r, StatusOptions{})
	assertClean(t, so.String()+se.String())
	if !strings.Contains(so.String(), "https://x.example/a") {
		t.Errorf("check host lost its URL:\n%s", so.String())
	}
}

func TestStatusSanitizesConfigDerivedText(t *testing.T) {
	hostile := "a\x1b[31mb\x1b]0;t\x07c\u009bd\u202ee\nf"
	r := &statusRunner{probes: map[string]string{LocalHost: localProbe("m-local")}}
	cfg := Config{
		BridgeHosts: []string{hostile},
		Projects: map[string]Declared{
			hostile: {Remote: "github.com/acme/widget", Hosts: map[string]DeclaredHost{hostile: {Path: "/srv/widget"}}},
			"bad":   {Remote: hostile + "/not a key"},
		},
	}
	var so, se bytes.Buffer
	Status(context.Background(), &so, &se, cfg, r, StatusOptions{})
	out := so.String() + se.String()
	for _, want := range []string{"github.com/acme/widget (abcd", "unreachable — ", "invalid host alias"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%q", want, out)
		}
	}
	for i, c := range out {
		if c == 0x1b || c == 0x9b || c == 0x202e || (c < 0x20 && c != '\n') || c == 0x7f {
			t.Errorf("hostile rune %#x at %d in output:\n%q", c, i, out)
			break
		}
	}
}

func assertClean(t *testing.T, s string) {
	t.Helper()
	if s == "" {
		t.Fatal("no output to check")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == 0x1b || (c < 0x20 && c != '\n') || c == 0x7f {
			t.Errorf("control byte %#x in output:\n%q", c, s)
			break
		}
	}
	for _, bad := range []string{"#(", "'", `"`, "|"} {
		if strings.Contains(s, bad) {
			t.Errorf("output holds %q:\n%s", bad, s)
		}
	}
}
