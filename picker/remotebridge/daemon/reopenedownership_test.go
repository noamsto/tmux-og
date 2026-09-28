package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flipThemeConn is an eofScriptConn whose write of the theme probe — the first
// remote round trip runMirror makes after its list-windows read — flips a flag
// the ownership fake reads. It models og-remote-open winning the race during
// runMirror's remote reads: the mirror session still stood when the run began,
// and a namesake daemon owns the name by the time the late check runs.
type flipThemeConn struct {
	*eofScriptConn
	fired   bool
	flipped *bool
}

func (c *flipThemeConn) Write(p []byte) (int, error) {
	if !c.fired && strings.Contains(string(p), "theme-toggle") {
		c.fired = true
		*c.flipped = true
	}
	return c.eofScriptConn.Write(p)
}

// TestReopenedRunLosesOwnershipDuringTheRemoteReads pins the late ownership
// check on the re-open path: the theme and session-path round trips are each a
// window in which og-remote-open can recreate the session under a daemon of
// its own, so a check that runs before them leaves the socket and the
// @bridge_session_path stamp to reach whatever now holds the name. The run
// must end in errNotOurs, leave a pre-existing socket untouched, and mutate
// nothing local.
func TestReopenedRunLosesOwnershipDuringTheRemoteReads(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		recreated bool
		mutations callLog
	)
	cfg := Config{
		LocalSess:     "h-api",
		RemoteHost:    "h",
		RemoteSession: "api",
		SockPath:      sock,
		reopened:      true,
		local:         testPin,
		View:          &Viewing{},
		LocalArea:     func() (int, int) { return 0, 0 },
		// The pin answers owned until the flip, then reports the session gone
		// the way a live server answers for a missing $N: exit 0, empty id.
		LocalTmuxOut: func(args ...string) (string, error) {
			if recreated {
				return "100|\n", nil
			}
			return ownedAnswer, nil
		},
		LocalTmux: func(args ...string) error {
			mutations.add(args)
			return nil
		},
	}
	// One reply block per runMirror command: the identity batch (proven by
	// identityMatch), the remote window list, the theme probe and the session
	// path. The script ends at EOF so a run that wrongly proceeds past the
	// check fails out of its post-listen setup rather than blocking here.
	script := identityMatch +
		"%begin 1 2 1\n@1 1 1 main\n%end 1 2 1\n" +
		"%begin 1 3 1\nyes\n%end 1 3 1\n" +
		"%begin 1 4 1\n/remote/api\n%end 1 4 1\n"
	conn := &flipThemeConn{eofScriptConn: newEOFScriptConn(withBarriers(script)), flipped: &recreated}
	cfg.Dial = func() (io.ReadWriteCloser, error) { return conn, nil }

	err := runMirror(cfg)
	if !errors.Is(err, errNotOurs) {
		t.Fatalf("runMirror(...) = %v, want %v", err, errNotOurs)
	}
	if b, rerr := os.ReadFile(sock); rerr != nil || string(b) != "stale" {
		t.Errorf("pre-existing socket = (%q, %v), want it left untouched", b, rerr)
	}
	if calls := mutations.snapshot(); len(calls) != 0 {
		t.Errorf("runMirror mutated the local session: %v", calls)
	}
}
