package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/proctree"
)

func TestParseControlClients(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"one bridged among real clients", "0\n1\n0\n", true},
		{"only real clients", "0\n0\n", false},
		{"no clients at all", "", false},
		{"garbage", "yes\nno\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseControlClients(tt.out); got != tt.want {
				t.Errorf("parseControlClients(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestUsagePollerArmed(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"armed", "@og-pr-tick \"x\"\n@og-usage-tick \"run-shell -b y\"\n", true},
		{"unarmed", "@og-pr-tick \"x\"\n@og-res-tick \"y\"\n", false},
		{"empty", "", false},
		{"only a lookalike prefix", "@og-usage-tick-x \"y\"\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usagePollerArmed(tt.out); got != tt.want {
				t.Errorf("usagePollerArmed(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestStampValue(t *testing.T) {
	tests := []struct {
		name   string
		totals proctree.Totals
		cores  int
		now    int64
		want   string
	}{
		{
			name:   "busy session",
			totals: proctree.Totals{CPUPct: 12.34, MemMB: 2048.7},
			cores:  8,
			now:    1700000000,
			want:   "12.3 2049 8 1700000000 -",
		},
		{
			// Zero is a real measurement, not an absent one: the daemon and the
			// picker must be able to tell an idle session from a missing one.
			name:   "idle session",
			totals: proctree.Totals{},
			cores:  8,
			now:    1700000000,
			want:   "0.0 0 8 1700000000 -",
		},
		{
			// %.1f is what keeps a sub-1% session off the idle row.
			name:   "sub one percent",
			totals: proctree.Totals{CPUPct: 0.4, MemMB: 3},
			cores:  32,
			now:    17,
			want:   "0.4 3 32 17 -",
		},
		{
			// The tree's agents ride along: a restored agent under a shell is
			// invisible to every pane-command read on the far side.
			name:   "agents in the tree",
			totals: proctree.Totals{CPUPct: 1, MemMB: 64, AgentCmds: []string{"claude", "pi"}},
			cores:  8,
			now:    17,
			want:   "1.0 64 8 17 claude,pi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stampValue(tt.totals, tt.cores, tt.now)
			if got != tt.want {
				t.Errorf("stampValue(%+v, %d, %d) = %q, want %q", tt.totals, tt.cores, tt.now, got, tt.want)
			}
			if strings.Contains(got, "|") {
				t.Errorf("stampValue produced a pipe (%q) — the picker's pipe-delimited read fails closed and drops the whole session", got)
			}
		})
	}
}

func TestSetOptionArgv(t *testing.T) {
	rows := map[string]string{
		"$2": "1.0 10 8 17 -",
		"$1": "0.0 0 8 17 -",
		"$0": "2.5 20 8 17 claude",
	}
	want := []string{
		"set-option", "-t", "$0", resOption, "2.5 20 8 17 claude", ";",
		"set-option", "-t", "$1", resOption, "0.0 0 8 17 -", ";",
		"set-option", "-t", "$2", resOption, "1.0 10 8 17 -",
	}
	got := setOptionArgv(rows)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("setOptionArgv() = %#v, want %#v", got, want)
	}
	if again := setOptionArgv(rows); !reflect.DeepEqual(again, got) {
		t.Errorf("setOptionArgv() is not deterministic: %#v then %#v", got, again)
	}
	if nilArgv := setOptionArgv(map[string]string{}); nilArgv != nil {
		t.Errorf("setOptionArgv(empty) = %#v, want nil", nilArgv)
	}
}

func TestParsePaneRows(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []paneRow
	}{
		{
			name: "two sessions",
			out:  "$0|%1|101\n$0|%2|102\n$3|%5|203\n",
			want: []paneRow{{"$0", "%1", 101}, {"$0", "%2", 102}, {"$3", "%5", 203}},
		},
		{
			name: "unusable rows skipped",
			out:  "$0|%1|0\n$0|%2|-3\n$0|%3|abc\n$0||7\n$0|%4|7\n",
			want: []paneRow{{"$0", "%4", 7}},
		},
		{
			name: "empty",
			out:  "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePaneRows(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePaneRows(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

// paneAgentMap is the producer's per-pane resolution: every pane in the root
// set gets an entry, and a pane whose tree carries no agent maps to "" — that
// empty entry is what makes paneAgentArgv emit the UNSET that clears a stale
// @og_pane_agent after the agent exits. A map that omits agentless panes (the
// bug this test pins) leaves the option set and the daemon reading the pane as
// an agent indefinitely.
func TestPaneAgentMap(t *testing.T) {
	ps := strings.Join([]string{
		"  PID  PPID %CPU   RSS COMMAND",
		"  100     1  0.0   512 fish",
		"  200   100  1.0 65536 claude",
		"  300     1  0.0   512 fish",
	}, "\n")
	got := paneAgentMap(map[string][]int{"%1": {100}, "%2": {300}}, ps)
	want := map[string]string{"%1": "claude", "%2": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paneAgentMap() = %v, want %v (an agentless pane must be present so it can be unset)", got, want)
	}
}

// paneAgentArgv is the per-pane half of the stamp: a pane whose tree carries an
// agent gets the agent name, a pane with none is UNSET so a stale value cannot
// keep a shell pane reading as an agent, and the order is deterministic.
func TestPaneAgentArgv(t *testing.T) {
	panes := map[string]string{"%2": "", "%1": "claude"}
	want := []string{
		"set-option", "-p", "-t", "%1", paneAgentOption, "claude", ";",
		"set-option", "-p", "-t", "%2", "-u", paneAgentOption,
	}
	got := paneAgentArgv(panes)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paneAgentArgv() = %#v, want %#v", got, want)
	}
	if again := paneAgentArgv(panes); !reflect.DeepEqual(again, got) {
		t.Errorf("paneAgentArgv() is not deterministic: %#v then %#v", got, again)
	}
	if nilArgv := paneAgentArgv(map[string]string{}); nilArgv != nil {
		t.Errorf("paneAgentArgv(empty) = %#v, want nil", nilArgv)
	}
}

func TestPSPath(t *testing.T) {
	t.Setenv(psEnv, "")
	if got := psPath(); got != psBin {
		t.Errorf("psPath() = %q, want the linked %q", got, psBin)
	}
	t.Setenv(psEnv, "/tmp/shim-ps")
	if got := psPath(); got != "/tmp/shim-ps" {
		t.Errorf("psPath() = %q, want the override", got)
	}
}
