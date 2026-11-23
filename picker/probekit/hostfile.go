// Package probekit holds the remote-probe helpers shared between the picker
// and project code: host-alias file naming, owner-only JSON state, probe
// error classification and remote-text sanitising.
package probekit

import "strings"

// HostFileName maps an ssh host alias onto a single safe path segment.
// Distinct hosts can collide, so a reader that cares must verify the host.
func HostFileName(host string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, host)
}
