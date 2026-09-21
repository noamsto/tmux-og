package main

// Remote CPU/memory for bridged (mirror) sessions. A mirror's local panes run
// renderers, so the local walk from their pane PIDs measures the renderer
// rather than the work — this fetches the remote's own process table and
// aggregates it with the same tree walk (#452).
//
// The ssh leg is now the LEGACY fallback: the remote's own poller stamps its
// session and the bridge daemon ships that across as @bridge_res, so a rebuilt
// remote is covered with no ssh at all (#693). What is left here serves a
// remote that has not been rebuilt — which is why the three shell workarounds
// this file documents (the separator must start with a letter, getconf rather
// than nproc, the whole process table over the wire) still live here and only
// here. Deletable once every host in @remote_bridge_hosts arms the poller:
// `tmux show -gv @og-res-tick` on its live server prints the command.

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// remoteResourcesSeparator ends the pane-PID section of the payload. It must
// begin with a letter: the remote's login shell is whatever the user set, and
// fish reads `echo --` as end-of-options and prints a blank line — which
// silently swallowed the separator and left every mirror reading 0% / 0M.
// tmux session names cannot contain a newline and no ps row is this string, so
// it splits the two sections unambiguously.
const remoteResourcesSeparator = "PSTABLE"

// remoteResourcesCmd emits, in order: the remote's online core count, one
// `<session>|<pane_pid>` line per pane, the separator, then the process table.
// Fish-safe, like every command remoteTmuxCmd builds. getconf rather than
// nproc: nproc is coreutils-only and a macOS remote has none.
var remoteResourcesCmd = `getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1; ` +
	remoteTmuxCmd(`list-panes -a -F '#{session_name}|#{pane_pid}'`) +
	`; echo ` + remoteResourcesSeparator + `; ps ` + strings.Join(psArgs, " ")

// remoteHostResources is one host's reply: per-session totals in the same
// unit the local leg produces (a raw per-core ps sum), plus the host's core
// count so the renderer can scale colour against the right machine.
type remoteHostResources struct {
	cores     int
	bySession map[string]sessionResources
}

// remoteResourceTTL is deliberately longer than resourceCacheTTL: this costs an
// ssh round-trip where the local leg costs a fork.
const remoteResourceTTL = 10 * time.Second

var remoteResourceCache struct {
	sync.Mutex
	byHost   map[string]remoteHostResources
	ts       map[string]time.Time
	inflight map[string]bool
}

// ensureRemoteResourceCacheLocked makes the cache's maps writable. Call with
// the lock held, from every writer.
func ensureRemoteResourceCacheLocked() {
	if remoteResourceCache.byHost == nil {
		remoteResourceCache.byHost = make(map[string]remoteHostResources)
	}
	if remoteResourceCache.ts == nil {
		remoteResourceCache.ts = make(map[string]time.Time)
	}
	if remoteResourceCache.inflight == nil {
		remoteResourceCache.inflight = make(map[string]bool)
	}
}

// parseRemoteResources turns remoteResourcesCmd's stdout into per-session
// totals. CPU stays the raw per-core ps sum the local leg also produces: one
// column cannot carry two units, and dividing by the remote's core count
// compressed every row on a 32-core host into a permanent "<1%".
func parseRemoteResources(stdout string) remoteHostResources {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) == 0 {
		return remoteHostResources{}
	}

	cores, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
	if cores < 1 {
		cores = 1
	}

	rootPIDs := make(map[string][]int)
	rest := lines[1:]
	psStart := len(rest)
	for i, line := range rest {
		line = strings.TrimSpace(line)
		if line == remoteResourcesSeparator {
			psStart = i + 1
			break
		}
		// Split at the LAST separator: pane_pid is the trailing field, so a
		// session name holding a `|` still parses.
		cut := strings.LastIndex(line, "|")
		if cut < 0 {
			continue
		}
		sess, pidStr := line[:cut], line[cut+1:]
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil || pid <= 0 {
			continue
		}
		rootPIDs[sess] = append(rootPIDs[sess], pid)
	}

	res := aggregateResources(rootPIDs, strings.Join(rest[psStart:], "\n"))
	return remoteHostResources{cores: cores, bySession: res}
}

// sshRemoteResources fetches one host's table. Bounded by remoteProbeTimeout
// like every other probe, and reuses a live ControlMaster when there is one.
var sshRemoteResources = func(host string) (remoteHostResources, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-T",
		host,
		"--",
		remoteResourcesCmd,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return remoteHostResources{}, err
	}
	return parseRemoteResources(stdout.String()), nil
}

// remoteResourcesFor returns what is cached and kicks a background refresh for
// any stale host. Never blocks — the caller is the 1s item rebuild. A host
// mid-flight is skipped rather than queued, so ticks cannot pile ssh processes
// up behind a slow host; a failed fetch leaves the previous values in place.
func remoteResourcesFor(hosts []string) map[string]remoteHostResources {
	remoteResourceCache.Lock()
	defer remoteResourceCache.Unlock()
	ensureRemoteResourceCacheLocked()

	out := make(map[string]remoteHostResources, len(hosts))
	for _, host := range hosts {
		if cached, ok := remoteResourceCache.byHost[host]; ok {
			out[host] = cached
		}
		if time.Since(remoteResourceCache.ts[host]) < remoteResourceTTL || remoteResourceCache.inflight[host] {
			continue
		}
		remoteResourceCache.inflight[host] = true
		// Resolve the fetcher now, not inside the goroutine: it is a var for
		// test substitution, and reading it seconds later races the swap.
		fetch := sshRemoteResources
		go func(host string) {
			res, err := fetch(host)
			remoteResourceCache.Lock()
			defer remoteResourceCache.Unlock()
			// Every writer asserts the maps itself: this one lands seconds
			// after it was spawned, and cannot assume the state it saw then.
			ensureRemoteResourceCacheLocked()
			remoteResourceCache.inflight[host] = false
			// Stamp the attempt either way: a host that cannot answer must not
			// be retried every tick.
			remoteResourceCache.ts[host] = time.Now()
			if err == nil {
				remoteResourceCache.byHost[host] = res
			}
		}(host)
	}
	return out
}

// bridgeResStaleAfter is three of the daemon's 30s unchanged-row refreshes, so
// one missed refresh never flaps a healthy mirror down onto the ssh leg.
const bridgeResStaleAfter = 90 * time.Second

// bridgeResMaxLen mirrors the daemon's sessionResMaxLen.
const bridgeResMaxLen = 128

// bridgeResFutureSlack absorbs the second the two clocks may disagree by from
// granularity alone. Past it the stamp is a clock jump, not a fresh
// measurement, and trusting it would pin the row fresh for as long as the jump
// lasts.
const bridgeResFutureSlack = 2 * time.Second

// parseBridgeRes reads "<cpu> <mem> <cores> <epoch> <agents>". The epoch is
// the daemon's own local clock, so it compares against ours directly. agents is
// the agent commands the remote found in the session's tree, comma-joined, or
// "-" for none. A zero row is a real measurement — an idle remote session — and
// must parse; absent is the other state entirely, and its caller falls back.
func parseBridgeRes(v string, now int64) (sessionResources, float64, bool) {
	// The daemon's own cap: any local writer can set a session option, and the
	// agents list would otherwise drive an unbounded merge on every rebuild.
	if len(v) > bridgeResMaxLen {
		return sessionResources{}, 0, false
	}
	f := strings.Fields(v)
	if len(f) != 5 {
		return sessionResources{}, 0, false
	}
	cpu, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return sessionResources{}, 0, false
	}
	mem, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return sessionResources{}, 0, false
	}
	cores, err := strconv.ParseFloat(f[2], 64)
	if err != nil {
		return sessionResources{}, 0, false
	}
	stamped, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return sessionResources{}, 0, false
	}
	age := now - stamped
	if age > int64(bridgeResStaleAfter/time.Second) || age < -int64(bridgeResFutureSlack/time.Second) {
		return sessionResources{}, 0, false
	}
	var agents []string
	if f[4] != "-" {
		agents = strings.Split(f[4], ",")
	}
	return sessionResources{cpuPct: cpu, memMB: mem, agentCmds: agents}, cores, true
}

// mergeRemoteResources overwrites each mirror session's CPU/mem with its remote
// counterpart's: from the daemon's @bridge_res stamp where there is a fresh
// one, and otherwise from the legacy ssh leg. A mirror neither covers marks the
// row unknown, so it renders "-" rather than its local figures, which measure
// the renderer.
func mergeRemoteResources(sessions []sessionData) {
	now := time.Now().Unix()
	covered := make([]bool, len(sessions))
	for i := range sessions {
		if sessions[i].bridgeHost == "" {
			continue
		}
		r, cores, ok := parseBridgeRes(sessions[i].bridgeRes, now)
		if !ok {
			continue
		}
		sessions[i].cpuPct = r.cpuPct
		sessions[i].memMB = r.memMB
		sessions[i].cores = cores
		mergeAgentCmds(&sessions[i], r.agentCmds)
		covered[i] = true
	}

	// Only hosts still owing an answer are worth probing. On a fleet of rebuilt
	// remotes this is empty and the steady state costs nothing at all — not even
	// bridgeSessionNames' local round-trip.
	hosts := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	for i := range sessions {
		h := sessions[i].bridgeHost
		if h == "" || covered[i] || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return
	}

	byHost := remoteResourcesFor(hosts)
	remoteSess := bridgeSessionNames()
	for i := range sessions {
		host := sessions[i].bridgeHost
		if host == "" || covered[i] {
			continue
		}
		res, ok := byHost[host]
		if !ok {
			sessions[i].resUnknown = true
			continue
		}
		r, ok := res.bySession[remoteSess[sessions[i].name]]
		if !ok {
			sessions[i].resUnknown = true
			continue
		}
		sessions[i].cpuPct = r.cpuPct
		sessions[i].memMB = r.memMB
		sessions[i].cores = float64(res.cores)
		mergeAgentCmds(&sessions[i], r.agentCmds)
	}
}

// parseBridgeSessionNames maps each local mirror session to the remote session
// it mirrors, from the same `list-sessions` output parseBridgeSessions reads.
func parseBridgeSessionNames(out string) map[string]string {
	res := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		res[parts[0]] = parts[2]
	}
	return res
}

var bridgeNameCache struct {
	sync.Mutex
	names map[string]string
	ts    time.Time
}

// bridgeSessionNames caches the mapping for as long as the resources it keys.
// It changes only when a bridge is opened or torn down, and the call is a local
// round-trip on the 1s item rebuild, which is the picker's scarce resource.
func bridgeSessionNames() map[string]string {
	bridgeNameCache.Lock()
	defer bridgeNameCache.Unlock()
	if bridgeNameCache.names != nil && time.Since(bridgeNameCache.ts) < remoteResourceTTL {
		return bridgeNameCache.names
	}
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}|#{@bridge_host}|#{@bridge_session}").Output()
	if err != nil {
		return bridgeNameCache.names
	}
	bridgeNameCache.names = parseBridgeSessionNames(string(out))
	bridgeNameCache.ts = time.Now()
	return bridgeNameCache.names
}
