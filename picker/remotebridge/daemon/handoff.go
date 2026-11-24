package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// handoffEntry is one validated prdash worktree hand-off: mode is "switch" or
// "create" (wt switch -c), pr is 0 for an issue.
type handoffEntry struct {
	repo, mode, target string
	pr                 int
}

// handoffWire is prdash's path-free hand-off JSON (its action.HandoffEntry).
type handoffWire struct {
	Repo   string   `json:"repo"`
	Kind   string   `json:"kind"`
	Target string   `json:"target"`
	PR     int      `json:"pr"`
	Args   []string `json:"args"`
	Key    string   `json:"key"`
}

const (
	maxHandoffEntries = 32
	maxHandoffLine    = 4096
	maxHandoffBranch  = 255
	maxHandoffPR      = 999999999
)

var (
	handoffKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
	// prdash's safeHeadRef set: it already sends any other head as pr:<N>.
	handoffBranchRe = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
	handoffPRRe     = regexp.MustCompile(`^pr:[0-9]+$`)
)

// parseHandoff validates raw prdash hand-off lines (`<key>\t<json>`). Any bad
// line rejects the whole batch, so nothing runs on the remote unless every
// entry would. Every value it returns is drawn from a charset holding nothing
// tmux, fish or sh treats specially — no quote, backslash, #, $, space or
// leading - — which is what lets handoffCommand single-quote them as-is.
func parseHandoff(lines []string) ([]handoffEntry, error) {
	if len(lines) < 1 || len(lines) > maxHandoffEntries {
		return nil, fmt.Errorf("handoff: want 1..%d entries, got %d", maxHandoffEntries, len(lines))
	}
	es := make([]handoffEntry, 0, len(lines))
	for i, line := range lines {
		e, err := parseHandoffLine(line)
		if err != nil {
			return nil, fmt.Errorf("handoff: entry %d: %w", i+1, err)
		}
		es = append(es, e)
	}
	return es, nil
}

func parseHandoffLine(line string) (handoffEntry, error) {
	if len(line) > maxHandoffLine {
		return handoffEntry{}, fmt.Errorf("line is %d bytes, over %d", len(line), maxHandoffLine)
	}
	key, js, ok := strings.Cut(line, "\t")
	if !ok {
		return handoffEntry{}, errors.New("no tab between key and entry")
	}
	if !handoffKeyRe.MatchString(key) {
		return handoffEntry{}, fmt.Errorf("bad key %q", key)
	}
	// An argv ([...]) line names a checkout path on this host, which means
	// nothing on the remote.
	if !strings.HasPrefix(js, "{") {
		return handoffEntry{}, errors.New("not a path-free {...} entry")
	}
	var w handoffWire
	if err := json.Unmarshal([]byte(js), &w); err != nil {
		return handoffEntry{}, fmt.Errorf("bad JSON: %w", err)
	}
	if w.Key != key {
		return handoffEntry{}, fmt.Errorf("key %q does not match the line's %q", w.Key, key)
	}
	owner, name, _ := strings.Cut(w.Repo, "/")
	if !validSlug(owner, name) {
		return handoffEntry{}, fmt.Errorf("bad repo %q", w.Repo)
	}
	switch w.Kind {
	case "pr":
		if w.PR < 1 || w.PR > maxHandoffPR {
			return handoffEntry{}, fmt.Errorf("pr entry with pr %d", w.PR)
		}
	case "issue":
		if w.PR != 0 {
			return handoffEntry{}, fmt.Errorf("issue entry with pr %d", w.PR)
		}
	default:
		return handoffEntry{}, fmt.Errorf("bad kind %q", w.Kind)
	}
	mode := ""
	switch {
	case len(w.Args) == 2 && w.Args[0] == "switch":
		mode = "switch"
	case len(w.Args) == 3 && w.Args[0] == "switch" && w.Args[1] == "-c" && w.Kind == "issue":
		mode = "create"
	default:
		// A user-configured switch action with extra flags lands here; naming
		// the args tells them which action to simplify.
		return handoffEntry{}, fmt.Errorf("unexpected args %q", w.Args)
	}
	if w.Args[len(w.Args)-1] != w.Target {
		return handoffEntry{}, fmt.Errorf("args %q do not end in target %q", w.Args, w.Target)
	}
	if !validHandoffTarget(w.Target, w.Kind, w.PR) {
		return handoffEntry{}, fmt.Errorf("bad target %q", w.Target)
	}
	return handoffEntry{repo: w.Repo, mode: mode, target: w.Target, pr: w.PR}, nil
}

// validHandoffTarget accepts pr:<pr> for a pr entry, or a branch name that is
// both in safeHeadRef's charset and one git could hold.
func validHandoffTarget(t, kind string, pr int) bool {
	if handoffPRRe.MatchString(t) {
		return kind == "pr" && t == "pr:"+strconv.Itoa(pr)
	}
	return len(t) <= maxHandoffBranch &&
		handoffBranchRe.MatchString(t) &&
		!strings.HasPrefix(t, "-") &&
		!strings.Contains(t, "..") &&
		!strings.Contains(t, "//") &&
		!slices.ContainsFunc([]string{"/", ".", ".lock"}, func(s string) bool { return strings.HasSuffix(t, s) })
}

// handoffScript is the POSIX body the hand-off float runs under exec /bin/sh
// -c, taking four positional parameters per entry: repo, mode, target, pr. It
// is a constant so entry data reaches sh only as $1..$N, never as code.
//
// Zero single quotes and zero backslashes: it is single-quoted for the remote's
// default shell, fish, which reads a backslash inside single quotes as an
// escape. Its only # are the ${p#*=}/${b#*=} trims, kept #*-only for
// toolResolveScript's reason; never ## or $#.
//
// The PATH and BROWSER restores are toolResolveScript's. The checkout is the
// float's cwd when its origin is the entry's repo, else the one under prdash's
// repos root; a case-insensitive match because prdash spells the repo as GitHub
// or the user typed it. A PR whose branch this checkout has neither locally nor
// on origin opens as pr:<N>, prdash's own standalone rule applied where the
// checkout is. A failure holds the float open long enough to read.
const handoffScript = "p=$(tmux show-environment -g PATH 2>/dev/null); " +
	"case $p in PATH=?*) PATH=${p#*=}:$PATH; export PATH;; esac; " +
	"b=$(tmux show-environment -g BROWSER 2>/dev/null); " +
	"case $b in BROWSER=?*) BROWSER=${b#*=}; export BROWSER;; esac; " +
	"if ! command -v wt >/dev/null 2>&1; then echo tmux-og: wt is not on PATH on this host; sleep 5; exit; fi; " +
	"root=${PRDASH_REPOS_ROOT:-$HOME/git}; fail=0; " +
	"while [ -n \"$1\" ]; do " +
	"repo=$1 mode=$2 target=$3 pr=$4; shift 4; " +
	"want=$(printf %s \"$repo\" | tr A-Z a-z); dir=; " +
	"for c in . \"$root/$repo\"; do " +
	"o=$(git -C \"$c\" remote get-url origin 2>/dev/null | tr A-Z a-z); o=${o%/}; o=${o%.git}; " +
	"case $o in *[@/]github.com[:/]\"$want\") dir=$c; break;; esac; " +
	"done; " +
	"if [ -z \"$dir\" ]; then echo \"tmux-og: no checkout of $repo here or at $root/$repo\"; fail=1; continue; fi; " +
	"if [ \"$mode\" = create ]; then wt -C \"$dir\" switch -c \"$target\" || fail=1; continue; fi; " +
	"case $target in pr:*) ;; *) " +
	"if [ \"$pr\" != 0 ] && ! git -C \"$dir\" rev-parse --verify --quiet \"refs/heads/$target\" >/dev/null && " +
	"! git -C \"$dir\" rev-parse --verify --quiet \"refs/remotes/origin/$target\" >/dev/null; then target=pr:$pr; fi;; " +
	"esac; " +
	"wt -C \"$dir\" switch \"$target\" || fail=1; " +
	"done; " +
	"[ \"$fail\" = 0 ] || sleep 5"

// handoffCommand is the remote command that runs es in one float on pane's
// window. cwd is already tmux-quoted. Each value is single-quoted bare: es came
// through parseHandoff, whose charsets hold no quote or backslash.
func handoffCommand(pane, win, cwd string, es []handoffEntry) string {
	var args strings.Builder
	for _, e := range es {
		fmt.Fprintf(&args, " '%s' '%s' '%s' '%d'", e.repo, e.mode, e.target, e.pr)
	}
	// -t <win> for the label, as in floatToolCommand: the new pane is the
	// window's active one whichever window the control client is on.
	return fmt.Sprintf("new-pane -t %s -c %s %s %s ; set -p -t %s @pane_label prdash-handoff",
		pane, cwd, remoteFloat, tmuxQuote("exec /bin/sh -c "+tmuxQuote(handoffScript)+" sh"+args.String()), win)
}
