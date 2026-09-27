package daemon

import (
	"errors"
	"testing"
)

// A pin is an exact-name match only: tmux's own bare-name resolution would hand
// h-api's pin to h-api-v2 once h-api is gone, which is the hijack the pin
// exists to rule out.
func TestPinLocalSession(t *testing.T) {
	for _, tc := range []struct {
		name, sess, listing string
		want                localPin
		ok                  bool
	}{
		{"only a prefix sibling present", "h-api", "100|$2|h-api-v2\n", localPin{}, false},
		{"exact name beside its prefix sibling", "h-api", "100|$2|h-api-v2\n100|$1|h-api\n", localPin{"100", "$1"}, true},
		{"a name holding the delimiter", "a|b", "100|$3|a\n100|$4|a|b\n", localPin{"100", "$4"}, true},
		{"a malformed session id", "h-api", "100|1|h-api\n", localPin{}, false},
		{"no server pid", "h-api", "|$1|h-api\n", localPin{}, false},
		{"no sessions", "h-api", "", localPin{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{LocalSess: tc.sess, LocalTmuxOut: func(...string) (string, error) { return tc.listing, nil }}
			got, ok := pinLocalSession(cfg)
			if got != tc.want || ok != tc.ok {
				t.Errorf("pinLocalSession(%q over %q) = (%+v, %v), want (%+v, %v)", tc.sess, tc.listing, got, ok, tc.want, tc.ok)
			}
		})
	}

	listed := func(...string) (string, error) { return "100|$1|h-api\n", nil }
	if _, ok := pinLocalSession(Config{LocalTmuxOut: listed}); ok {
		t.Error("pinLocalSession with an empty LocalSess pinned something")
	}
	failing := func(...string) (string, error) { return "100|$1|h-api\n", errors.New("boom") }
	if _, ok := pinLocalSession(Config{LocalSess: "h-api", LocalTmuxOut: failing}); ok {
		t.Error("pinLocalSession pinned from a failed list-sessions")
	}
}

// Only an exact pid|id echo is ownership. display-message on a missing $N
// exits 0 with an empty id — the trap that makes "exit 0" alone worthless —
// and a server that restarted may have reused $N for someone else.
func TestOwnershipCheck(t *testing.T) {
	pin := localPin{serverPID: "100", id: "$1"}
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want ownership
	}{
		{"the pinned session on the pinned server", "100|$1\n", nil, owned},
		{"a missing $N answers exit 0 with an empty id", "100|\n", nil, notOwned},
		{"every field empty", "|\n", nil, notOwned},
		{"$N reused by a restarted server", "200|$1\n", nil, notOwned},
		{"exit 1 is tmux's definite negative", "", exitError(t, 1), notOwned},
		{"another exit status could not ask", "", exitError(t, 2), ownershipUnknown},
		{"tmux could not be exec'd", "", errors.New(`exec: "tmux": executable file not found in $PATH`), ownershipUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argv []string
			cfg := Config{LocalTmuxOut: func(args ...string) (string, error) {
				argv = args
				return tc.out, tc.err
			}}
			if got := pin.check(cfg); got != tc.want {
				t.Errorf("check over (%q, %v) = %v, want %v", tc.out, tc.err, got, tc.want)
			}
			if want := []string{"display-message", "-p", "-t", "$1", "#{pid}|#{session_id}"}; argvKey(argv) != argvKey(want) {
				t.Errorf("check asked %v, want %v", argv, want)
			}
		})
	}

	asked := false
	cfg := Config{LocalTmuxOut: func(...string) (string, error) {
		asked = true
		return "100|$1\n", nil
	}}
	if got := (localPin{}).check(cfg); got != ownershipUnknown {
		t.Errorf("an unset pin checked as %v, want ownershipUnknown", got)
	}
	if asked {
		t.Error("an unset pin asked tmux about an empty target")
	}
}
