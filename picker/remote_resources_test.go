package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAggregateResourcesAgentCommandsFromTree(t *testing.T) {
	// 100 is a pane whose foreground command is a shell — the tmux-remux restore
	// shape, where the agent is a child of a non-interactive shell, shares its
	// process group, and so is invisible to pane_current_command.
	ps := strings.Join([]string{
		"  PID  PPID %CPU   RSS COMMAND",
		"  100     1  0.0   512 fish",
		"  200   100  1.0 65536 claude",
		"  300   200  0.0  1024 node",
		"  400     1  0.0   512 /nix/store/x/.claude-wrapped",
		"  500     1  0.0   512 /usr/bin/python3",
	}, "\n")

	got := aggregateResources(map[string][]int{"restored": {100}, "wrapped": {400}, "plain": {500}}, ps)
	for _, tc := range []struct {
		sess string
		want []string
	}{
		{"restored", []string{"claude"}},
		{"wrapped", []string{"claude"}},
		{"plain", nil},
	} {
		if !slices.Equal(got[tc.sess].agentCmds, tc.want) {
			t.Errorf("%s agentCmds = %v, want %v", tc.sess, got[tc.sess].agentCmds, tc.want)
		}
	}
	if got["restored"].cpuPct != 1.0 || got["restored"].memMB != 65.5 {
		t.Errorf("restored = %v%% / %v MiB, want the whole tree's 1.0 / 65.5",
			got["restored"].cpuPct, got["restored"].memMB)
	}
}

func TestMergeAgentCmdsKeepsPaneCommandsFirst(t *testing.T) {
	sessions := []sessionData{{name: "prdash", procs: []string{"fish"}}}
	mergeResources(sessions, map[string]sessionResources{
		"prdash": {cpuPct: 1, memMB: 2, agentCmds: []string{"claude"}},
	})
	if want := []string{"fish", "claude"}; !slices.Equal(sessions[0].procs, want) {
		t.Errorf("procs = %v, want %v", sessions[0].procs, want)
	}
	// Idempotent: a pane command that already named the agent is not doubled.
	mergeResources(sessions, map[string]sessionResources{"prdash": {agentCmds: []string{"claude"}}})
	if want := []string{"fish", "claude"}; !slices.Equal(sessions[0].procs, want) {
		t.Errorf("procs after a re-merge = %v, want %v", sessions[0].procs, want)
	}
}

func TestParseRemoteResources(t *testing.T) {
	payload := strings.Join([]string{
		"8",
		"work|100",
		"work|200",
		"idle|400",
		remoteResourcesSeparator,
		"  PID  PPID %CPU   RSS",
		"  100     1 40.0  1024",
		"  200     1 40.0  1024",
		"  300   200  0.0  2048",
		"  400     1  0.0  1024",
	}, "\n")

	got := parseRemoteResources(payload)
	if got.cores != 8 {
		t.Fatalf("cores = %d, want 8", got.cores)
	}
	// The raw per-core sum, undivided: the local leg reports the same unit,
	// and one column cannot carry two.
	if got.bySession["work"].cpuPct != 80.0 {
		t.Errorf("work cpu = %v, want 80.0", got.bySession["work"].cpuPct)
	}
	if got.bySession["work"].memMB != 4.0 {
		t.Errorf("work mem = %v MiB, want 4.0", got.bySession["work"].memMB)
	}
	if got.bySession["idle"].cpuPct != 0 {
		t.Errorf("idle cpu = %v, want 0", got.bySession["idle"].cpuPct)
	}
}

func TestParseRemoteResourcesDegenerate(t *testing.T) {
	// An unreachable host's empty stdout, and a core count that did not parse:
	// neither may divide by zero or panic.
	for _, payload := range []string{"", "\n", "not-a-number\n" + remoteResourcesSeparator} {
		got := parseRemoteResources(payload)
		if got.cores < 1 {
			t.Errorf("payload %q gave cores = %d, want >= 1", payload, got.cores)
		}
	}
}

func TestParseRemoteResourcesSessionNameWithPipe(t *testing.T) {
	got := parseRemoteResources(strings.Join([]string{
		"1", "we|ird|100", remoteResourcesSeparator,
		"  100     1  5.0  1024",
	}, "\n"))
	if got.bySession["we|ird"].cpuPct != 5.0 {
		t.Errorf("got %v, want the pane counted under the full name", got.bySession)
	}
}

func TestParseBridgeSessionNames(t *testing.T) {
	got := parseBridgeSessionNames(strings.Join([]string{
		"tmux-og||",
		"tp-g6-work|tp-g6|work",
		"lab-main|lab|main",
	}, "\n"))
	if len(got) != 2 {
		t.Fatalf("got %v, want only the two mirrors", got)
	}
	if got["tp-g6-work"] != "work" || got["lab-main"] != "main" {
		t.Errorf("got %v", got)
	}
}

func TestRemoteResourcesCmdFishSafe(t *testing.T) {
	if strings.Contains(remoteResourcesCmd, "td=") || strings.Contains(remoteResourcesCmd, "; t=") {
		t.Fatalf("must not use shell assignments (fish-incompatible): %q", remoteResourcesCmd)
	}
	for _, want := range []string{"getconf _NPROCESSORS_ONLN", "list-panes -a", "TMUX_TMPDIR=/tmp ", "ps -Ao"} {
		if !strings.Contains(remoteResourcesCmd, want) {
			t.Errorf("missing %q in %q", want, remoteResourcesCmd)
		}
	}
	// nproc is coreutils-only; a macOS remote has none.
	if strings.Contains(remoteResourcesCmd, "nproc") {
		t.Errorf("must not use nproc: %q", remoteResourcesCmd)
	}
	// The separator is echoed by the remote's own login shell, which is fish on
	// these hosts: `echo --` there prints a blank line, and the ps table then
	// never gets found. Verified live against tp-g6.
	if first := remoteResourcesSeparator[0]; !(first >= 'A' && first <= 'Z') && !(first >= 'a' && first <= 'z') {
		t.Errorf("separator %q must start with a letter so no shell reads it as an option", remoteResourcesSeparator)
	}
	if !strings.Contains(remoteResourcesCmd, "echo "+remoteResourcesSeparator) {
		t.Errorf("command should echo the separator verbatim: %q", remoteResourcesCmd)
	}
}

func TestMergeRemoteResourcesOverridesRendererFigures(t *testing.T) {
	remoteResourceCache.Lock()
	remoteResourceCache.byHost = map[string]remoteHostResources{
		"tp-g6": {cores: 4, bySession: map[string]sessionResources{"work": {cpuPct: 12, memMB: 300}}},
	}
	// "lab" is stamped fresh with no entry: a host that has been asked but has
	// not answered yet. Keeps the merge from spawning an ssh fetch mid-test.
	remoteResourceCache.ts = map[string]time.Time{"tp-g6": time.Now(), "lab": time.Now()}
	remoteResourceCache.inflight = map[string]bool{}
	remoteResourceCache.Unlock()
	t.Cleanup(func() {
		remoteResourceCache.Lock()
		remoteResourceCache.byHost, remoteResourceCache.ts, remoteResourceCache.inflight = nil, nil, nil
		remoteResourceCache.Unlock()
	})

	bridgeNameCache.Lock()
	bridgeNameCache.names = map[string]string{"tp-g6-work": "work"}
	bridgeNameCache.ts = time.Now()
	bridgeNameCache.Unlock()
	t.Cleanup(func() {
		bridgeNameCache.Lock()
		bridgeNameCache.names, bridgeNameCache.ts = nil, time.Time{}
		bridgeNameCache.Unlock()
	})

	sessions := []sessionData{
		{name: "tmux-og", cpuPct: 3, memMB: 100},
		{name: "tp-g6-work", bridgeHost: "tp-g6", cpuPct: 1, memMB: 40},
		{name: "lab-main", bridgeHost: "lab", cpuPct: 2, memMB: 50},
	}
	mergeRemoteResources(sessions)

	if sessions[0].cpuPct != 3 || sessions[0].memMB != 100 {
		t.Errorf("local session was rewritten: %+v", sessions[0])
	}
	if sessions[1].cpuPct != 12 || sessions[1].memMB != 300 {
		t.Errorf("mirror kept renderer figures: %+v", sessions[1])
	}
	// The remote's core count rides along, or the colour scales against this
	// machine's and a mirror row is grey whatever the host is doing.
	if sessions[1].cores != 4 {
		t.Errorf("mirror cores = %v, want 4", sessions[1].cores)
	}
	if sessions[0].cores != 0 {
		t.Errorf("local session must keep cores unset (means this machine): %+v", sessions[0])
	}
	// lab has not answered: the row must say "unknown", not show the
	// renderer's figures and not a fabricated zero.
	if !sessions[2].resUnknown {
		t.Errorf("unanswered host must mark the row unknown: %+v", sessions[2])
	}
	if sessions[1].resUnknown {
		t.Errorf("answered mirror must not be marked unknown: %+v", sessions[1])
	}
	if sessions[0].resUnknown {
		t.Errorf("local session must never be marked unknown: %+v", sessions[0])
	}
}

func TestFormatCPUSubOnePercent(t *testing.T) {
	cases := map[float64]string{0: "0%", 0.04: "<1%", 0.2: "<1%", 0.99: "<1%", 1: "1%", 462.4: "462%"}
	for in, want := range cases {
		if got := formatCPU(in); got != want {
			t.Errorf("formatCPU(%v) = %q, want %q", in, got, want)
		}
	}
	// The reserved column must still fit the widest string it can produce.
	if len("<1%") > cpuColWidth() {
		t.Errorf("<1%% (%d cells) overflows the %d-cell CPU column", len("<1%"), cpuColWidth())
	}
}

func TestCPUColorScalesAgainstTheRowsOwnMachine(t *testing.T) {
	rc := resourceColors{low: "low", med: "med", high: "high", crit: "crit"}
	// One core pegged: critical on a single-core host, background noise on 32.
	if got := rc.cpuColor(100, 1); got != rc.crit {
		t.Errorf("100%% of 1 core = %q, want crit", got)
	}
	if got := rc.cpuColor(100, 32); got != rc.low {
		t.Errorf("100%% of 32 cores = %q, want low", got)
	}
	// A local row carries no core count and must fall back to this machine's.
	if got := rc.cpuColor(numCPU*100, 0); got != rc.crit {
		t.Errorf("fully loaded local row = %q, want crit", got)
	}
}

// seedRemoteResourceCache installs a known ssh-leg cache and tears it down
// again. The cleanup waits for any fetch goroutine first: remoteResourcesFor
// writes back seconds after it was spawned, and a straggler landing in the next
// test's cache is a flake nobody would read as one.
func seedRemoteResourceCache(t *testing.T, byHost map[string]remoteHostResources, ts map[string]time.Time) {
	t.Helper()
	remoteResourceCache.Lock()
	remoteResourceCache.byHost, remoteResourceCache.ts, remoteResourceCache.inflight = byHost, ts, nil
	remoteResourceCache.Unlock()
	t.Cleanup(func() {
		for range 200 {
			remoteResourceCache.Lock()
			busy := false
			for _, v := range remoteResourceCache.inflight {
				busy = busy || v
			}
			remoteResourceCache.byHost, remoteResourceCache.ts, remoteResourceCache.inflight = nil, nil, nil
			remoteResourceCache.Unlock()
			if !busy {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("an ssh fetch never finished")
	})
}

func seedBridgeNames(t *testing.T, names map[string]string) {
	t.Helper()
	bridgeNameCache.Lock()
	bridgeNameCache.names, bridgeNameCache.ts = names, time.Now()
	bridgeNameCache.Unlock()
	t.Cleanup(func() {
		bridgeNameCache.Lock()
		bridgeNameCache.names, bridgeNameCache.ts = nil, time.Time{}
		bridgeNameCache.Unlock()
	})
}

// fakeRemoteResources substitutes the ssh leg and records every host it is
// asked for, so a test can assert both that it ran and that it did not.
func fakeRemoteResources(t *testing.T, byHost map[string]remoteHostResources) chan string {
	t.Helper()
	calls := make(chan string, 8)
	prev := sshRemoteResources
	sshRemoteResources = func(host string) (remoteHostResources, error) {
		calls <- host
		return byHost[host], nil
	}
	t.Cleanup(func() { sshRemoteResources = prev })
	return calls
}

func TestParseBridgeRes(t *testing.T) {
	now := time.Now().Unix()
	cases := map[string]bool{
		"12.5 340 32 %d -":         true,
		"12.5 340 32 %d claude,pi": true,
		"12.5 340 32 %d":           false, // short row: the pre-agent-field shape
		"12.5 340 32 x %d":         false, // the epoch is the fourth field, not the fifth
		"x 340 32 %d -":            false,
		"12.5 340 32 %s -":         false,
	}
	for shape, want := range cases {
		v := strings.NewReplacer("%d", strconv.FormatInt(now, 10), "%s", "later").Replace(shape)
		if _, _, ok := parseBridgeRes(v, now); ok != want {
			t.Errorf("parseBridgeRes(%q) ok = %v, want %v", v, ok, want)
		}
	}
	// A stamp from the future by more than the clocks' granularity is a clock
	// jump, not a measurement: trusting it would hold the row fresh for the
	// whole jump.
	if _, _, ok := parseBridgeRes("1 1 1 "+strconv.FormatInt(now+3600, 10)+" -", now); ok {
		t.Error("a far-future stamp must not read as fresh")
	}
	if _, _, ok := parseBridgeRes("1 1 1 "+strconv.FormatInt(now+1, 10)+" -", now); !ok {
		t.Error("a stamp one second ahead is granularity, not a jump")
	}
}

// The stamp the bridge daemon ships is the primary path: a covered mirror must
// cost no ssh at all, which is the whole point of #693.
func TestMergeRemoteResourcesPrefersBridgeStamp(t *testing.T) {
	calls := fakeRemoteResources(t, nil)
	seedRemoteResourceCache(t, nil, nil)

	sessions := []sessionData{
		{name: "tmux-og", cpuPct: 3, memMB: 100},
		{name: "tp-g6-work", bridgeHost: "tp-g6", cpuPct: 1, memMB: 40,
			bridgeRes: "12.5 340 32 " + strconv.FormatInt(time.Now().Unix(), 10) + " -"},
	}
	mergeRemoteResources(sessions)

	// byHost is nil until remoteResourcesFor asserts the cache's maps, which it
	// does synchronously on entry — so this proves the ssh leg was never
	// reached, with no wait on a goroutine that may not have run yet.
	remoteResourceCache.Lock()
	entered := remoteResourceCache.byHost != nil
	remoteResourceCache.Unlock()
	if entered || len(calls) != 0 {
		t.Fatalf("a covered mirror was still probed over ssh (entered=%v calls=%d)", entered, len(calls))
	}

	if sessions[1].cpuPct != 12.5 || sessions[1].memMB != 340 || sessions[1].cores != 32 {
		t.Errorf("mirror did not take the stamped figures: %+v", sessions[1])
	}
	if sessions[1].resUnknown {
		t.Errorf("a stamped mirror is known: %+v", sessions[1])
	}
	if sessions[0].cpuPct != 3 || sessions[0].memMB != 100 || sessions[0].cores != 0 {
		t.Errorf("local session was rewritten: %+v", sessions[0])
	}
}

// Coverage is per session, not per host: one stamped mirror must not take the
// ssh probe away from its unstamped neighbour, nor have its own values
// overwritten by the answer that neighbour needed.
func TestMergeRemoteResourcesProbesPartiallyCoveredHost(t *testing.T) {
	answer := map[string]remoteHostResources{
		"tp-g6": {cores: 4, bySession: map[string]sessionResources{"play": {cpuPct: 7, memMB: 70}}},
	}
	calls := fakeRemoteResources(t, answer)
	// No timestamps: every host reads stale, so the merge kicks a real fetch we
	// can count while still being served the cached answer.
	seedRemoteResourceCache(t, answer, nil)
	seedBridgeNames(t, map[string]string{"tp-g6-work": "work", "tp-g6-play": "play"})

	sessions := []sessionData{
		{name: "tp-g6-work", bridgeHost: "tp-g6", cpuPct: 1, memMB: 40,
			bridgeRes: "12.5 340 32 " + strconv.FormatInt(time.Now().Unix(), 10) + " -"},
		{name: "tp-g6-play", bridgeHost: "tp-g6", cpuPct: 2, memMB: 50},
	}
	mergeRemoteResources(sessions)

	if host := <-calls; host != "tp-g6" {
		t.Errorf("probed %q, want tp-g6", host)
	}
	if len(calls) != 0 {
		t.Errorf("host probed %d extra times", len(calls))
	}
	if sessions[0].cpuPct != 12.5 || sessions[0].memMB != 340 || sessions[0].cores != 32 {
		t.Errorf("stamped mirror was overwritten by the ssh answer: %+v", sessions[0])
	}
	if sessions[1].cpuPct != 7 || sessions[1].memMB != 70 || sessions[1].cores != 4 {
		t.Errorf("unstamped mirror was not filled from ssh: %+v", sessions[1])
	}
}

// An aged-out stamp means the daemon stopped refreshing it — a dead bridge or a
// dead remote poller — so the row falls back rather than freezing on figures
// that no longer describe anything.
func TestMergeRemoteResourcesStaleStampFallsBack(t *testing.T) {
	answer := map[string]remoteHostResources{
		"tp-g6": {cores: 4, bySession: map[string]sessionResources{"work": {cpuPct: 7, memMB: 70}}},
	}
	fakeRemoteResources(t, answer)
	seedRemoteResourceCache(t, answer, map[string]time.Time{"tp-g6": time.Now()})
	seedBridgeNames(t, map[string]string{"tp-g6-work": "work"})

	old := time.Now().Add(-bridgeResStaleAfter - time.Second).Unix()
	sessions := []sessionData{
		{name: "tp-g6-work", bridgeHost: "tp-g6", bridgeRes: "12.5 340 32 " + strconv.FormatInt(old, 10) + " -"},
	}
	mergeRemoteResources(sessions)

	if sessions[0].cpuPct != 7 || sessions[0].memMB != 70 || sessions[0].cores != 4 {
		t.Errorf("stale stamp was trusted: %+v", sessions[0])
	}
}

// Zero and absent are different states: an idle remote session measures 0, and
// that is an answer.
func TestMergeRemoteResourcesZeroRowIsCovered(t *testing.T) {
	calls := fakeRemoteResources(t, nil)
	seedRemoteResourceCache(t, nil, nil)

	sessions := []sessionData{
		{name: "tp-g6-work", bridgeHost: "tp-g6", cpuPct: 1, memMB: 40,
			bridgeRes: "0.0 0 8 " + strconv.FormatInt(time.Now().Unix(), 10) + " -"},
	}
	mergeRemoteResources(sessions)

	if len(calls) != 0 {
		t.Errorf("a zero row was read as absent and probed over ssh")
	}
	if sessions[0].cpuPct != 0 || sessions[0].memMB != 0 || sessions[0].cores != 8 {
		t.Errorf("zero row = %+v, want the measured zeroes and cores 8", sessions[0])
	}
	if sessions[0].resUnknown {
		t.Errorf("zero is known: %+v", sessions[0])
	}
}

// Neither source answered: the row renders "-", never the renderer's own
// figures, which measure the bridge and not the work.
func TestMergeRemoteResourcesUncoveredStaysUnknown(t *testing.T) {
	fakeRemoteResources(t, nil)
	// Stamped fresh with no entry: a host that has been asked and has not
	// answered. Keeps the merge from spawning a fetch mid-test.
	seedRemoteResourceCache(t, map[string]remoteHostResources{}, map[string]time.Time{"lab": time.Now()})
	seedBridgeNames(t, map[string]string{"lab-main": "main"})

	sessions := []sessionData{{name: "lab-main", bridgeHost: "lab", cpuPct: 2, memMB: 50}}
	mergeRemoteResources(sessions)

	if !sessions[0].resUnknown {
		t.Errorf("a mirror neither source covers must be unknown: %+v", sessions[0])
	}
}

// A mirror's own pane command is the renderer and its @bridge_proc names only
// the remote pane's group leader, so an agent a restore chain relaunched under
// a shell reaches the Procs column through the stamp's agent field alone — the
// covered path must carry it the way the ssh leg's tree walk does.
func TestMergeRemoteResourcesStampCarriesTreeAgents(t *testing.T) {
	fakeRemoteResources(t, nil)
	seedRemoteResourceCache(t, nil, nil)

	sessions := []sessionData{
		{name: "tp-g6-work", bridgeHost: "tp-g6", procs: []string{"fish"},
			bridgeRes: "1.0 64 8 " + strconv.FormatInt(time.Now().Unix(), 10) + " claude"},
		{name: "tp-g6-idle", bridgeHost: "tp-g6", procs: []string{"fish"},
			bridgeRes: "0.0 0 8 " + strconv.FormatInt(time.Now().Unix(), 10) + " -"},
	}
	mergeRemoteResources(sessions)

	if want := []string{"fish", "claude"}; !slices.Equal(sessions[0].procs, want) {
		t.Errorf("procs = %v, want %v", sessions[0].procs, want)
	}
	if want := []string{"fish"}; !slices.Equal(sessions[1].procs, want) {
		t.Errorf("a \"-\" agent field must add nothing: procs = %v, want %v", sessions[1].procs, want)
	}
}

// collectPanesSnapshot's rows feed TWO parsers, each failing closed on a wrong
// field count. paneMap is the one that maps a pane to its session for the
// agent counts, so a count left behind there empties every agent badge with
// nothing else visibly wrong — asserted through paneMap itself, not only
// through sessions().
func TestSnapshotParsersTrackTheFieldCount(t *testing.T) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	row := "%7|tp-g6-work|2|/p|1900000300|tp-g6|renderer|42|claude|/srv/r|12.5 340 32 " + now + " claude"
	snap := panesSnapshot{row}

	pm := snap.paneMap()
	if got, ok := pm["7"]; !ok || got.session != "tp-g6-work" || got.winIdx != 2 {
		t.Errorf("paneMap()[7] = %+v, %v; want tp-g6-work window 2", got, ok)
	}
	sessions := snap.sessions()
	if len(sessions) != 1 || sessions[0].bridgeRes != "12.5 340 32 "+now+" claude" {
		t.Errorf("sessions() = %+v, want one session carrying @bridge_res", sessions)
	}

	short := panesSnapshot{row[:strings.LastIndexByte(row, '|')]}
	if got := short.paneMap(); len(got) != 0 {
		t.Errorf("paneMap() accepted a %d-field row: %v", strings.Count(short[0], "|")+1, got)
	}
	if got := short.sessions(); len(got) != 0 {
		t.Errorf("sessions() accepted a %d-field row: %v", strings.Count(short[0], "|")+1, got)
	}
}
