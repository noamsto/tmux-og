package main

import (
	"reflect"
	"testing"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

func TestParseArgsDefaults(t *testing.T) {
	o := parseArgs(nil)
	if o.mode != "tick" {
		t.Fatalf("mode = %q, want tick", o.mode)
	}
	if o.force || o.target != "" || len(o.cfgArgs) != 0 {
		t.Fatalf("unexpected non-zero opts: %+v", o)
	}
}

func TestParseArgsModes(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"tick", []string{"--tick"}, "tick"},
		{"tickrun", []string{"--tick-run"}, "tickrun"},
		{"tickrunpending", []string{"--tick-run-pending"}, "tickrunpending"},
		{"later wins", []string{"--tick", "--tick-run"}, "tickrun"},
		{"later wins reversed", []string{"--tick-run", "--tick"}, "tick"},
		{"mock after tickrun", []string{"--tick-run", "--mock-pr-number", "7"}, "mock"},
		{"mock then tickrun", []string{"--mock-pr-number", "7", "--tick-run"}, "tickrun"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseArgs(c.argv).mode; got != c.want {
				t.Fatalf("mode = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseArgsTargetValues(t *testing.T) {
	o := parseArgs([]string{"--target", "@3", "--branch", "feat/x", "--dir", "/w/x", "--url", "https://github.com/a/b", "--force"})
	if o.target != "@3" || o.branch != "feat/x" || o.dir != "/w/x" || o.url != "https://github.com/a/b" || !o.force {
		t.Fatalf("got %+v", o)
	}
	if len(o.cfgArgs) != 0 {
		t.Fatalf("cfgArgs = %v, want empty", o.cfgArgs)
	}
}

func TestParseArgsMock(t *testing.T) {
	o := parseArgs([]string{
		"--mock-pr-number", "42", "--mock-pr-state", "OPEN", "--mock-check-state", "pending",
		"--mock-pr-title", "T", "--mock-pr-url", "https://github.com/a/b/pull/42",
		"--mock-mergeable", "CONFLICTING", "--mock-draft", "true", "--mock-review", "APPROVED",
		"--mock-auto-merge", "1", "--mock-check-progress", "1/3",
	})
	want := map[string]string{
		"number": "42", "state": "OPEN", "check": "pending", "title": "T",
		"url": "https://github.com/a/b/pull/42", "mergeable": "CONFLICTING", "draft": "true",
		"review": "APPROVED", "autoMerge": "1", "progress": "1/3",
	}
	got := map[string]string{
		"number": o.mock.number, "state": o.mock.state, "check": o.mock.check, "title": o.mock.title,
		"url": o.mock.url, "mergeable": o.mock.mergeable, "draft": o.mock.draft,
		"review": o.mock.review, "autoMerge": o.mock.autoMerge, "progress": o.mock.progress,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mock = %v, want %v", got, want)
	}
	if o.mode != "mock" {
		t.Fatalf("mode = %q, want mock", o.mode)
	}
	if len(o.cfgArgs) != 0 {
		t.Fatalf("cfgArgs = %v, want empty", o.cfgArgs)
	}
}

func TestParseArgsConfig(t *testing.T) {
	argv := []string{
		"--tick-run", "--refresh-seconds", "90", "--force", "--check-refresh-seconds", "300",
		"--reflow", "tmux-reflow-windows", "--notify", "notify-send", "--lib-log", "/tmp/l",
		"--target", "@1",
		"--icon-pending", "P", "--icon-success", "S", "--icon-failure", "F", "--icon-merged", "M",
		"--icon-closed", "C", "--icon-conflict", "X", "--icon-draft", "D",
	}
	o := parseArgs(argv)
	if o.refreshSeconds != 90 || o.checkRefreshSeconds != 300 {
		t.Fatalf("seconds = %d, %d", o.refreshSeconds, o.checkRefreshSeconds)
	}
	if o.reflow != "tmux-reflow-windows" || o.notify != "notify-send" || o.libLog != "/tmp/l" {
		t.Fatalf("got %+v", o)
	}
	wantIcons := enrichstate.Icons{Pending: "P", Success: "S", Failure: "F", Merged: "M", Closed: "C", Conflict: "X", Draft: "D"}
	if o.icons != wantIcons {
		t.Fatalf("icons = %+v, want %+v", o.icons, wantIcons)
	}
	wantCfg := []string{
		"--refresh-seconds", "90", "--check-refresh-seconds", "300",
		"--reflow", "tmux-reflow-windows", "--notify", "notify-send", "--lib-log", "/tmp/l",
		"--icon-pending", "P", "--icon-success", "S", "--icon-failure", "F", "--icon-merged", "M",
		"--icon-closed", "C", "--icon-conflict", "X", "--icon-draft", "D",
	}
	if !reflect.DeepEqual(o.cfgArgs, wantCfg) {
		t.Fatalf("cfgArgs = %v, want %v", o.cfgArgs, wantCfg)
	}
}

func TestParseArgsUnknownIgnored(t *testing.T) {
	o := parseArgs([]string{"--bogus", "--tick-run", "stray"})
	if o.mode != "tickrun" {
		t.Fatalf("mode = %q", o.mode)
	}
}

func TestParseArgsTrailingValueFlagIgnored(t *testing.T) {
	for _, f := range []string{"--target", "--branch", "--dir", "--url", "--mock-pr-number", "--refresh-seconds", "--reflow", "--icon-draft", "--lib-log"} {
		t.Run(f, func(t *testing.T) {
			o := parseArgs([]string{"--tick-run", f})
			if o.mode != "tickrun" {
				t.Fatalf("mode = %q, want tickrun", o.mode)
			}
			if len(o.cfgArgs) != 0 {
				t.Fatalf("cfgArgs = %v, want empty", o.cfgArgs)
			}
		})
	}
}
