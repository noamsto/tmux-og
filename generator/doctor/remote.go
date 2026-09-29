package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ParseRemote reads the stdout of the sweep script RunRemote builds: one
// "name=1" (found) or "name=0" (not found) line per remoteChecklist entry.
func ParseRemote(output string) []Result {
	found := make(map[string]string, len(remoteChecklist))
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		found[name] = val
	}

	results := make([]Result, 0, len(remoteChecklist))
	for _, item := range remoteChecklist {
		val, present := found[item.Name]
		switch {
		case !present:
			// A checklist name with no line at all is a garbled/truncated
			// sweep, not a clean negative.
			results = append(results, Result{Name: item.Name, Feature: item.Feature, Detail: "no answer"})
		case val == "1":
			results = append(results, Result{Name: item.Name, Feature: item.Feature, OK: true})
		default:
			detail := "not found"
			if item.Name == "og-remote-picker" {
				detail = "remote too old to probe itself"
			}
			results = append(results, Result{Name: item.Name, Feature: item.Feature, Detail: detail})
		}
	}
	return results
}

// RunRemote sweeps remoteChecklist against host's own PATH over ssh.
func RunRemote(ctx context.Context, host string) (string, error) {
	var sweep strings.Builder
	for _, item := range remoteChecklist {
		fmt.Fprintf(&sweep, "command -v %s >/dev/null 2>&1 && echo %s=1 || echo %s=0;", item.Name, item.Name, item.Name)
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", host, sweep.String()) //nolint:gosec // host follows "--"; sweep is built from the fixed checklist
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("doctor: ssh %s: %s", host, sshErrorDetail(err))
	}
	return string(out), nil
}

// sshErrorDetail prefers the command's captured stderr — which carries the
// actual reason (e.g. "Could not resolve hostname") — over the bare exit
// status exec.ExitError.Error() would otherwise report.
func sshErrorDetail(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return err.Error()
}
