package daemon

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	originSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	slugOwnerRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	slugNameRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	remoteWinIDRe  = regexp.MustCompile(`^@[0-9]+$`)
)

// githubSlug returns owner/name for a github.com origin, or "" for anything
// else. The host match is exact: the value becomes a --repo argument and a
// checkout path component on this host, so a substring match on "github.com"
// would let github.com.evil or notgithub.com pass.
//
// The remote probe has already stripped the scheme and userinfo; they are
// stripped again here so a probe that ever stops doing so cannot leak a token
// into @bridge_repo.
func githubSlug(origin string) string {
	s := originSchemeRe.ReplaceAllString(origin, "")
	authority, _, _ := strings.Cut(s, "/")
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) < len("github.com") || !strings.EqualFold(s[:len("github.com")], "github.com") {
		return ""
	}
	rest := s[len("github.com"):]
	if rest == "" || (rest[0] != ':' && rest[0] != '/') {
		return ""
	}
	rest = strings.TrimLeft(rest, ":/")
	rest = strings.TrimRight(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") {
		return ""
	}
	if !slugOwnerRe.MatchString(owner) || !slugNameRe.MatchString(name) || name == "." || name == ".." {
		return ""
	}
	return owner + "/" + name
}

// originProbeScript is the POSIX body run under exec /bin/sh -c on the remote.
// Like enrichRefreshScript it holds zero single quotes and zero backslashes,
// and its only # are the ${p#*=}, ${u#*://} and ${u#*@} trims: run-shell
// format-expands the whole string first, and the remote's default shell is
// fish, so neither may see a form it rewrites. Never ## or $#.
//
// The remote runs git because the checkout lives there, and strips the scheme
// and userinfo remote-side so a credentialed https origin never crosses the
// wire. The dir rides in the stamp so the daemon can tell a stamp taken for the
// window's current directory from a stale one left by an earlier cwd.
//
// Output is discarded and the exit is always 0 for enrichRefreshScript's
// reason: a -t job's output or failure paints view mode onto the mirrored pane.
func originProbeScript(win string) string {
	return fmt.Sprintf(
		"exec >/dev/null 2>&1; "+
			"p=$(tmux show-environment -g PATH); "+
			"case $p in PATH=?*) PATH=${p#*=}:$PATH; export PATH;; esac; "+
			"d=$(tmux show-options -wqv -t %s @worktree); "+
			"[ -n \"$d\" ] || d=$(tmux show-options -wqv -t %s @git_root); "+
			"[ -n \"$d\" ] || exit 0; "+
			"u=$(git -C \"$d\" remote get-url origin); "+
			"u=${u#*://}; "+
			"case $u in *@*) u=${u#*@};; esac; "+
			"tmux set-option -w -t %s @og_bridge_origin \"$u $d\"; "+
			"exit 0",
		win, win, win)
}

// originProbeCmd is the remote command that stamps win's @og_bridge_origin. win
// is a remote window id, validated because it is interpolated into a script.
func originProbeCmd(win string) (string, bool) {
	if !remoteWinIDRe.MatchString(win) {
		return "", false
	}
	return fmt.Sprintf("run-shell -b -t %s %s", win,
		tmuxQuote("exec /bin/sh -c "+tmuxQuote(originProbeScript(win)))), true
}
