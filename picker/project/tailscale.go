package project

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// TailscaleTimeout bounds `tailscale status --json`: with tailscaled absent it fails only after 2s.
const TailscaleTimeout = 2 * time.Second

// Peer is a tailnet node a host could be reached on.
type Peer struct {
	Name   string // DNSName's first label, lower-cased; [a-z0-9-]+
	Online bool   // never proves ssh works (Tailscale SSH check mode hangs on an online peer)
	OS     string
}

type tailscaleStatus struct {
	BackendState string
	Peer         map[string]struct {
		DNSName    string
		OS         string
		Online     bool
		ShareeNode bool
	}
}

// ParseTailscaleStatus reads `tailscale status --json`. false means no data: invalid JSON, or a
// BackendState other than Running.
func ParseTailscaleStatus(data []byte) ([]Peer, bool) {
	var st tailscaleStatus
	if json.Unmarshal(data, &st) != nil || st.BackendState != "Running" {
		return nil, false
	}
	var peers []Peer
	for _, p := range st.Peer {
		name := firstLabel(p.DNSName)
		if (p.OS != "linux" && p.OS != "macOS") || p.ShareeNode || !validPeerName(name) {
			continue
		}
		peers = append(peers, Peer{Name: name, Online: p.Online, OS: p.OS})
	}
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return peers, true
}

// TailscalePeers lists tailnet peers, or nil when disabled (no process is run) or on any failure:
// no tailscale source is never a user-facing error.
func TailscalePeers(ctx context.Context, enabled bool, r Runner) []Peer {
	if !enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, TailscaleTimeout)
	defer cancel()
	stdout, _, err := r.Run(ctx, []string{"tailscale", "status", "--json"})
	if err != nil {
		return nil
	}
	peers, _ := ParseTailscaleStatus([]byte(stdout))
	return peers
}

// SSHHostname returns the first label of the hostname `ssh -G alias` resolves, lower-cased, or ""
// on any failure or an alias ProbeHost would refuse.
func SSHHostname(ctx context.Context, r Runner, alias string) string {
	if !validHostAlias(alias) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, TailscaleTimeout)
	defer cancel()
	stdout, _, err := r.Run(ctx, []string{"ssh", "-G", alias})
	if err != nil {
		return ""
	}
	for line := range strings.Lines(stdout) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "hostname" {
			return firstLabel(f[1])
		}
	}
	return ""
}

// MatchPeers pairs aliases with peers by the first label of the hostname each resolves to. offline
// holds the matched aliases (input order) whose peer is not online; suggestions the sorted names of
// peers no alias matched.
func MatchPeers(peers []Peer, aliases []string, hostname func(alias string) string) (offline, suggestions []string) {
	online := make(map[string]bool, len(peers))
	for _, p := range peers {
		online[p.Name] = p.Online
	}
	matched := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		name := hostname(a)
		up, ok := online[name]
		if !ok {
			continue
		}
		matched[name] = true
		if !up {
			offline = append(offline, a)
		}
	}
	for _, p := range peers {
		if !matched[p.Name] {
			suggestions = append(suggestions, p.Name)
		}
	}
	slices.Sort(suggestions)
	return offline, suggestions
}

func firstLabel(dns string) string {
	label, _, _ := strings.Cut(dns, ".")
	return strings.ToLower(label)
}

func validPeerName(n string) bool {
	return n != "" && allBytes(n, func(c byte) bool {
		return c >= 'a' && c <= 'z' || isDigit(c) || c == '-'
	})
}
