package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/project"
)

func runArgs(args ...string) (code int, stdout, stderr string) {
	var so, se bytes.Buffer
	code = run(args, &so, &se)
	return code, so.String(), se.String()
}

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{
		{"help"}, {"--help"}, {"-h"},
		{"--", "--help"},
		{"--remote-open", "/x/og-remote-open", "--", "help"},
	} {
		code, out, errOut := runArgs(args...)
		if code != 0 || out != project.Usage || errOut != "" {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	if !strings.Contains(project.Usage, "Usage: og project <subcommand>") || !strings.Contains(project.Usage, "status [<project>]") {
		t.Errorf("usage text drifted:\n%s", project.Usage)
	}
}

func TestRunStatusHelpDoesNotProbe(t *testing.T) {
	t.Setenv("TMUX_OG_PROJECTS", filepath.Join(t.TempDir(), "missing.json"))
	for _, flag := range []string{"--help", "-h"} {
		code, out, errOut := runArgs("status", flag)
		if code != 0 || out != project.Usage || errOut != "" {
			t.Errorf("status %s: code %d, stdout %q, stderr %q", flag, code, out, errOut)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand":    {},
		"only separator":   {"--"},
		"unknown":          {"frobnicate"},
		"remote-open only": {"--remote-open", "/x", "--"},
		"missing value":    {"--remote-open"},
		"too many args":    {"status", "a", "b"},
		"unknown flag":     {"--bogus", "status"},
	} {
		code, out, errOut := runArgs(args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "Usage: og project <subcommand>") {
			t.Errorf("%s: code %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
}

func TestRunRemoteOpenIsParsedNotRun(t *testing.T) {
	code, out, errOut := runArgs("--remote-open", "", "--", "frobnicate")
	if code != 2 || out != "" || errOut == "" {
		t.Errorf("empty --remote-open value: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestRunStatusRegistryErrorExitsBeforeProbing(t *testing.T) {
	t.Setenv("TMUX_OG_PROJECTS", filepath.Join(t.TempDir(), "missing.json"))
	code, out, errOut := runArgs("status")
	if code != 1 || out != "" || !strings.HasPrefix(errOut, "og project: ") {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
