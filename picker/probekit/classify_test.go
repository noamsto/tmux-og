package probekit

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// Exit 255 means ssh itself failed; stderr says whether a human could fix it.
func TestClassifyProbeErr(t *testing.T) {
	exitErr := func(code int) error {
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}

	const hostKeyChangedStderr = "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n" +
		"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n" +
		"Host key verification failed."

	// ssh's refusal banner for a RevokedHostKeys match: no "IDENTIFICATION HAS
	// CHANGED" line, just this warning followed by the same bare
	// "Host key verification failed." that AuthFailurePatterns matches.
	const revokedHostKeyStderr = "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n" +
		"@    WARNING: REVOKED HOST KEY DETECTED!               @\n" +
		"The ECDSA host key for tp-g6 is marked as revoked.\n" +
		"Host key verification failed."

	// timeoutErr mirrors how a real probe's exec.Command looks after its
	// context deadline kills it: an *exec.ExitError from a signal-killed
	// process, same as the "timeout beats the exit status" case below.
	timeoutErr := exitErr(255)

	// The real captured transcript from a Tailscale SSH host whose ACL rule
	// requires an interactive "check" (#486) — verified, do not re-derive.
	const tailscaleCheckStdout = "# Tailscale SSH requires an additional check.\n" +
		"# To authenticate, visit: https://login.tailscale.com/a/l1e32e4993312e8\n"

	cases := []struct {
		name     string
		err      error
		stdout   string
		stderr   string
		timedOut bool
		want     error
	}{
		// Baselines: the pre-#357 behaviour, unchanged.
		{"bare 255", exitErr(255), "", "", false, ErrRemoteUnreachable},
		{"exit 1 is the remote command's own", exitErr(1), "", "", false, ErrRemoteNoServer},
		{"tmux missing on the remote", exitErr(127), "", "", false, ErrRemoteNoServer},
		{"timeout beats the exit status", exitErr(1), "", "", true, ErrRemoteUnreachable},
		{"ssh binary missing", errors.New(`exec: "ssh": not found`), "", "", false, ErrRemoteUnreachable},

		// New: 255 plus a stderr signature a prompt could fix.
		{"unknown host key", exitErr(255), "", "Host key verification failed.", false, ErrRemoteNeedsAuth},
		{"password refused", exitErr(255), "", "noams@mbp: Permission denied (publickey,password).", false, ErrRemoteNeedsAuth},
		{"agent exhausted", exitErr(255), "", "Received disconnect: Too many authentication failures", false, ErrRemoteNeedsAuth},
		{"2fa offered", exitErr(255), "", "Authentications that can continue: keyboard-interactive", false, ErrRemoteNeedsAuth},

		// New: a changed key outranks the auth patterns ssh prints alongside it.
		{"host key changed", exitErr(255), "", hostKeyChangedStderr, false, ErrRemoteHostKeyChanged},
		// New: a revoked key must land on the same inert row as a changed one —
		// ssh refuses it unconditionally, so nothing unsafe can be accepted, but
		// the row must never invite the "Enter to connect" action regardless.
		{"revoked host key", exitErr(255), "", revokedHostKeyStderr, false, ErrRemoteHostKeyChanged},

		// Genuinely down hosts must not be dragged into the auth flow.
		{"refused", exitErr(255), "", "ssh: connect to host lab port 22: Connection refused", false, ErrRemoteUnreachable},
		{"no route", exitErr(255), "", "ssh: connect to host lab port 22: No route to host", false, ErrRemoteUnreachable},
		{"unknown name", exitErr(255), "", "ssh: Could not resolve hostname lab: Name or service not known", false, ErrRemoteUnreachable},
		// A local firewall's EACCES prints the same words as ssh's own auth
		// refusal but with no "(publickey,...)" reason list — a genuinely down
		// host, not one a prompt could fix.
		{"firewall EACCES", exitErr(255), "", "ssh: connect to host lab port 22: Permission denied", false, ErrRemoteUnreachable},

		// Precedence: a non-255 exit is the remote command's, whatever it printed.
		{"remote command printed Permission denied", exitErr(1), "", "cat: /etc/shadow: Permission denied", false, ErrRemoteNoServer},
		// Precedence: a killed process has no meaningful stderr verdict.
		{"timeout beats an auth signature", exitErr(255), "", "Host key verification failed.", true, ErrRemoteUnreachable},

		// New (#486): tailscaled's own "check" banner arrives on stdout before
		// the probe's context deadline kills the process — that's why this is
		// a timeout, not a 255. ClassifyProbeErr must read stdout only when
		// timedOut, and only then reclassify away from ErrRemoteUnreachable.
		{"tailscale check banner + URL", timeoutErr, tailscaleCheckStdout, "", true, ErrRemoteTailscaleCheck},
		// The URL line is best-effort — the banner alone still commits the
		// classification even if tailscaled's second line ever changes shape.
		{"tailscale check banner, no URL line", timeoutErr, "Tailscale SSH requires an additional check.\n", "", true, ErrRemoteTailscaleCheck},

		// Regression guard (#486): a plain timeout with nothing on stdout —
		// i.e. a genuinely unreachable host, not a Tailscale interception —
		// must stay ErrRemoteUnreachable. A classifier that reclassified any
		// timeout would turn every dead host into a false "run ssh yourself"
		// prompt.
		{"plain timeout, empty stdout, stays unreachable", exitErr(255), "", "", true, ErrRemoteUnreachable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyProbeErr(tc.err, tc.stdout, tc.stderr, tc.timedOut)
			if !errors.Is(got, tc.want) {
				t.Errorf("ClassifyProbeErr(%v, %q, %q, %v) = %v, want %v", tc.err, tc.stdout, tc.stderr, tc.timedOut, got, tc.want)
			}
		})
	}

	// Pin the round-trip: ClassifyProbeErr must hand TailscaleCheckURL a
	// *TailscaleCheckErr it can recover via errors.As, or the URL a user
	// would copy-paste silently goes missing.
	t.Run("tailscale check URL round-trips through TailscaleCheckURL", func(t *testing.T) {
		got := ClassifyProbeErr(timeoutErr, tailscaleCheckStdout, "", true)
		const want = "https://login.tailscale.com/a/l1e32e4993312e8"
		if url := TailscaleCheckURL(got); url != want {
			t.Errorf("TailscaleCheckURL(got) = %q, want %q", url, want)
		}
	})

	t.Run("tailscale check with no URL line yields an empty URL", func(t *testing.T) {
		got := ClassifyProbeErr(timeoutErr, "Tailscale SSH requires an additional check.\n", "", true)
		if url := TailscaleCheckURL(got); url != "" {
			t.Errorf("TailscaleCheckURL(got) = %q, want empty", url)
		}
	})

	// An attacker-controlled remote could try to smuggle ANSI/control bytes
	// into the URL it prints. tailscaleCheckURLPattern's character class must
	// stop capturing at the first non-URL-safe byte, so the escape sequence
	// never reaches a row rendered outside stripStringEscapes.
	t.Run("ANSI/control-byte injection in the URL line is not captured", func(t *testing.T) {
		const injected = "Tailscale SSH requires an additional check.\n" +
			"To authenticate, visit: https://x\x1b[31mFAKE\x1b[0m.example.com/a\n"
		got := ClassifyProbeErr(timeoutErr, injected, "", true)
		if !errors.Is(got, ErrRemoteTailscaleCheck) {
			t.Fatalf("got = %v, want ErrRemoteTailscaleCheck", got)
		}
		url := TailscaleCheckURL(got)
		if strings.ContainsRune(url, 0x1b) {
			t.Errorf("TailscaleCheckURL(got) = %q, contains an ESC byte", url)
		}
		if url != "https://x" {
			t.Errorf("TailscaleCheckURL(got) = %q, want capture to stop at the first non-URL-safe byte (%q)", url, "https://x")
		}
	})
}
