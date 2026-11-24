package project

import (
	"strconv"
	"strings"

	"github.com/noamsto/tmux-og/picker/probekit"
)

// Window is one window of a host's tmux server showing a worktree.
type Window struct {
	Session  string // raw remote session name; sanitize before rendering
	WindowID string // "@N"
}

// Worktree is one checkout of a repo on a host.
type Worktree struct {
	Path, Branch, Head string // Branch "" = detached
	Windows            []Window
}

// Drift is the host's default branch against origin/<default>.
type Drift struct {
	Known         bool   // both refs existed, so Ahead/Behind are real counts
	Ahead, Behind int    // local commits not on origin/<default>; origin commits not in local
	Tracked       string // refs/remotes/origin/<default> as of the host's last fetch ("" unknown)
	RemoteHead    string // origin's true head from ls-remote ("" unknown)
	Stale         bool   // ls-remote failed or did not run: RemoteHead is not confirmed
}

// Snapshot is one project's checkout on one host.
type Snapshot struct {
	Host, Key, RepoPath, DefaultBranch string
	Worktrees                          []Worktree
	Drift                              Drift
	Err                                error // per-(host, project) problem, set by Merge; nil from ParseProbe
}

// HostProbe is everything one probe of a host returned.
type HostProbe struct {
	Host      string
	Identity  string   // "<machine-id>|<user>" from the I row; "" when absent or the machine id is empty
	Aliases   []string // other configured hosts that probed as this same machine, folded in by Status
	Snapshots []Snapshot
	// Skipped counts malformed or unsafe rows. A repo whose origin is a local path is dropped
	// silently; any other origin without a key counts its R row once, and the W and D rows of either
	// are dropped with it uncounted. A worktree kept without its unsafe branch still counts.
	Skipped int
	Err     error // classified probe failure (probekit.ClassifyProbeErr); nil when the host answered
}

// HeadReq asks for origin's head of Branch, resolved in the checkout at Path.
type HeadReq struct{ Path, Branch string }

// LocalHost is the machine og runs on: probed with a local sh, never ssh.
const LocalHost = "local"

// ProbeScript prints a host's identity, tmux windows and git checkouts as `|`-delimited rows (I, T,
// R, W, D; the free-form field last). It rides inside single quotes through the remote login shell,
// often fish, where `\` and `\'` are escapes, so it holds neither: newlines come from $nl (a command
// substitution keeps an inner newline) and the tmux format is double-quoted. It needs only sh and git
// and exits 0 with fewer rows when tmux, zoxide or git is missing. Every path it examines passes the
// SafePath charset first; a credentialed origin loses its userinfo here, before it crosses the wire.
// The common dir is made absolute with cd and pwd because `rev-parse --path-format` needs git 2.31;
// both cds are -P because git prints that dir relative to the physical cwd, not a symlinked candidate.
// The 150-candidate cap counts distinct paths, so repeated window roots cannot starve zoxide.
// A var because the tmux call is probekit.RemoteTmuxCmd's.
var ProbeScript = `nl=$(echo; echo x); nl=${nl%x}; unset CDPATH; ` +
	`safe() { case $1 in /*) ;; *) return 1;; esac; case $1 in *[!A-Za-z0-9._/@+:-]*) return 1;; esac; }; ` +
	`{ printf "I|%s|%s$nl" "$(grep . /etc/machine-id 2>/dev/null || sysctl -n kern.uuid 2>/dev/null || uname -n)" "$(id -un)"; ` +
	`command -v git >/dev/null 2>&1 || exit 0; ` +
	`tw=$({ ` + probekit.RemoteTmuxCmd(`list-windows -a -F "#{@bridge_win}|#{window_id}|#{@git_root}|#{session_name}"`) + `; } | head -n 1000); ` +
	`printf "%s$nl" "$tw" | while IFS= read -r l; do [ -z "$l" ] || printf "T|%s$nl" "$l"; done; ` +
	`z=$(command -v zoxide 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/zoxide); ` +
	`seen=; sc=; m=0; n=0; ` +
	`{ for c in "$@"; do printf "%s$nl" "$c"; done; ` +
	`printf "%s$nl" "$tw" | while IFS="|" read -r bw wid gr rest; do [ -z "$bw" ] && [ -n "$gr" ] && printf "%s$nl" "$gr"; done; ` +
	`"$z" query -l 2>/dev/null | head -n 300; } | while IFS= read -r c; do ` +
	`safe "$c" || continue; case " $sc " in *" $c "*) continue;; esac; sc="$sc $c"; m=$((m+1)); [ "$m" -le 150 ] || break; ` +
	`[ -d "$c" ] || continue; ` +
	`g=$(cd -P "$c" 2>/dev/null && x=$(git rev-parse --git-common-dir 2>/dev/null) && cd -P "$x" 2>/dev/null && pwd -P) || continue; ` +
	`safe "$g" || continue; case " $seen " in *" $g "*) continue;; esac; seen="$seen $g"; ` +
	`u=$(git -C "$g" remote get-url origin 2>/dev/null); [ -n "$u" ] || continue; ` +
	`case $u in *://*) s=${u%%://*}; r=${u#*://}; au=${r%%/*}; case $au in *@*) u=$s://${au##*@}${r#"$au"};; esac;; esac; ` +
	`d=$(git -C "$g" symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null); d=${d#origin/}; [ -n "$d" ] || d=main; ` +
	`printf "R|%s|%s|%s$nl" "$g" "$d" "$u"; ` +
	`git -C "$g" worktree list --porcelain 2>/dev/null | head -n 2000 | while read -r k v; do case $k in ` +
	`worktree) wp=$v; h=; br=;; HEAD) h=$v;; branch) br=${v#refs/heads/};; ` +
	`"") safe "$wp" && printf "W|%s|%s|%s|%s$nl" "$g" "$h" "$wp" "$br"; wp=;; esac; done; ` +
	`c2=$(git -C "$g" rev-list --left-right --count "refs/heads/$d...refs/remotes/origin/$d" 2>/dev/null); ` +
	`t=$(git -C "$g" rev-parse -q --verify "refs/remotes/origin/$d" 2>/dev/null); ` +
	`printf "D|%s|%s|%s|%s$nl" "$g" "${c2%%[!0-9]*}" "${c2##*[!0-9]}" "$t"; ` +
	`n=$((n+1)); [ "$n" -lt 60 ] || break; done; } | head -n 5000`

// HeadsScript prints `H|<path>|<sha>` for each (path, branch) argument pair whose origin answers
// `git ls-remote`, in parallel and each bounded by timeout(1) where the host has it. One printf per
// row, so the parallel writers never interleave a line, and no pipe stage after them: a stdio
// buffer would lose the rows already answered when the deadline kills the run. Go bounds the output
// by the pairs it passes. The git environment refuses every credential, askpass and host-key
// prompt, since nothing can answer one. Same quoting rules as ProbeScript.
const HeadsScript = `nl=$(echo; echo x); nl=${nl%x}; t=; command -v timeout >/dev/null 2>&1 && t="timeout 4"; ` +
	`while [ $# -ge 2 ]; do p=$1; b=$2; shift 2; ` +
	`( h=$(GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=true SSH_ASKPASS=true SSH_ASKPASS_REQUIRE=never GCM_INTERACTIVE=never $t git -C "$p" ls-remote --exit-code origin "refs/heads/$b" 2>/dev/null </dev/null) && ` +
	`printf "H|%s|%s$nl" "$p" "${h%%[!0-9a-f]*}" ) & done; wait`

const (
	maxLineLen          = 4096
	maxIdentityLen      = 256
	maxRepos            = 200
	maxWorktreesPerRepo = 500
	maxWindows          = 2000
)

// ProbeArgv runs ProbeScript locally over the SafePath registry paths.
func ProbeArgv(paths []string) []string {
	return append([]string{"sh", "-c", ProbeScript, "_"}, safePaths(paths)...)
}

// ProbeRemoteCommand is ProbeArgv as one remote login-shell command line.
func ProbeRemoteCommand(paths []string) string {
	return remoteCommand(ProbeScript, safePaths(paths))
}

// HeadsArgv runs HeadsScript locally over the requests that pass SafePath and SafeBranch.
func HeadsArgv(reqs []HeadReq) []string {
	return append([]string{"sh", "-c", HeadsScript, "_"}, headArgs(reqs)...)
}

// HeadsRemoteCommand is HeadsArgv as one remote login-shell command line.
func HeadsRemoteCommand(reqs []HeadReq) string {
	return remoteCommand(HeadsScript, headArgs(reqs))
}

// remoteCommand single-quotes script and args without escaping: the scripts hold no quote and every
// arg passed SafePath or SafeBranch, whose charsets have none either.
func remoteCommand(script string, args []string) string {
	var b strings.Builder
	b.WriteString("sh -c '" + script + "' _")
	for _, a := range args {
		b.WriteString(" '" + a + "'")
	}
	return b.String()
}

func safePaths(paths []string) []string {
	safe := make([]string, 0, len(paths))
	for _, p := range paths {
		if SafePath(p) {
			safe = append(safe, p)
		}
	}
	return safe
}

func headArgs(reqs []HeadReq) []string {
	args := make([]string, 0, 2*len(reqs))
	for _, r := range reqs {
		if SafePath(r.Path) && SafeBranch(r.Branch) {
			args = append(args, r.Path, r.Branch)
		}
	}
	return args
}

type repoState struct {
	snap      Snapshot
	commonDir string
	pathSet   bool
	drifted   bool
}

type pendingWindow struct {
	gitRoot string
	win     Window
}

type probeParser struct {
	hp         HostProbe
	identified bool
	repos      []*repoState
	byDir      map[string]*repoState
	dropped    map[string]bool // common dirs of repos without a key, whose W and D rows go with them
	windows    []pendingWindow
}

// ParseProbe keeps only the well-formed, safe rows of ProbeScript's output; everything a host
// returns is untrusted. Every snapshot comes back Stale: no ls-remote has run yet.
func ParseProbe(host, out string) HostProbe {
	p := probeParser{hp: HostProbe{Host: host}, byDir: map[string]*repoState{}, dropped: map[string]bool{}}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		if len(line) > maxLineLen || !p.row(line) {
			p.hp.Skipped++
		}
	}
	return p.finish()
}

func (p *probeParser) row(line string) bool {
	tag, _, _ := strings.Cut(line, "|")
	switch tag {
	case "I":
		return p.identity(strings.SplitN(line, "|", 3))
	case "R":
		return p.repo(strings.SplitN(line, "|", 4))
	case "W":
		return p.worktree(strings.SplitN(line, "|", 5))
	case "D":
		return p.drift(strings.SplitN(line, "|", 5))
	case "T":
		return p.window(strings.SplitN(line, "|", 5))
	}
	return false
}

func (p *probeParser) identity(f []string) bool {
	if len(f) != 3 {
		return false
	}
	if p.identified {
		return true
	}
	id := f[1] + "|" + f[2]
	if len(id) > maxIdentityLen || !allBytes(id, isPrintable) {
		return false
	}
	p.identified = true
	// An empty machine id would match every other host that has none.
	if f[1] != "" {
		p.hp.Identity = id
	}
	return true
}

func (p *probeParser) repo(f []string) bool {
	if len(f) != 4 || !SafePath(f[1]) {
		return false
	}
	key, ok := NormalizeKey(f[3])
	if !ok {
		p.dropped[f[1]] = true
		// A local-path origin is a normal repo with no project key, not a hostile row.
		return isLocalOrigin(f[3])
	}
	if p.byDir[f[1]] != nil || len(p.repos) == maxRepos {
		return true
	}
	branch := f[2]
	if !SafeBranch(branch) {
		branch = ""
	}
	r := &repoState{commonDir: f[1], snap: Snapshot{Host: p.hp.Host, Key: key, DefaultBranch: branch}}
	p.repos = append(p.repos, r)
	p.byDir[f[1]] = r
	return true
}

func (p *probeParser) worktree(f []string) bool {
	if len(f) != 5 {
		return false
	}
	r := p.byDir[f[1]]
	head, path, branch := f[2], f[3], f[4]
	if r == nil && p.dropped[f[1]] {
		return true
	}
	if r == nil || (head != "" && !isSHA(head)) || !SafePath(path) {
		return false
	}
	// git accepts branch names SafeBranch refuses: the checkout stays, shown as detached.
	branchOK := branch == "" || SafeBranch(branch)
	if !branchOK {
		branch = ""
	}
	if !r.pathSet {
		r.snap.RepoPath, r.pathSet = path, true
	}
	if head != "" && len(r.snap.Worktrees) < maxWorktreesPerRepo {
		r.snap.Worktrees = append(r.snap.Worktrees, Worktree{Path: path, Branch: branch, Head: head})
	}
	return branchOK
}

func (p *probeParser) drift(f []string) bool {
	if len(f) != 5 {
		return false
	}
	r := p.byDir[f[1]]
	if r == nil && p.dropped[f[1]] {
		return true
	}
	if r == nil || (f[4] != "" && !isSHA(f[4])) {
		return false
	}
	var d Drift
	switch {
	case f[2] == "" && f[3] == "":
	case f[2] != "" && f[3] != "" && allBytes(f[2], isDigit) && allBytes(f[3], isDigit):
		ahead, errA := strconv.Atoi(f[2])
		behind, errB := strconv.Atoi(f[3])
		if errA != nil || errB != nil {
			return false
		}
		d = Drift{Known: true, Ahead: ahead, Behind: behind}
	default:
		return false
	}
	if r.drifted {
		return true
	}
	d.Tracked = f[4]
	r.snap.Drift, r.drifted = d, true
	return true
}

func (p *probeParser) window(f []string) bool {
	if len(f) != 5 {
		return false
	}
	bridgeWin, id, gitRoot, session := f[1], f[2], f[3], f[4]
	// A mirror and a window outside any repo are expected, not malformed.
	if bridgeWin != "" || gitRoot == "" {
		return true
	}
	if !isWindowID(id) || !SafePath(gitRoot) || session == "" {
		return false
	}
	if len(p.windows) < maxWindows {
		p.windows = append(p.windows, pendingWindow{gitRoot: gitRoot, win: Window{Session: session, WindowID: id}})
	}
	return true
}

// finish attaches windows to worktrees, which T rows precede, and fills the defaults.
func (p *probeParser) finish() HostProbe {
	byPath := map[string]*Worktree{}
	for _, r := range p.repos {
		for i := range r.snap.Worktrees {
			if _, dup := byPath[r.snap.Worktrees[i].Path]; !dup {
				byPath[r.snap.Worktrees[i].Path] = &r.snap.Worktrees[i]
			}
		}
	}
	for _, w := range p.windows {
		if wt := byPath[w.gitRoot]; wt != nil {
			wt.Windows = append(wt.Windows, w.win)
		}
	}
	for _, r := range p.repos {
		if !r.pathSet {
			r.snap.RepoPath = r.commonDir
			if trimmed, ok := strings.CutSuffix(r.commonDir, "/.git"); ok && trimmed != "" {
				r.snap.RepoPath = trimmed
			}
		}
		r.snap.Drift.Stale = true
		p.hp.Snapshots = append(p.hp.Snapshots, r.snap)
	}
	return p.hp
}

// ParseHeads keeps the well-formed `H|<path>|<sha>` rows of HeadsScript's output.
func ParseHeads(out string) map[string]string {
	heads := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if len(line) > maxLineLen {
			continue
		}
		f := strings.SplitN(line, "|", 3)
		if len(f) == 3 && f[0] == "H" && SafePath(f[1]) && isSHA(f[2]) {
			heads[f[1]] = f[2]
		}
	}
	return heads
}

// ApplyHeads confirms each snapshot's remote head from heads, keyed by RepoPath. A snapshot without
// an answer stays Stale.
func ApplyHeads(snaps []Snapshot, heads map[string]string) {
	for i := range snaps {
		if sha, ok := heads[snaps[i].RepoPath]; ok {
			snaps[i].Drift.RemoteHead, snaps[i].Drift.Stale = sha, false
		}
	}
}

// DriftChip renders the default branch's drift; up to date renders nothing.
func (s Snapshot) DriftChip() string {
	d := s.Drift
	switch {
	case s.DefaultBranch == "":
		return ""
	case !d.Known:
		return s.DefaultBranch + " ?"
	case d.Ahead > 0 && d.Behind > 0:
		return s.DefaultBranch + " ≠"
	case d.Behind > 0:
		return s.DefaultBranch + " ↓" + strconv.Itoa(d.Behind)
	case d.Ahead > 0:
		return s.DefaultBranch + " ↑" + strconv.Itoa(d.Ahead)
	}
	return ""
}

// DriftNote qualifies DriftChip: whether origin's true head was checked, and whether it moved past
// the host's last fetch.
func (s Snapshot) DriftNote() string {
	d := s.Drift
	switch {
	case d.Stale:
		return "remote head unchecked"
	case d.RemoteHead != "" && d.Tracked != "" && d.RemoteHead != d.Tracked:
		return "origin moved since last fetch"
	}
	return ""
}

// isSHA reports a full SHA-1 or SHA-256 object name as git prints it.
func isSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && allBytes(s, func(c byte) bool {
		return isDigit(c) || c >= 'a' && c <= 'f'
	})
}

func isLocalOrigin(u string) bool {
	return strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "file://")
}

func isWindowID(s string) bool {
	return len(s) > 1 && s[0] == '@' && allBytes(s[1:], isDigit)
}

func isPrintable(c byte) bool { return c >= 0x20 && c != 0x7f }
