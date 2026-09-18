package main

import (
	"slices"
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

func TestAggregateResources(t *testing.T) {
	// 100 is the pane's shell; 200 and 300 are its descendants. 999 belongs to
	// nobody and must not be counted anywhere.
	ps := strings.Join([]string{
		"  PID  PPID %CPU   RSS",
		"  100     1  1.0  1024",
		"  200   100  2.5  2048",
		"  300   200  0.5  1024",
		"  400     1  9.0  8192",
		"  999     1  1.0  1024",
	}, "\n")

	got := aggregateResources(map[string][]int{"a": {100}, "b": {400}}, ps)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got["a"].cpuPct != 4.0 {
		t.Errorf("a cpu = %v, want 4.0 (whole tree)", got["a"].cpuPct)
	}
	if got["a"].memMB != 4.0 {
		t.Errorf("a mem = %v MiB, want 4.0", got["a"].memMB)
	}
	if got["b"].cpuPct != 9.0 {
		t.Errorf("b cpu = %v, want 9.0", got["b"].cpuPct)
	}
}

func TestAggregateResourcesNoRoots(t *testing.T) {
	if got := aggregateResources(nil, "  PID  PPID %CPU   RSS\n 100 1 1.0 1024"); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
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
