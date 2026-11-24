package probekit

import "time"

// ProbeTimeout bounds each per-host ssh list-sessions probe so a down host
// cannot stall the session picker's async remote section.
const ProbeTimeout = 3 * time.Second

// SnapshotProbeTimeout bounds one project snapshot probe. It runs git in every
// candidate checkout, so it gets more than a session listing.
const SnapshotProbeTimeout = 8 * time.Second

// HeadsProbeTimeout bounds the `git ls-remote` heads call.
const HeadsProbeTimeout = 6 * time.Second

// SSHArgv is the argv after `ssh` for running body on host. Never a non-batch
// ssh: a prompt would land in the caller's pty.
func SSHArgv(host, body string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=2", "-T", host, "--", body}
}
