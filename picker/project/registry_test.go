package project

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	keyWidget = "github.com/acme/widget"
	keyGadget = "github.com/acme/gadget"
)

func wt(path, branch, head string) Worktree {
	return Worktree{Path: path, Branch: branch, Head: head}
}

func snap(host, key, repo string, wts ...Worktree) Snapshot {
	return Snapshot{Host: host, Key: key, RepoPath: repo, DefaultBranch: "main", Worktrees: wts}
}

func answered(host string, snaps ...Snapshot) HostProbe {
	return HostProbe{Host: host, Snapshots: snaps}
}

func hostsOf(p Project) []string {
	var hosts []string
	for _, s := range p.Hosts {
		hosts = append(hosts, s.Host)
	}
	return hosts
}

func findProject(t *testing.T, ps []Project, key string) Project {
	t.Helper()
	for _, p := range ps {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("no project %q in %v", key, ps)
	return Project{}
}

func TestMergeDeclarativePathWinsOverKey(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/widget"}}},
	}}
	// The checkout at the declared path reports a different key (an ssh alias git cannot see).
	probes := []HostProbe{answered("lab",
		snap("lab", "gh-work/acme/widget", "/srv/widget", wt("/srv/widget", "main", "")),
	)}

	ps, errs := Merge(c, probes)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(ps) != 1 || ps[0].Key != keyWidget {
		t.Fatalf("projects = %+v, want one %s", ps, keyWidget)
	}
	if got := ps[0].Hosts; len(got) != 1 || got[0].Err != nil || got[0].RepoPath != "/srv/widget" {
		t.Fatalf("hosts = %+v", got)
	}
	if ps[0].Name != "widget" {
		t.Errorf("Name = %q", ps[0].Name)
	}
}

func TestMergeDeclaredPathMatchesWorktreeAndBecomesRepoPath(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/widget-feat"}}},
	}}
	probes := []HostProbe{answered("lab",
		snap("lab", "other.example/x/y", "/srv/widget", wt("/srv/widget", "main", ""), wt("/srv/widget-feat", "feat/x", "")),
	)}

	ps, errs := Merge(c, probes)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	got := findProject(t, ps, keyWidget).Hosts
	if len(got) != 1 || got[0].RepoPath != "/srv/widget-feat" || got[0].Err != nil {
		t.Fatalf("hosts = %+v", got)
	}
	if len(findProject(t, ps, keyWidget).Hosts[0].Worktrees) != 2 {
		t.Errorf("worktrees lost: %+v", got[0])
	}
}

func TestMergeAliasKeysFoldIntoOneProject(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {
			Remote:  "git@github.com:acme/widget.git",
			Remotes: []string{"https://gh-work.example/acme/widget.git", "gh-work.example/acme/widget", "git@github.com:acme/widget"},
		},
	}}
	probes := []HostProbe{
		answered("local", snap("local", keyWidget, "/home/u/widget")),
		answered("lab", snap("lab", "gh-work.example/acme/widget", "/srv/widget")),
	}

	ps, errs := Merge(c, probes)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(ps) != 1 {
		t.Fatalf("projects = %+v, want one", ps)
	}
	p := ps[0]
	if p.Key != keyWidget {
		t.Errorf("Key = %q", p.Key)
	}
	if want := []string{"gh-work.example/acme/widget"}; !reflect.DeepEqual(p.Aliases, want) {
		t.Errorf("Aliases = %v, want %v (deduped, Key excluded)", p.Aliases, want)
	}
	if got, want := hostsOf(p), []string{"local", "lab"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hosts = %v, want probe order %v", got, want)
	}
	if p.Hosts[1].Key != keyWidget {
		t.Errorf("alias snapshot Key = %q, want canonical", p.Hosts[1].Key)
	}
}

func TestMergeAliasesSorted(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Remotes: []string{"z.example/a/b", "a.example/a/b"}},
	}}
	ps, _ := Merge(c, nil)
	if want := []string{"a.example/a/b", "z.example/a/b"}; !reflect.DeepEqual(ps[0].Aliases, want) {
		t.Errorf("Aliases = %v, want %v", ps[0].Aliases, want)
	}
}

func TestMergeDiscoveredOnlyHostAppears(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/widget"}}},
	}}
	probes := []HostProbe{
		answered("local", snap("local", keyWidget, "/home/u/widget"), snap("local", keyGadget, "/home/u/gadget")),
		answered("lab", snap("lab", keyWidget, "/srv/widget")),
	}

	ps, errs := Merge(c, probes)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(ps) != 2 || ps[0].Key != keyGadget || ps[1].Key != keyWidget {
		t.Fatalf("projects = %+v, want sorted by key: gadget, widget", ps)
	}
	if g := findProject(t, ps, keyGadget); g.Name != "" || !reflect.DeepEqual(hostsOf(g), []string{"local"}) {
		t.Errorf("gadget = %+v", g)
	}
	if got, want := hostsOf(findProject(t, ps, keyWidget)), []string{"local", "lab"}; !reflect.DeepEqual(got, want) {
		t.Errorf("widget hosts = %v, want %v", got, want)
	}
}

func TestMergeFirstSnapshotPerHostWins(t *testing.T) {
	probes := []HostProbe{answered("lab",
		snap("lab", keyWidget, "/srv/first"),
		snap("lab", keyWidget, "/srv/second"),
	)}
	ps, _ := Merge(Config{}, probes)
	if len(ps) != 1 || len(ps[0].Hosts) != 1 || ps[0].Hosts[0].RepoPath != "/srv/first" {
		t.Fatalf("projects = %+v", ps)
	}
}

func TestMergeDeclaredButMissingCheckout(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"argon": {Path: "/x"}}},
	}}
	ps, errs := Merge(c, []HostProbe{answered("argon")})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	got := findProject(t, ps, keyWidget).Hosts
	if len(got) != 1 {
		t.Fatalf("hosts = %+v", got)
	}
	s := got[0]
	if !errors.Is(s.Err, ErrNoCheckout) {
		t.Fatalf("Err = %v, want ErrNoCheckout", s.Err)
	}
	if want := "no checkout of github.com/acme/widget on argon at /x"; s.Err.Error() != want {
		t.Errorf("Err = %q, want %q", s.Err, want)
	}
	if s.Host != "argon" || s.Key != keyWidget || s.RepoPath != "/x" {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestMergeDeclaredPathWinsOverDiscoveredCheckoutElsewhere(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/declared"}}},
	}}
	ps, _ := Merge(c, []HostProbe{answered("lab", snap("lab", keyWidget, "/srv/found"))})
	got := findProject(t, ps, keyWidget).Hosts
	if len(got) != 1 || !errors.Is(got[0].Err, ErrNoCheckout) || got[0].RepoPath != "/srv/declared" {
		t.Fatalf("hosts = %+v, want the declared path reported missing", got)
	}
}

func TestMergeProjectDeclaredOnTwoAliasesKeepsTheMatch(t *testing.T) {
	folded := func(snaps ...Snapshot) []HostProbe {
		return []HostProbe{{Host: "lab", Aliases: []string{"lab-ts"}, Snapshots: snaps}}
	}
	both := Config{Projects: map[string]Declared{"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{
		"lab":    {Path: "/srv/wrong"},
		"lab-ts": {Path: "/srv/widget"},
	}}}}

	ps, _ := Merge(both, folded(snap("lab", keyWidget, "/srv/widget")))
	got := findProject(t, ps, keyWidget).Hosts
	if len(got) != 1 || got[0].Err != nil || got[0].Host != "lab" || got[0].RepoPath != "/srv/widget" {
		t.Fatalf("hosts = %+v, want the alias's match under lab", got)
	}

	// The error names the claim's own host; when every alias misses, the first in name order.
	only := Config{Projects: map[string]Declared{"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{
		"lab-ts": {Path: "/srv/widget"},
	}}}}
	for cfg, want := range map[*Config]string{
		&only: "no checkout of github.com/acme/widget on lab-ts at /srv/widget",
		&both: "no checkout of github.com/acme/widget on lab at /srv/wrong",
	} {
		ps, _ = Merge(*cfg, folded())
		got = findProject(t, ps, keyWidget).Hosts
		if len(got) != 1 || got[0].Host != "lab" || !errors.Is(got[0].Err, ErrNoCheckout) || got[0].Err.Error() != want {
			t.Errorf("hosts = %+v, want one lab snapshot erring %q", got, want)
		}
	}
}

func TestMergeDeclaredHostUnreachable(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/widget"}}},
	}}
	probeErr := errors.New("remote unreachable: probe timed out")
	ps, errs := Merge(c, []HostProbe{{Host: "lab", Err: probeErr}})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	got := findProject(t, ps, keyWidget).Hosts
	if len(got) != 1 || !errors.Is(got[0].Err, probeErr) || got[0].Host != "lab" || got[0].Key != keyWidget || got[0].RepoPath != "/srv/widget" {
		t.Fatalf("hosts = %+v", got)
	}
}

func TestMergeUnreachableHostContributesNoDiscovery(t *testing.T) {
	probes := []HostProbe{{Host: "lab", Err: errors.New("down"), Snapshots: []Snapshot{snap("lab", keyWidget, "/srv/widget")}}}
	ps, _ := Merge(Config{}, probes)
	if len(ps) != 0 {
		t.Fatalf("projects = %+v, want none", ps)
	}
}

func TestMergeHostWithoutProbeContributesNothing(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"widget": {Remote: keyWidget, Hosts: map[string]DeclaredHost{"halo": {Path: "/srv/widget"}}},
	}}
	ps, errs := Merge(c, []HostProbe{answered("local", snap("local", keyWidget, "/home/u/widget"))})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if got, want := hostsOf(findProject(t, ps, keyWidget)), []string{"local"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
}

func TestMergeInvalidDeclarationsReportedAndSkipped(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"bad-remote": {Remote: "../not-a-key"},
		"good": {
			Remote:  keyWidget,
			Remotes: []string{"file:///x", "gh-work.example/acme/widget"},
			Hosts: map[string]DeclaredHost{
				"lab":  {Path: "relative/path"},
				"halo": {Path: "/srv/ok"},
			},
		},
	}}
	ps, errs := Merge(c, []HostProbe{answered("halo", snap("halo", keyWidget, "/srv/ok"))})

	if len(ps) != 1 || ps[0].Key != keyWidget {
		t.Fatalf("projects = %+v", ps)
	}
	if want := []string{"gh-work.example/acme/widget"}; !reflect.DeepEqual(ps[0].Aliases, want) {
		t.Errorf("Aliases = %v, want %v", ps[0].Aliases, want)
	}
	if got := hostsOf(ps[0]); !reflect.DeepEqual(got, []string{"halo"}) {
		t.Errorf("hosts = %v, want only halo", got)
	}
	if len(errs) != 3 {
		t.Fatalf("errs = %v, want 3", errs)
	}
	joined := errs[0].Error() + "\n" + errs[1].Error() + "\n" + errs[2].Error()
	for _, want := range []string{"bad-remote", "remotes entry is not a valid origin url or key", "relative/path"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors %q do not name %q", joined, want)
		}
	}
}

func TestMergeRejectsCredentialedRemotes(t *testing.T) {
	const secret = "s3cr3t-token"
	for name, raw := range map[string]string{
		"https oauth2 userinfo": "https://oauth2:" + secret + "@gitlab.com/o/r.git",
		"https token only":      "https://" + secret + "@github.com/o/r",
		"http userinfo":         "http://" + secret + "@github.com/o/r",
		"ssh user:password":     "ssh://git:" + secret + "@host.example/o/r",
		"git user:password":     "git://user:" + secret + "@host.example/o/r",
		"https without path":    "https://" + secret + "@github.com",
		"scp user:password":     "user:" + secret + "@host.example:o/r",
	} {
		for field, d := range map[string]Declared{
			"remote":        {Remote: raw},
			"remotes entry": {Remote: keyWidget, Remotes: []string{raw}},
		} {
			ps, errs := Merge(Config{Projects: map[string]Declared{"widget": d}}, nil)
			if len(errs) != 1 {
				t.Errorf("%s/%s: errs = %v, want 1", name, field, errs)
				continue
			}
			want := `project "widget": ` + field + " carries credentials; declare the key (host/owner/repo) instead"
			if errs[0].Error() != want {
				t.Errorf("%s/%s: error %q, want %q", name, field, errs[0], want)
			}
			for _, p := range ps {
				if strings.Contains(p.Key+strings.Join(p.Aliases, " "), secret) {
					t.Errorf("%s/%s: project %+v holds the secret", name, field, p)
				}
			}
		}
	}
}

func TestMergeAcceptsUsernameOnlyRemotes(t *testing.T) {
	for _, raw := range []string{
		"ssh://git@github.com/acme/widget.git",
		"git@github.com:acme/widget.git",
		"git://github.com/acme/widget",
	} {
		ps, errs := Merge(Config{Projects: map[string]Declared{"widget": {Remote: raw}}}, nil)
		if len(errs) != 0 || len(ps) != 1 || ps[0].Key != keyWidget {
			t.Errorf("%q: projects %+v, errs %v", raw, ps, errs)
		}
	}
}

func TestMergeInvalidRemoteErrorNeverEchoesTheValue(t *testing.T) {
	const secret = "s3cr3t-token"
	for _, raw := range []string{"ftp://" + secret + "@host.example", "../" + secret, "file:///" + secret} {
		for field, d := range map[string]Declared{
			"remote":        {Remote: raw},
			"remotes entry": {Remote: keyWidget, Remotes: []string{raw}},
		} {
			_, errs := Merge(Config{Projects: map[string]Declared{"widget": d}}, nil)
			if len(errs) != 1 || strings.Contains(errs[0].Error(), secret) {
				t.Errorf("%q/%s: errs = %v", raw, field, errs)
			}
		}
	}
	_, errs := Merge(Config{Projects: map[string]Declared{"widget": {Remote: "../plain"}}}, nil)
	want := `project "widget": remote is not a valid origin url or key`
	if len(errs) != 1 || errs[0].Error() != want {
		t.Errorf("errs = %v, want %q", errs, want)
	}
}

func TestCredentialBearingCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/credential-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Value   string `json:"value"`
		Refused bool   `json:"refused"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for i, c := range cases {
		if got := credentialBearing(c.Value); got != c.Refused {
			t.Errorf("case %d %q: credentialBearing = %v, want %v", i, c.Value, got, c.Refused)
		}
	}
}

func TestMergeSortsProjectsByKey(t *testing.T) {
	probes := []HostProbe{answered("local",
		snap("local", "github.com/z/z", "/z"),
		snap("local", "github.com/a/a", "/a"),
	)}
	ps, _ := Merge(Config{}, probes)
	if len(ps) != 2 || ps[0].Key != "github.com/a/a" {
		t.Fatalf("projects = %+v", ps)
	}
}

func TestDuplicates(t *testing.T) {
	const sha1, sha2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	tests := []struct {
		name  string
		hosts []Snapshot
		want  []Duplicate
	}{
		{
			name: "same branch on two hosts, heads differ",
			hosts: []Snapshot{
				snap("local", keyWidget, "/l", wt("/l", "main", sha1), wt("/l-x", "feat/x", sha1)),
				snap("lab", keyWidget, "/s", wt("/s", "main", sha1), wt("/s-x", "feat/x", sha2)),
			},
			want: []Duplicate{
				{Branch: "feat/x", Hosts: []string{"local", "lab"}, HeadsDiffer: true},
				{Branch: "main", Hosts: []string{"local", "lab"}},
			},
		},
		{
			name: "three hosts, sorted by branch, one head differs",
			hosts: []Snapshot{
				snap("a", keyWidget, "/a", wt("/a", "b", sha1)),
				snap("b", keyWidget, "/b", wt("/b", "b", sha1)),
				snap("c", keyWidget, "/c", wt("/c", "b", sha2)),
			},
			want: []Duplicate{{Branch: "b", Hosts: []string{"a", "b", "c"}, HeadsDiffer: true}},
		},
		{
			name: "detached worktrees are ignored",
			hosts: []Snapshot{
				snap("local", keyWidget, "/l", wt("/l", "", sha1)),
				snap("lab", keyWidget, "/s", wt("/s", "", sha1)),
			},
		},
		{
			name: "same branch twice on one host is not a duplicate",
			hosts: []Snapshot{
				snap("local", keyWidget, "/l", wt("/l", "main", sha1), wt("/l2", "main", sha1)),
				snap("lab", keyWidget, "/s", wt("/s", "other", sha1)),
			},
		},
		{
			name: "errored hosts are ignored",
			hosts: []Snapshot{
				snap("local", keyWidget, "/l", wt("/l", "main", sha1)),
				{Host: "lab", Key: keyWidget, RepoPath: "/s", Worktrees: []Worktree{wt("/s", "main", sha1)}, Err: ErrNoCheckout},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Duplicates(Project{Key: keyWidget, Hosts: tt.hosts})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Duplicates = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	p := Project{Key: keyWidget, Name: "widget-app", Aliases: []string{"gh-work.example/acme/widget"}}
	tests := []struct {
		query string
		want  bool
	}{
		{keyWidget, true},
		{"widget-app", true},
		{"gh-work.example/acme/widget", true},
		{"git@github.com:acme/widget.git", true},
		{"https://github.com/acme/widget", true},
		{"git@gh-work.example:acme/widget.git", true},
		{"widget", false},
		{"", false},
		{"github.com/acme/gadget", false},
		{"https://github.com/other/widget", false},
	}
	for _, tt := range tests {
		if got := p.Matches(tt.query); got != tt.want {
			t.Errorf("Matches(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
	if (Project{Key: keyWidget}).Matches("") {
		t.Error("an undeclared project must not match the empty name")
	}
}

func TestRemoteHosts(t *testing.T) {
	c := Config{
		BridgeHosts: []string{"lab", "", LocalHost, "lab", "nas"},
		Projects: map[string]Declared{
			"zeta":  {Hosts: map[string]DeclaredHost{"argon": {Path: "/z"}, "nas": {Path: "/z"}, LocalHost: {Path: "/z"}}},
			"alpha": {Hosts: map[string]DeclaredHost{"halo": {Path: "/a"}, "argon": {Path: "/a"}}},
		},
	}
	want := []string{"lab", "nas", "argon", "halo"}
	if got := c.RemoteHosts(); !reflect.DeepEqual(got, want) {
		t.Errorf("RemoteHosts = %v, want %v", got, want)
	}
	if got := (Config{}).RemoteHosts(); len(got) != 0 {
		t.Errorf("zero Config RemoteHosts = %v", got)
	}
}

func TestPathsFor(t *testing.T) {
	c := Config{Projects: map[string]Declared{
		"a": {Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/b"}, "halo": {Path: "/h"}}},
		"b": {Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/a"}}},
		"c": {Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/a"}}},
		"d": {Hosts: map[string]DeclaredHost{"lab": {Path: "/srv/bad path"}}},
	}}
	if got, want := c.PathsFor("lab"), []string{"/srv/a", "/srv/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PathsFor(lab) = %v, want %v", got, want)
	}
	if got := c.PathsFor("nowhere"); len(got) != 0 {
		t.Errorf("PathsFor(nowhere) = %v", got)
	}
}

func TestLoadConfigEnvUnsetIsZero(t *testing.T) {
	t.Setenv(Env, "")
	if err := os.Unsetenv(Env); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(c, Config{}) {
		t.Errorf("config = %+v, want zero", c)
	}
}

func TestLoadConfigReadsEnvFile(t *testing.T) {
	path := writeRegistry(t, `{"projects":{"widget":{"remote":"github.com/acme/widget","remotes":["a.example/x/y"],"hosts":{"lab":{"path":"/srv/widget"}}}},"bridgeHosts":["lab"],"tailscale":true}`)
	t.Setenv(Env, path)
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := Config{
		Projects: map[string]Declared{"widget": {
			Remote:  keyWidget,
			Remotes: []string{"a.example/x/y"},
			Hosts:   map[string]DeclaredHost{"lab": {Path: "/srv/widget"}},
		}},
		BridgeHosts: []string{"lab"},
		Tailscale:   true,
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config = %+v, want %+v", c, want)
	}
}

func TestLoadConfigFileRejectsUnknownField(t *testing.T) {
	path := writeRegistry(t, `{"projects":{},"bridgeHost":["lab"]}`)
	if _, err := LoadConfigFile(path); err == nil || !strings.Contains(err.Error(), "bridgeHost") {
		t.Fatalf("err = %v, want unknown-field error naming bridgeHost", err)
	}
}

func TestLoadConfigFileErrors(t *testing.T) {
	if _, err := LoadConfigFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: want error")
	}
	if _, err := LoadConfigFile(writeRegistry(t, `{"projects":`)); err == nil {
		t.Error("truncated JSON: want error")
	}
}

func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
