package daemon

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

// localPin names the local mirror session the way a name cannot. Once the
// session is gone tmux resolves a bare name by unique prefix — `-t h-api`
// reaches h-api-v2 — and a namesake og-remote-open recreated is
// indistinguishable by name even under `=name`. A session id is never reused
// by a live server, but a restarted one numbers from $0 again, hence the
// server pid beside it.
type localPin struct{ serverPID, id string }

var sessionIDPattern = regexp.MustCompile(`^\$[0-9]+$`)

// pinLocalSession resolves cfg.LocalSess by exact name to its pin. Only the
// first two `|` split a row: the name is last and may itself hold one.
func pinLocalSession(cfg Config) (localPin, bool) {
	if cfg.LocalSess == "" || cfg.LocalTmuxOut == nil {
		return localPin{}, false
	}
	out, err := cfg.LocalTmuxOut("list-sessions", "-F", "#{pid}|#{session_id}|#{session_name}")
	if err != nil {
		return localPin{}, false
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) == 3 && f[2] == cfg.LocalSess && f[0] != "" && sessionIDPattern.MatchString(f[1]) {
			return localPin{serverPID: f[0], id: f[1]}, true
		}
	}
	return localPin{}, false
}

// ownership is what localPin.check can say about the pinned session.
type ownership int

const (
	// ownershipUnknown — the question could not be asked, or nothing is pinned.
	ownershipUnknown ownership = iota
	// owned — the pinned session is alive on the pinned server.
	owned
	// notOwned — tmux answered definitely that it is not.
	notOwned
)

// check asks the local server whether the pinned session still stands.
//
// display-message is the probe because it answers with the server pid, but it
// has a trap: with a `$N` target that no longer exists, a client-less caller
// gets exit 0 and `<pid>|` with an empty id, not "can't find session". Only an exact
// pid|id echo is owned. Exit 1 — "no server running", or an older tmux that
// does error on the missing target — is tmux's definite negative; any other
// failure is a question that could not be asked.
func (p localPin) check(cfg Config) ownership {
	if p.id == "" || cfg.LocalTmuxOut == nil {
		return ownershipUnknown
	}
	out, err := cfg.LocalTmuxOut("display-message", "-p", "-t", p.id, "#{pid}|#{session_id}")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return notOwned
		}
		return ownershipUnknown
	}
	if strings.TrimSpace(out) == p.serverPID+"|"+p.id {
		return owned
	}
	return notOwned
}
