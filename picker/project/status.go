package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/noamsto/tmux-og/picker/probekit"
)

// maxHeadsPerHost bounds how many checkouts one ls-remote call asks about.
const maxHeadsPerHost = 32

// StatusOptions selects what Status prints.
type StatusOptions struct {
	Query string // project key, declared name, alias, or any origin URL; "" lists every project
}

// hostState is how one probed host renders in the Hosts block.
type hostState struct{ host, text string }

// Status probes the local machine and every configured host concurrently and prints, per project,
// where it is checked out and how far each default branch drifted. It returns the exit code.
func Status(ctx context.Context, stdout, stderr io.Writer, c Config, r Runner, opt StatusOptions) int {
	probes, suggestions, offline := probeAll(ctx, c, r)
	states, merged := hostStates(probes, offline)

	projects, errs := Merge(c, merged)
	for _, err := range errs {
		_, _ = fmt.Fprintf(stderr, "og project: %s\n", clean(err.Error()))
	}
	if opt.Query != "" {
		projects = slices.DeleteFunc(projects, func(p Project) bool { return !p.Matches(opt.Query) })
		if len(projects) == 0 {
			_, _ = fmt.Fprintf(stderr, "og project: no project matches %s\n", clean(opt.Query))
			return 1
		}
	}
	applyHeads(ctx, r, projects)

	var b strings.Builder
	width := writeHosts(&b, states)
	if len(projects) == 0 {
		b.WriteString("\nNo projects found.\n")
	}
	for _, p := range projects {
		b.WriteByte('\n')
		writeProject(&b, p, width)
	}
	if len(suggestions) > 0 {
		names := make([]string, len(suggestions))
		for i, s := range suggestions {
			names[i] = clean(s)
		}
		fmt.Fprintf(&b, "\nSuggested hosts from Tailscale (not configured): %s\n", strings.Join(names, ", "))
	}
	_, _ = fmt.Fprint(stdout, b.String())
	return 0
}

// probeAll probes every host and, when the Tailscale source is on, asks it which configured aliases
// it knows offline and which peers no alias covers.
func probeAll(ctx context.Context, c Config, r Runner) (probes []HostProbe, suggestions, offline []string) {
	remotes := c.RemoteHosts()
	hosts := append([]string{LocalHost}, remotes...)
	probes = make([]HostProbe, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() { probes[i] = ProbeHost(ctx, r, h, c.PathsFor(h)) })
	}
	wg.Go(func() {
		peers := TailscalePeers(ctx, c.Tailscale, r)
		if len(peers) == 0 {
			return
		}
		offline, suggestions = MatchPeers(peers, remotes, func(a string) string { return SSHHostname(ctx, r, a) })
	})
	wg.Wait()
	return probes, suggestions, offline
}

// hostStates describes each probed host and returns the probes Merge should see: a host that
// reports the identity an earlier host already reported is the same machine, folded into that
// earlier probe so its declared paths still match.
func hostStates(probes []HostProbe, offline []string) ([]hostState, []HostProbe) {
	firstByIdentity := map[string]int{} // identity -> index in merged
	states := make([]hostState, len(probes))
	merged := make([]HostProbe, 0, len(probes))
	for i, p := range probes {
		st := hostState{host: p.Host}
		identity := ""
		if p.Err == nil {
			identity = p.Identity
		}
		if j, dup := firstByIdentity[identity]; dup && identity != "" {
			st.text = "same machine as " + merged[j].Host + ", merged"
			merged[j] = fold(merged[j], p)
		} else {
			st.text = probeState(p)
			if identity != "" {
				firstByIdentity[identity] = len(merged)
			}
			merged = append(merged, p)
		}
		if slices.Contains(offline, p.Host) {
			st.text += " (Tailscale reports it offline)"
		}
		states[i] = st
	}
	return states, merged
}

// fold adds dup, another alias of into's machine, to into. One machine has one checkout per repo
// path, so only the repos into lacks are added, under into's host. Both slices are cloned: into's
// are shared with the caller's probes.
func fold(into, dup HostProbe) HostProbe {
	into.Aliases = append(slices.Clone(into.Aliases), dup.Host)
	into.Snapshots = slices.Clone(into.Snapshots)
	for _, s := range dup.Snapshots {
		if slices.ContainsFunc(into.Snapshots, func(t Snapshot) bool { return t.RepoPath == s.RepoPath }) {
			continue
		}
		s.Host = into.Host
		into.Snapshots = append(into.Snapshots, s)
	}
	return into
}

func probeState(p HostProbe) string {
	err := p.Err
	switch {
	case err == nil:
		switch p.Skipped {
		case 0:
			return "reachable"
		case 1:
			return "reachable (1 unsafe row skipped)"
		}
		return "reachable (" + strconv.Itoa(p.Skipped) + " unsafe rows skipped)"
	case errors.Is(err, probekit.ErrRemoteTailscaleCheck):
		if url := probekit.TailscaleCheckURL(err); url != "" {
			return "needs a Tailscale SSH check — " + url
		}
		return "needs a Tailscale SSH check"
	case errors.Is(err, probekit.ErrRemoteHostKeyChanged):
		return "host key changed"
	case errors.Is(err, probekit.ErrRemoteNeedsAuth):
		return "needs authentication — run og remote auth " + p.Host
	case errors.Is(err, probekit.ErrRemoteNoServer):
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return "probe failed — remote command exited " + strconv.Itoa(exit.ExitCode())
		}
		return "probe failed — " + err.Error()
	}
	return "unreachable — " + err.Error()
}

// applyHeads asks each host for origin's true head of every displayed checkout, one bounded call
// per host, so a slow forge delays only its own host's note.
func applyHeads(ctx context.Context, r Runner, projects []Project) {
	reqs := map[string][]HeadReq{}
	var hosts []string
	for _, p := range projects {
		for _, s := range p.Hosts {
			if s.Err != nil || s.DefaultBranch == "" || len(reqs[s.Host]) >= maxHeadsPerHost {
				continue
			}
			if reqs[s.Host] == nil {
				hosts = append(hosts, s.Host)
			}
			reqs[s.Host] = append(reqs[s.Host], HeadReq{Path: s.RepoPath, Branch: s.DefaultBranch})
		}
	}
	heads := make([]map[string]string, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() { heads[i] = FetchHeads(ctx, r, h, reqs[h]) })
	}
	wg.Wait()
	byHost := make(map[string]map[string]string, len(hosts))
	for i, h := range hosts {
		byHost[h] = heads[i]
	}
	for _, p := range projects {
		for i := range p.Hosts {
			ApplyHeads(p.Hosts[i:i+1], byHost[p.Hosts[i].Host])
		}
	}
}

// clean is the one door remote- or config-derived text passes through before it is measured or printed.
func clean(s string) string { return probekit.SanitizeStatusText(s) }

func cells(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-cells(s)))
}

// writeHosts prints the Hosts block and returns the host column width.
func writeHosts(b *strings.Builder, states []hostState) int {
	width := 0
	names := make([]string, len(states))
	for i, st := range states {
		names[i] = clean(st.host)
		width = max(width, cells(names[i]))
	}
	b.WriteString("Hosts\n")
	for i, st := range states {
		b.WriteString("  " + pad(names[i], width) + "  " + clean(st.text) + "\n")
	}
	return width
}

type projectRow struct {
	host, path, count, detail string
	err                       string
}

func writeProject(b *strings.Builder, p Project, hostWidth int) {
	b.WriteString(clean(p.Key))
	if p.Name != "" {
		b.WriteString(" (" + clean(p.Name) + ")")
	}
	b.WriteByte('\n')

	rows := make([]projectRow, len(p.Hosts))
	pathWidth, countWidth := 0, 0
	for i, s := range p.Hosts {
		row := projectRow{host: clean(s.Host)}
		if s.Err != nil {
			row.err = clean(s.Err.Error())
		} else {
			row.path = clean(s.RepoPath)
			row.count = worktreeCount(len(s.Worktrees))
			row.detail = strings.Join(slices.DeleteFunc([]string{clean(s.DriftChip()), clean(s.DriftNote())}, func(x string) bool { return x == "" }), " · ")
			pathWidth = max(pathWidth, cells(row.path))
			countWidth = max(countWidth, cells(row.count))
		}
		rows[i] = row
	}
	for _, row := range rows {
		line := "  " + pad(row.host, hostWidth) + "  "
		if row.err != "" {
			line += row.err
		} else {
			line += pad(row.path, pathWidth) + "  " + pad(row.count, countWidth) + "  " + row.detail
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	for _, d := range Duplicates(p) {
		names := make([]string, len(d.Hosts))
		for i, h := range d.Hosts {
			names[i] = clean(h)
		}
		line := "  ⚠ " + clean(d.Branch) + " is checked out on " + joinNames(names)
		if d.HeadsDiffer {
			line += " (heads differ)"
		}
		b.WriteString(line + "\n")
	}
}

func worktreeCount(n int) string {
	if n == 1 {
		return "1 worktree"
	}
	return strconv.Itoa(n) + " worktrees"
}

func joinNames(names []string) string {
	last := len(names) - 1
	return strings.Join(names[:last], ", ") + " and " + names[last]
}
