package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/themestate"
)

// gitOutput runs git with a short timeout so a stalled repo (NFS, held
// index.lock) can't wedge the once-a-second statusline render. Returns the
// trimmed stdout and whether it succeeded.
func gitOutput(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// volatileFields lists the tmux tokens fetched in one display-message call, in
// output order. Keeping these OUT of the #() argv is what stops line 0 from
// blinking: tmux keys each #() job by (tag, unexpanded cmd, client), but when the
// EXPANSION changes (pane_current_command, client_prefix, @issue_*, …) it kills
// the in-flight job and restarts it (format.c:426-439). A job restarted on every
// tick never lives long enough to finish once the machine is loaded, so it never
// reaches the completion callback that publishes its output — and until a first
// completion that output is empty, which paints line 0 blank. A command string
// that stays constant across ticks lets tmux reuse the one job and keep the last
// line painted while this binary recomputes.
var volatileFields = []string{
	"#{client_prefix}",
	"#{@issue_id}", "#{@issue_branch}", "#{@issue_provider}", "#{@issue_title}",
	"#{@branch}", "#{pane_current_path}", "#{@git_root}",
	"#{@active_pane_icon}", "#{pane_current_command}", "#{@claude_session_fg}",
	"#{@crew_name}", "#{@crew_color}",
	"#{@bridge_win}", "#{@bridge_host}", "#{@bridge_state}",
	"#{@bridge_crew_name}", "#{@bridge_crew_color}",
	"#{@bridge_label_id}", "#{@bridge_label_rest_long}",
	"#{@bridge_proc}",
	"#{@window_has_agent}",
}

// fetchVolatile fills the volatile fields via a single display-message
// roundtrip to the session's active pane. It reports whether prefix is active
// and whether the fetch succeeded; a failed fetch leaves the fields empty (the
// caller re-paints the cached last-good line instead of that degraded frame).
// Fields are pipe-delimited; wrong field count fails closed (paths/titles may contain |).
func (a *args) fetchVolatile() (prefixActive, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	format := strings.Join(volatileFields, "|")
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", a.session, "-F", format).Output()
	if err != nil {
		return false, false
	}
	f := strings.Split(strings.TrimRight(string(out), "\n"), "|")
	if len(f) != len(volatileFields) {
		return false, false
	}
	a.issueID, a.issueBranch, a.issueProvider, a.issueTitle = f[1], f[2], f[3], f[4]
	a.branch, a.panePath, a.gitRoot = f[5], f[6], f[7]
	a.paneIcon, a.paneCmd, a.claudeFg = f[8], f[9], f[10]
	a.crewName, a.crewColor = f[11], f[12]
	a.bridgeWin, a.bridgeHost, a.bridgeState = f[13], f[14], f[15]
	a.bridgeCrewName, a.bridgeCrewColor = f[16], f[17]
	a.bridgeLabelID, a.bridgeLabelRestLong = f[18], f[19]
	a.bridgeProc = f[20]
	a.windowHasAgent = f[21]
	return f[0] == "1", true
}

// statuslineCacheDir holds the per-session last-good rendered line so a failed
// fetchVolatile re-paints the previous frame rather than a degraded one.
const statuslineCacheDir = "/tmp/og-statusline"

// cacheFileName maps a session name to a filesystem-safe file name; distinct
// names stay distinct (any non-safe byte becomes its 2-hex escape).
func cacheFileName(session string) string {
	var b strings.Builder
	for i := 0; i < len(session); i++ {
		c := session[i]
		if c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, ".%02x", c)
	}
	return b.String()
}

func readLastGood(dir, session string) (string, bool) {
	out, err := os.ReadFile(filepath.Join(dir, cacheFileName(session)))
	if err != nil {
		return "", false
	}
	return string(out), true
}

func writeLastGood(dir, session, line string) {
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	path := filepath.Join(dir, cacheFileName(session))
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if os.WriteFile(tmp, []byte(line), 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

type args struct {
	session                                         string
	issueID, issueBranch, issueProvider, issueTitle string
	branch, panePath, gitRoot                       string
	paneIcon, paneCmd, claudeFg, bridgeProc         string
	crewName, crewColor                             string
	windowHasAgent                                  string
	bridgeWin, bridgeHost, bridgeState              string
	bridgeCrewName, bridgeCrewColor                 string
	bridgeLabelID, bridgeLabelRestLong              string

	// theme palette (passed pre-expanded from tmux @thm_* options)
	thmBg, thmRed, thmMauve, thmBlue, thmText, thmSubtext0 string
	thmOverlay1, thmPeach, thmGreen                        string

	// glyphs (tmux @icon_* options + Nix enrich icon set for issue provider)
	iconSession, iconBranch, iconDir string
	iconLinear, iconGitHub           string
	iconRemote                       string

	// coding-agent usage segment (feature off when usageMonthlyThreshold == 0)
	iconUsageClaude, iconUsageCodex, iconUsageCursor string
	usageMonthlyThreshold                            int
}

// branchDisplay mirrors tmux-branch-display.sh: prefer the cached @branch,
// else `git -C <path> branch --show-current`.
func branchDisplay(branch, panePath string) string {
	if branch != "" {
		return branch
	}
	if panePath == "" {
		return ""
	}
	if s, ok := gitOutput(panePath, "branch", "--show-current"); ok {
		return s
	}
	return ""
}

// dirDisplay mirrors tmux-dir-display.sh: path relative to git root as ./sub,
// "./" at root, else ~-collapsed absolute path.
func dirDisplay(panePath, gitRoot string) string {
	if gitRoot == "" && panePath != "" {
		if s, ok := gitOutput(panePath, "rev-parse", "--show-toplevel"); ok {
			gitRoot = s
		}
	}
	if gitRoot != "" && strings.HasPrefix(panePath, gitRoot) {
		if panePath == gitRoot {
			return "./"
		}
		return "./" + strings.TrimPrefix(panePath, gitRoot+"/")
	}
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(panePath, home) {
		return "~" + strings.TrimPrefix(panePath, home)
	}
	return panePath
}

// sessionSegment renders the leading session + issue-or-branch chunk.
func sessionSegment(a args, prefixActive bool) string {
	var b strings.Builder
	switch {
	case prefixActive:
		b.WriteString("#[fg=" + a.thmRed + ",bold]")
	case a.claudeFg != "":
		b.WriteString("#[fg=" + a.claudeFg + "]")
	default:
		b.WriteString("#[fg=" + a.thmMauve + "]")
	}
	// range=left marks the session name as a click target; MouseDown1StatusLeft
	// opens the session picker.
	b.WriteString(" #[range=left]" + a.iconSession + " " + a.session + "#[norange]  ")

	// Remote-bridge mirror window (#167 @bridge_win opt-out): the active
	// window's identity belongs to the remote session, not this host repo —
	// stop at the session pill, after naming the machine it really runs on.
	if a.bridgeWin == "1" {
		if a.bridgeHost != "" {
			b.WriteString("#[fg=" + a.thmPeach + "]" + a.iconRemote + " " + a.bridgeHost + "  ")
		}
		// Additive, like @pr_draft on a PR badge: names the mirror's own frozen
		// state without touching the host badge above it.
		if a.bridgeState != "" {
			b.WriteString("#[fg=" + a.thmRed + "]" + bridgeDisconnectedGlyph + "  ")
		}
		if a.bridgeCrewName != "" {
			fg := a.bridgeCrewColor
			if fg == "" {
				fg = a.thmMauve
			}
			b.WriteString("#[fg=" + fg + "]" + a.bridgeCrewName + "  ")
		}
		switch {
		case a.bridgeLabelID != "":
			// @bridge_label_id already carries the provider glyph
			// ("<icon> <id>"); @bridge_label_rest_long already carries its
			// own leading space in this arm. No separator here would double
			// either.
			b.WriteString("#[fg=" + a.thmBlue + ",bold]" + a.bridgeLabelID +
				"#[fg=" + a.thmText + ",nobold]" + a.bridgeLabelRestLong)
		case a.bridgeLabelRestLong != "":
			b.WriteString("#[fg=" + a.thmBlue + ",bold]" + a.iconBranch + " " + a.bridgeLabelRestLong)
		}
		return b.String()
	}

	// Agent-codename badge for the active window (fan-out harness stamp). Tinted
	// by its @crew_color when set; the issue/branch block below re-sets fg.
	// Gated on @window_has_agent so the badge doesn't outlive the agent that
	// earned it (#671).
	if a.crewName != "" && a.windowHasAgent == "1" {
		fg := a.crewColor
		if fg == "" {
			fg = a.thmMauve
		}
		b.WriteString("#[fg=" + fg + "]" + a.crewName + "  ")
	}

	if a.issueID != "" && a.issueBranch == a.branch {
		glyph := a.iconGitHub
		if a.issueProvider == "linear" {
			glyph = a.iconLinear
		}
		b.WriteString("#[fg=" + a.thmBlue + ",bold]" + glyph + " " + a.issueID +
			" #[fg=" + a.thmText + ",nobold]" + a.issueTitle)
	} else {
		b.WriteString("#[fg=" + a.thmBlue + ",bold]" + a.iconBranch + " " +
			branchDisplay(a.branch, a.panePath))
	}
	return b.String()
}

// bridgeDisconnectedGlyph flags a mirror whose control connection dropped: the
// daemon is retrying, but the panes on screen are frozen, not live — the badge
// says so before the user mistakes a stale screen for a working one.
const bridgeDisconnectedGlyph = "󰲛" // nerd: nf-md-lan_disconnect

var wrappedRe = regexp.MustCompile(`^\.(.*)-wrapped$`)

// paneCmdDisplay names the command beside the pane icon. bridgeProc wins when
// set: a mirror pane runs the bridge renderer, so pane_current_command names
// that rather than the remote command on screen — the same preference the icon
// beside it takes (tmux-update-icons) and the agent scan in usage.go (#590).
func paneCmdDisplay(cmd, bridgeProc string) string {
	if bridgeProc != "" {
		cmd = bridgeProc
	}
	if m := wrappedRe.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	return cmd
}

// paneSlotKeep/paneSlotPad pin the trailing "<icon> <command>" unit to a fixed
// display width. #[align=right] anchors the END of the tail, so any width change
// here slides the whole right-hand block — including the usage segment —
// sideways (#260).
//
// tmux does the measuring, not Go: #{=/N/…:} truncates to N display cells and
// appends a 1-cell ellipsis, and #{p-N:}/#{pN:} pad to N cells with the same
// utf8_cstrwidth() that computes the right section's width in format-draw.c, so
// the pad cannot disagree with tmux's own layout. 16 cells fits the longest
// agent command in use (cursor-agent, 12) plus a 2-cell icon and a space.
//
// Measured empirically (the man page's prose is inverted from actual
// behaviour): `#{p-N:...}` pads on the LEFT (blanks before the content),
// `#{pN:...}` pads on the RIGHT (blanks after it) — do not "fix" this back to
// match the man page. Either way the total rendered width is exactly
// paneSlotPad cells, which is the only thing #260 cares about (see paneSlot),
// so choosing a side never reintroduces that jitter.
const (
	paneSlotKeep = 16
	// Derived, not independent: bumping keep without the pad would let the
	// truncated value overflow its slot and re-open #260.
	paneSlotPad = paneSlotKeep + 1 // truncated width + the 1-cell ellipsis
)

// slotSafe drops the three bytes that would break the slot's fixed width. tmux
// counts braces, so a `}` in the value closes #{l:} early and leaks the rest out
// as literal text past the padding (`foo}bar` measured 18 cells, not 17); `#`
// can open a `#[...]` style run that format_draw strips only AFTER #{p-} has
// already padded for its width. A process name or icon holds none of them —
// but pane_current_command is argv[0], which a process picks for itself.
func slotSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '#', '{', '}':
			return -1
		}
		return r
	}, s)
}

// paneSlot renders the icon+command unit at exactly paneSlotPad cells. #{l:} is
// required, not defensive: a modifier's argument is resolved as a format, so bare
// literal text would vanish entirely.
//
// adjacentToUsage flips which side of the box the reserved pad cells land on
// (#575): left-pad (today's default) leaves a command-length-dependent dead
// run between the usage segment and a short pane command, since usage is
// emitted immediately before this slot in renderLine. Right-pad instead
// pushes that slack to the command's right, so the command sits flush
// against usage — safe per the width invariant above. Gated on usage being
// non-empty: with no agent running there's nothing to be adjacent to, so
// that render stays byte-identical to before this parameter existed.
func paneSlot(icon, cmd string, adjacentToUsage bool) string {
	if adjacentToUsage {
		return fmt.Sprintf("#{p%d:#{=/%d/…:#{l:%s %s}}}",
			paneSlotPad, paneSlotKeep, slotSafe(icon), slotSafe(cmd))
	}
	return fmt.Sprintf("#{p-%d:#{=/%d/…:#{l:%s %s}}}",
		paneSlotPad, paneSlotKeep, slotSafe(icon), slotSafe(cmd))
}

func renderLine(a args, claudeDir, theme string, prefixActive bool, now int64, usage string) string {
	var b strings.Builder
	b.WriteString("#[align=left,bg=" + a.thmBg + "]")
	b.WriteString(sessionSegment(a, prefixActive))
	// Remote-bridge mirror window: dir belongs to the host repo the launcher
	// ran in, not the remote content this window mirrors — suppress.
	if a.bridgeWin != "1" {
		b.WriteString("  #[fg=" + a.thmSubtext0 + ",nobold]" + a.iconDir + " " + dirDisplay(a.panePath, a.gitRoot))
	}
	b.WriteString("  #[fg=" + a.thmOverlay1 + "]" + claudeSegment(claudeDir, a.session, theme, now))
	b.WriteString(" #[align=right]") // literal space mirrors `#(claude) #[align=right]` in the old format
	b.WriteString(usage)
	b.WriteString("#[fg=" + a.thmSubtext0 + "]" + paneSlot(a.paneIcon, paneCmdDisplay(a.paneCmd, a.bridgeProc), usage != "") + " ")
	return b.String()
}

func main() {
	var a args
	// Only stable args here (see volatileFields). Session stays an arg — stable
	// per client, but needed to keep the #() distinct across clients on
	// different sessions.
	flag.StringVar(&a.session, "session", "", "")
	flag.StringVar(&a.thmBg, "thm-bg", "", "")
	flag.StringVar(&a.thmRed, "thm-red", "", "")
	flag.StringVar(&a.thmMauve, "thm-mauve", "", "")
	flag.StringVar(&a.thmBlue, "thm-blue", "", "")
	flag.StringVar(&a.thmText, "thm-text", "", "")
	flag.StringVar(&a.thmSubtext0, "thm-subtext0", "", "")
	flag.StringVar(&a.thmOverlay1, "thm-overlay1", "", "")
	flag.StringVar(&a.thmPeach, "thm-peach", "", "")
	flag.StringVar(&a.thmGreen, "thm-green", "", "")
	flag.StringVar(&a.iconSession, "icon-session", "", "")
	flag.StringVar(&a.iconBranch, "icon-branch", "", "")
	flag.StringVar(&a.iconDir, "icon-dir", "", "")
	flag.StringVar(&a.iconLinear, "icon-linear", "", "")
	flag.StringVar(&a.iconGitHub, "icon-github", "", "")
	flag.StringVar(&a.iconRemote, "icon-remote", "", "")
	flag.StringVar(&a.iconUsageClaude, "icon-usage-claude", "", "")
	flag.StringVar(&a.iconUsageCodex, "icon-usage-codex", "", "")
	flag.StringVar(&a.iconUsageCursor, "icon-usage-cursor", "", "")
	flag.IntVar(&a.usageMonthlyThreshold, "agent-usage-monthly-threshold", 0, "")
	flag.Parse()

	// Land a complete line before the slow work below (display-message, git,
	// list-panes). tmux publishes a #()'s output only on a newline-terminated
	// line (format.c:336), and once a job has run >1s with nothing published it
	// paints <'CMD' not ready> over the segment (format.c:446) — carrying this
	// job's ~1000-char argv. `echo;` fixes that for the side-effect-only
	// tickers but cannot be used here: publishing an empty line every tick
	// would blank line 0. The cached frame keeps the previous content painted,
	// and tmux keeps only the LAST complete line, so the render below wins.
	lastGood, hadLastGood := readLastGood(statuslineCacheDir, a.session)
	if hadLastGood {
		os.Stdout.WriteString(lastGood + "\n")
	} else {
		os.Stdout.WriteString("\n")
	}

	prefixActive, ok := a.fetchVolatile()

	claudeDir := os.Getenv("CLAUDE_STATUS_DIR")
	if claudeDir == "" {
		claudeDir = "/tmp/claude-status"
	}

	// On a failed fetch the volatile fields are empty; the last-good frame
	// already on stdout stands, so a transient timeout (common under load)
	// doesn't flash a degraded one. Cold start has no cache and renders below.
	if !ok && hadLastGood {
		return
	}

	// Usage segment: cheapest gates first — reading the three cache files
	// forks nothing, so the per-second tmux list-panes gate only runs when
	// there's actual data to display.
	usage := ""
	if a.usageMonthlyThreshold > 0 {
		usageDir := os.Getenv("OG_AGENT_USAGE_DIR")
		if usageDir == "" {
			usageDir = usageCacheDir
		}
		if caches := loadUsageCaches(usageDir); len(caches) > 0 && agentsRunning() {
			usage = usageSegment(a, caches, time.Now().Unix())
		}
	}

	// One job-output line is one status frame, and tmux publishes only the LAST
	// complete one — so an embedded newline (reachable through a path component)
	// would leave just the fragment after it on line 0. Collapse before this
	// escapes to stdout or the cache.
	line := strings.ReplaceAll(
		renderLine(a, claudeDir, themestate.Detect(), prefixActive, time.Now().Unix(), usage), "\n", " ")
	if ok {
		writeLastGood(statuslineCacheDir, a.session, line)
	}
	os.Stdout.WriteString(line + "\n")
}
