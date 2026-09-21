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

func TestParsePanePIDs(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want map[string][]int
	}{
		{
			name: "two sessions",
			out:  "work|101\nwork|102\nscratch|203\n",
			want: map[string][]int{"work": {101, 102}, "scratch": {203}},
		},
		{
			name: "session name containing a pipe",
			out:  "a|b|404\n",
			want: map[string][]int{"a|b": {404}},
		},
		{
			name: "unusable pids skipped",
			out:  "work|0\nwork|-3\nwork|abc\nwork|7\n",
			want: map[string][]int{"work": {7}},
		},
		{
			name: "empty",
			out:  "",
			want: map[string][]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePanePIDs(tt.out)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePanePIDs(%q) = %v, want %v", tt.out, got, tt.want)
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
		"work":    "1.0 10 8 17",
		"scratch": "0.0 0 8 17",
		"alpha":   "2.5 20 8 17",
	}
	want := []string{
		"set-option", "-t", "alpha", resOption, "2.5 20 8 17", ";",
		"set-option", "-t", "scratch", resOption, "0.0 0 8 17", ";",
		"set-option", "-t", "work", resOption, "1.0 10 8 17",
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
