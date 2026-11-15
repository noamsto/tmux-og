package remoteopen

import "regexp"

// Env is the slice of the environment the argv checks read; a value counts as
// set only when non-empty, like bash's `-n ${X:-}`.
type Env struct{ NewDir, Restore, Tmpdir string }

// Opts is the parsed command line plus the env values that shape the open.
type Opts struct {
	Host, Sess, Win string
	Placed          bool
	IntoName        string
	Selector        string
	WantMode        bool
	NewDir          string
	Restore         string
	Tmpdir          string
}

const (
	intoNeedsBoth = "--into needs both a remote session and a local session name"
	intoUsage     = "usage: og-remote-open <host> <sess> --into <local-session> [--window <selector>]"
)

var (
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)
	selectIDRe  = regexp.MustCompile(`^@(0|[1-9][0-9]*)$`)
	selectOptRe = regexp.MustCompile(`^@[A-Za-z_][A-Za-z0-9_-]{0,63}=[A-Za-z0-9._/@+:-]{1,255}$`)
	windowRe    = regexp.MustCompile(`^@?[0-9]+$`)
)

// ParseArgs covers the argv refusals that run before any tmux read (A1-A10).
func ParseArgs(args []string, env Env) (Opts, error) {
	if len(args) < 1 {
		return Opts{}, &UsageError{"usage: og-remote-open <host> [<sess> [<win>]]"}
	}
	arg := func(n int) string { // 1-based, "" past the end
		if n > len(args) {
			return ""
		}
		return args[n-1]
	}
	o := Opts{Host: args[0], Sess: arg(2), Win: arg(3), NewDir: env.NewDir, Restore: env.Restore, Tmpdir: env.Tmpdir}

	if o.Sess == "--into" {
		return Opts{}, &UsageError{intoNeedsBoth}
	}
	hasWindow, hasInto := false, false
	for i := 3; i <= len(args); i++ {
		hasWindow = hasWindow || args[i-1] == "--window"
		hasInto = hasInto || args[i-1] == "--into"
	}
	if hasWindow && !hasInto {
		return Opts{}, &UsageError{"--window needs --into"}
	}
	if hasWindow && o.Win != "--into" && arg(4) != "--into" {
		return Opts{}, &UsageError{intoUsage}
	}
	if o.Win == "--into" || arg(4) == "--into" {
		if o.Win != "--into" {
			return Opts{}, &UsageError{"--into takes no window argument"}
		}
		if len(args) > 4 && (len(args) != 6 || args[4] != "--window") {
			return Opts{}, &UsageError{intoUsage}
		}
		o.Placed = true
		o.Win = ""
		o.IntoName = arg(4)
		if env.NewDir != "" || env.Restore != "" {
			return Opts{}, &UsageError{"--into cannot be combined with OG_REMOTE_NEW_DIR or OG_REMOTE_RESTORE"}
		}
		if o.Sess == "" || o.IntoName == "" {
			return Opts{}, &UsageError{intoNeedsBoth}
		}
		if !hostRe.MatchString(o.Host) {
			return Opts{}, &UsageError{"--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: " + o.Host}
		}
		if len(args) == 6 {
			o.Selector = args[5]
			if !selectIDRe.MatchString(o.Selector) && !selectOptRe.MatchString(o.Selector) {
				return Opts{}, &UsageError{"--window must be @<id> or @<option>=<value>, got: " + bashQuote(o.Selector)}
			}
		}
		return o, nil
	}
	o.WantMode = o.Win != "" && o.Sess != "" && env.NewDir == "" && env.Restore == ""
	return o, nil
}

// validateWin is A11, which bash runs after the placed local-session reads
// and before the @og_bridge_predict check.
func validateWin(o Opts) error {
	if o.Win != "" && !windowRe.MatchString(o.Win) {
		return &UsageError{"window must be a numeric index or an @id, got: " + o.Win}
	}
	return nil
}

// Validate covers A13-A18, which bash runs after the @og_bridge_predict check.
func Validate(o Opts) error {
	if o.Win != "" && o.Win[0] == '@' && !o.WantMode {
		return &UsageError{"a window @id needs a live session to resolve against (not with OG_REMOTE_RESTORE, OG_REMOTE_NEW_DIR, or no session): " + o.Win}
	}
	if o.NewDir != "" && o.Restore != "" {
		return &UsageError{"OG_REMOTE_NEW_DIR and OG_REMOTE_RESTORE are mutually exclusive"}
	}
	if o.NewDir != "" && !shellQuotable(o.NewDir) {
		return &UsageError{"OG_REMOTE_NEW_DIR contains a backslash, which no remote shell dialect can quote safely: " + o.NewDir}
	}
	if err := requireSessionName(o.Sess); err != nil {
		return err
	}
	if o.Tmpdir != "" && !validRemotePath(o.Tmpdir) {
		return &UsageError{"unusable remote tmpdir: " + o.Tmpdir}
	}
	return nil
}
