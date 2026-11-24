package probekit

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// SSHConnectFailureExit is the exit status ssh uses when it could not reach
// the host, as opposed to the remote command's own status.
const SSHConnectFailureExit = 255

var (
	ErrRemoteUnreachable    = errors.New("remote unreachable")
	ErrRemoteNoServer       = errors.New("remote tmux server not running")
	ErrRemoteNeedsAuth      = errors.New("remote needs interactive authentication")
	ErrRemoteHostKeyChanged = errors.New("remote host key changed")
	ErrRemoteTailscaleCheck = errors.New("remote requires a Tailscale SSH check")
)

// AuthFailurePatterns are the ssh stderr signatures meaning a human at a real
// terminal could fix this by answering a prompt. Only consulted on exit 255,
// where the failure is ssh's own.
var AuthFailurePatterns = []string{
	"Host key verification failed",
	// The "(" anchors on ssh's own "Permission denied (publickey,password)."
	// rather than the bare phrase, which also appears in a local firewall's
	// "ssh: connect to host X port 22: Permission denied" (EACCES) — a
	// genuinely down host, not one a prompt could fix.
	"Permission denied (",
	"Too many authentication failures",
	"keyboard-interactive",
}

// HostKeyChangedPattern is ssh's warning that the host key no longer matches
// known_hosts. Kept out of AuthFailurePatterns deliberately: this is the
// signature of a MITM as much as of a reinstalled host, so it must never reach
// a flow that invites the user to connect. ssh prints it alongside "Host key
// verification failed", so it is matched first.
const HostKeyChangedPattern = "REMOTE HOST IDENTIFICATION HAS CHANGED"

// RevokedHostKeyPattern is ssh's refusal banner for a key listed in a
// RevokedHostKeys file. It prints no HostKeyChangedPattern alongside it, only a
// bare "Host key verification failed.", which would otherwise fall into
// AuthFailurePatterns and offer to connect to a host ssh has already refused.
const RevokedHostKeyPattern = "REVOKED HOST KEY DETECTED"

// tailscaleCheckPattern is Tailscale SSH's own banner when the peer's ACL
// rule requires an interactive re-check (`action: "check"`). It arrives on
// STDOUT over the already-established ssh session, not through ssh's own
// auth flow, and blocks until the check clears — which is why the probe
// always exhausts ProbeTimeout instead of exiting 255 (#486).
const tailscaleCheckPattern = "Tailscale SSH requires an additional check"

// tailscaleCheckURLPattern extracts the per-attempt login URL tailscaled
// prints on the line after tailscaleCheckPattern (e.g. "# To authenticate,
// visit: https://login.tailscale.com/a/xyz"). Restricted to a URL-safe
// character class and capped in length deliberately: this is
// remote-controlled stdout, and an unrestricted \S capture matches ESC
// (0x1B) — the remote-row preview is not run through stripStringEscapes,
// so an uncapped capture could carry ANSI into a row fitVisibleWidth
// accounts for in visible cells.
var tailscaleCheckURLPattern = regexp.MustCompile(`To authenticate, visit:\s*([A-Za-z0-9:/_.\-]{1,200})`)

// DetectTailscaleCheck reports whether stdout carries tailscaled's "check"
// banner and, if so, the login URL it printed (empty if the banner's shape
// ever changes upstream and the URL line isn't found).
func DetectTailscaleCheck(stdout string) (url string, ok bool) {
	if !strings.Contains(stdout, tailscaleCheckPattern) {
		return "", false
	}
	if m := tailscaleCheckURLPattern.FindStringSubmatch(stdout); len(m) == 2 {
		return m[1], true
	}
	return "", true
}

// ClassifyProbeErr decides which failure a non-zero probe was. ssh exits 255
// when it could not reach the host; any other status is the remote command's
// own, so the host answered and only its tmux server is missing (#266). Within
// 255, stderr distinguishes a host that merely wants an interactive answer from
// one that is genuinely down (#357) — an unrecognised 255 stays unreachable, so
// being wrong costs a stale label rather than a pointless password prompt. The
// timeout branch also inspects stdout: a Tailscale SSH "check" host is killed
// by the probe's own deadline rather than exiting 255, but tailscaled's check
// banner still arrives on stdout before the process is killed, so a killed
// probe's stdout is still usable evidence (#486).
func ClassifyProbeErr(err error, stdout, stderr string, timedOut bool) error {
	if timedOut {
		if url, ok := DetectTailscaleCheck(stdout); ok {
			return &TailscaleCheckErr{URL: url}
		}
		return fmt.Errorf("%w: probe timed out", ErrRemoteUnreachable)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != SSHConnectFailureExit {
		return fmt.Errorf("%w: %w", ErrRemoteNoServer, err)
	}
	if strings.Contains(stderr, HostKeyChangedPattern) || strings.Contains(stderr, RevokedHostKeyPattern) {
		return fmt.Errorf("%w: %w", ErrRemoteHostKeyChanged, err)
	}
	for _, p := range AuthFailurePatterns {
		if strings.Contains(stderr, p) {
			return fmt.Errorf("%w: %w", ErrRemoteNeedsAuth, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrRemoteUnreachable, err)
}

// TailscaleCheckErr carries the login URL ClassifyProbeErr captured for a
// Tailscale SSH "check" host; TailscaleCheckURL recovers it via errors.As.
type TailscaleCheckErr struct {
	URL string
}

func (e *TailscaleCheckErr) Error() string {
	if e.URL == "" {
		return ErrRemoteTailscaleCheck.Error()
	}
	return ErrRemoteTailscaleCheck.Error() + ": " + e.URL
}

func (e *TailscaleCheckErr) Unwrap() error { return ErrRemoteTailscaleCheck }

// TailscaleCheckURL recovers the login URL ClassifyProbeErr captured for a
// Tailscale SSH "check" host, or "" if err isn't that state or carried none.
func TailscaleCheckURL(err error) string {
	if e, ok := errors.AsType[*TailscaleCheckErr](err); ok && e != nil {
		return e.URL
	}
	return ""
}
