package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Env names the registry JSON; the og-project wrapper sets it.
const Env = "TMUX_OG_PROJECTS"

// Config is the declarative layer the Nix module renders: everything optional, discovery works
// without it.
type Config struct {
	Projects    map[string]Declared `json:"projects"`
	BridgeHosts []string            `json:"bridgeHosts"` // programs.tmux-og.remote.hosts
	Tailscale   bool                `json:"tailscale"`   // programs.tmux-og.projects.tailscale.enable
}

// Declared is one programs.tmux-og.projects.<name> entry.
type Declared struct {
	Remote  string                  `json:"remote"`  // key or origin url
	Remotes []string                `json:"remotes"` // alias keys/urls folded into this project
	Hosts   map[string]DeclaredHost `json:"hosts"`
}

// DeclaredHost fixes the checkout path on a host where discovery cannot find the project.
type DeclaredHost struct {
	Path string `json:"path"`
}

// Project is one repository across every host that has (or should have) a checkout.
type Project struct {
	Key     string
	Name    string     // declared name, "" when discovered only
	Aliases []string   // normalized `remotes`, sorted, deduped, Key excluded
	Hosts   []Snapshot // one per host, in probe order; Err set for declared-but-missing / unreachable
}

// Duplicate is a branch checked out on more than one host.
type Duplicate struct {
	Branch      string
	Hosts       []string // ≥ 2, probe order
	HeadsDiffer bool
}

// ErrNoCheckout marks a declared (project, host, path) the host's probe did not find.
var ErrNoCheckout = errors.New("no checkout")

// LoadConfig reads the file Env names; unset or empty means no declarations.
func LoadConfig() (Config, error) {
	path := os.Getenv(Env)
	if path == "" {
		return Config{}, nil
	}
	return LoadConfigFile(path)
}

// LoadConfigFile reads one registry file, rejecting fields the reader does not know.
func LoadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the module's store file, set by its wrapper
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", Env, err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s %s: %w", Env, path, err)
	}
	return c, nil
}

// RemoteHosts lists the hosts to probe over ssh: the bridge hosts, then every declared host.
func (c Config) RemoteHosts() []string {
	var hosts []string
	add := func(h string) {
		if h != "" && h != LocalHost && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	for _, h := range c.BridgeHosts {
		add(h)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Projects)) {
		for _, h := range slices.Sorted(maps.Keys(c.Projects[name].Hosts)) {
			add(h)
		}
	}
	return hosts
}

// PathsFor lists the checkout paths declared on host, as extra probe candidates.
func (c Config) PathsFor(host string) []string {
	var paths []string
	for _, d := range c.Projects {
		if h, ok := d.Hosts[host]; ok && SafePath(h.Path) {
			paths = append(paths, h.Path)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// claim is a validated declared (project, host, path).
type claim struct{ key, host, path string }

// declarations validates c: canonical key per project, the alias map, and the declared paths in
// project-name, host-name order. Invalid entries are reported and skipped.
func (c Config) declarations() (names map[string]string, aliases map[string]string, claims []claim, errs []error) {
	names = map[string]string{}   // canonical key -> declared name
	aliases = map[string]string{} // alias key -> canonical key
	for _, name := range slices.Sorted(maps.Keys(c.Projects)) {
		d := c.Projects[name]
		key, err := declaredKey(name, "remote", d.Remote)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := names[key]; !dup {
			names[key] = name
		}
		for _, r := range d.Remotes {
			alias, err := declaredKey(name, "remotes entry", r)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if alias != key {
				aliases[alias] = key
			}
		}
		for _, host := range slices.Sorted(maps.Keys(d.Hosts)) {
			path := d.Hosts[host].Path
			if !SafePath(path) {
				errs = append(errs, fmt.Errorf("project %q: host %q path %q is not a safe absolute path", name, host, path))
				continue
			}
			claims = append(claims, claim{key, host, path})
		}
	}
	return names, aliases, claims, errs
}

// declaredKey normalizes a declared remote. The error never echoes raw: a declared value reaches
// the world-readable Nix store, so any invalid one may be a secret.
func declaredKey(name, what, raw string) (string, error) {
	if credentialBearing(raw) {
		return "", fmt.Errorf("project %q: %s carries credentials; declare the key (host/owner/repo) instead", name, what)
	}
	key, ok := NormalizeKey(raw)
	if !ok {
		return "", fmt.Errorf("project %q: %s is not a valid origin url or key", name, what)
	}
	return key, nil
}

var plainUserinfo = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// credentialBearing reports whether a declared remote may embed a secret. It is the Go twin of
// modules/project-credentials.nix; testdata/credential-cases.json pins both. A query or fragment
// is refused outright. An `@` is allowed only after a bare username (scp form `git@host:o/r`) or an
// ssh/git scheme and bare username (`ssh://git@host/o/r`); any other text before it, including a
// mistyped scheme (`https:/u:TOK@h/o/r`), is refused, since it could be a credential. An `@` in a URL
// path after a non-empty authority (`https://host/o/r@v1`) is not userinfo.
func credentialBearing(raw string) bool {
	if strings.ContainsAny(raw, "?#") {
		return true
	}
	before, after, hasAt := strings.Cut(raw, "@")
	if !hasAt {
		return false
	}
	if plainUserinfo.MatchString(before) {
		return strings.Contains(after, "@")
	}
	scheme, rest, hasScheme := strings.Cut(before, "://")
	if !hasScheme {
		return true
	}
	if authority, _, inPath := strings.Cut(rest, "/"); inPath {
		return authority == "" || strings.Contains(rest, ":")
	}
	switch strings.ToLower(scheme) {
	case "ssh", "git", "git+ssh", "ssh+git":
	default:
		return true
	}
	return !plainUserinfo.MatchString(rest) || strings.Contains(after, "@")
}

// entry accumulates one project while Merge walks the probes.
type entry struct {
	Project
	seen map[string]bool // hosts already holding a snapshot
}

func (e *entry) add(s Snapshot) {
	if e.seen[s.Host] {
		return
	}
	e.seen[s.Host] = true
	e.Hosts = append(e.Hosts, s)
}

// Merge folds the declarative layer and the host probes into projects, the declared path winning
// over what discovery found. A declared host matches the probe of that host or the probe it was
// folded into (HostProbe.Aliases), and its snapshot lives under that probe's host. The returned
// errors are invalid declarations; a probe's own failure lives on its snapshots and HostProbe.
func Merge(c Config, probes []HostProbe) ([]Project, []error) {
	names, aliases, claims, errs := c.declarations()
	entries := map[string]*entry{}
	get := func(key string) *entry {
		e, ok := entries[key]
		if !ok {
			e = &entry{Project: Project{Key: key, Name: names[key]}, seen: map[string]bool{}}
			entries[key] = e
		}
		return e
	}
	for key := range names {
		get(key)
	}
	for alias, key := range aliases {
		e := get(key)
		e.Aliases = append(e.Aliases, alias)
	}

	for _, probe := range probes {
		claimed := make([]bool, len(probe.Snapshots))
		var missed []claim
		for _, cl := range claims {
			if cl.host != probe.Host && !slices.Contains(probe.Aliases, cl.host) {
				continue
			}
			if i := matchPath(probe.Snapshots, claimed, cl.path); probe.Err == nil && i >= 0 {
				claimed[i] = true
				s := probe.Snapshots[i]
				s.Key, s.RepoPath = cl.key, cl.path
				get(cl.key).add(s)
				continue
			}
			missed = append(missed, cl)
		}
		// Misses land after every match, so a project declared on two aliases of one machine keeps
		// the alias that matched; when none did, the first alias in name order names the error.
		for _, cl := range missed {
			err := probe.Err
			if err == nil {
				err = fmt.Errorf("%w of %s on %s at %s", ErrNoCheckout, cl.key, cl.host, cl.path)
			}
			get(cl.key).add(Snapshot{Host: probe.Host, Key: cl.key, RepoPath: cl.path, Err: err})
		}
		if probe.Err != nil {
			continue
		}
		for i, s := range probe.Snapshots {
			if claimed[i] {
				continue
			}
			key := s.Key
			if canonical, ok := aliases[key]; ok {
				key = canonical
			}
			s.Key = key
			get(key).add(s)
		}
	}

	projects := make([]Project, 0, len(entries))
	for _, e := range entries {
		slices.Sort(e.Aliases)
		projects = append(projects, e.Project)
	}
	slices.SortFunc(projects, func(a, b Project) int { return strings.Compare(a.Key, b.Key) })
	return projects, errs
}

// matchPath returns the first unclaimed snapshot whose repo or a worktree sits at path, or -1.
func matchPath(snaps []Snapshot, claimed []bool, path string) int {
	for i, s := range snaps {
		if claimed[i] {
			continue
		}
		if s.RepoPath == path || slices.ContainsFunc(s.Worktrees, func(w Worktree) bool { return w.Path == path }) {
			return i
		}
	}
	return -1
}

// Matches reports whether query names p: its key, declared name or an alias, or any origin URL that
// normalizes to one of them.
func (p Project) Matches(query string) bool {
	if query == p.Key || (p.Name != "" && query == p.Name) || slices.Contains(p.Aliases, query) {
		return true
	}
	key, ok := NormalizeKey(query)
	return ok && (key == p.Key || slices.Contains(p.Aliases, key))
}

// Duplicates lists branches checked out on two or more hosts, sorted by branch. A detached
// worktree has no branch, a host that failed has no checkouts to compare, and a branch twice on
// one host is that host's own business.
func Duplicates(p Project) []Duplicate {
	type seen struct {
		hosts []string
		heads map[string]bool
	}
	byBranch := map[string]*seen{}
	for _, s := range p.Hosts {
		if s.Err != nil {
			continue
		}
		onHost := map[string]bool{}
		for _, w := range s.Worktrees {
			if w.Branch == "" || onHost[w.Branch] {
				continue
			}
			onHost[w.Branch] = true
			b, ok := byBranch[w.Branch]
			if !ok {
				b = &seen{heads: map[string]bool{}}
				byBranch[w.Branch] = b
			}
			b.hosts = append(b.hosts, s.Host)
			b.heads[w.Head] = true
		}
	}
	var dups []Duplicate
	for branch, b := range byBranch {
		if len(b.hosts) >= 2 {
			dups = append(dups, Duplicate{Branch: branch, Hosts: b.hosts, HeadsDiffer: len(b.heads) > 1})
		}
	}
	slices.SortFunc(dups, func(a, b Duplicate) int { return strings.Compare(a.Branch, b.Branch) })
	return dups
}
