// Command daemon is the production entrypoint: it opens an ssh -CC
// control-mode connection to a remote tmux, mirrors every window of the
// bridged session into its own local window, and runs until the remote
// session exits or the connection drops. See picker/remotebridge/daemon for
// the orchestration.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/daemon"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

// How long the control connection may go unanswered before ssh gives up on it
// and exits, which is what surfaces a dead remote to this process (#471).
//
// The product is the detection window: 15s x 4 = 60s of total silence. Probes
// are sent only while the connection is idle and cost a round-trip each, so the
// interval is cheap; the count is what keeps a transient blip from tearing down
// a live mirror, since a mirror torn down is every window of it gone.
const (
	serverAliveInterval = 15
	serverAliveCountMax = 4
)

// sshControlArgs builds the argv for the control-mode ssh. Extracted so the
// options below are assertable — every one of them is load-bearing and none of
// them is visible in a passing test otherwise.
func sshControlArgs(ctlSock, host, tmpdir, term, colorterm, termProgram, session string, tmuxArgv []string) []string {
	args := []string{"-T", "-e", "none",
		// ControlMaster on the control connection makes every image fetch a
		// multiplexed exec on this same TCP connection: no second handshake,
		// and image bytes never share the control stream with live output.
		// ControlPersist=no ties the master's lifetime to this process.
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + ctlSock, "-o", "ControlPersist=no",
		// Keepalives are how this process finds out the remote is gone. A host
		// that dies abruptly — a reboot, a yanked cable — sends no FIN and no
		// RST, so the TCP connection stays open and ssh sits on it
		// indefinitely. Without these the control stream never reaches EOF, so
		// the daemon's teardown is never reached and the mirror goes on
		// presenting stale screens while discarding every keystroke (#471).
		// Observed on a rebooted host: hours in that state.
		"-o", "ServerAliveInterval=" + strconv.Itoa(serverAliveInterval),
		"-o", "ServerAliveCountMax=" + strconv.Itoa(serverAliveCountMax),
		host, "--", "env", "TMUX_TMPDIR=" + tmpdir}
	// TERM decides the remote viewer's graphics backend: it reads
	// #{client_termname}, which is whatever this control client advertises.
	// term is newSSHDialCmd's fresh read of Viewing.Desired() (#574) — the
	// identity of whichever terminal is currently VIEWING the mirror, not the
	// one that launched it — because tmux exposes no runtime setter for a
	// live client's termname: the only way to change what the remote sees is
	// to dial a new control client with a new TERM, which is exactly what
	// replaceConn and reattach do. -T means no pty, so ssh won't send TERM
	// itself — it has to ride in this env prefix like TMUX_TMPDIR does.
	if term != "" {
		args = append(args, "TERM="+shellQuote(term))
	}
	// COLORTERM/TERM_PROGRAM are in tmux-og's update-environment
	// (config/tmux.conf.nix) alongside TERM, so a bridged attach that carries
	// none of them into the remote tmux gets both marked "explicitly removed"
	// (#543) — every later program in that session loses truecolor detection.
	//
	// TERMINFO/TERMINFO_DIRS are local filesystem paths and stay out of this
	// list: forwarding them would point the remote at a directory that
	// doesn't exist there. They (and KITTY_LISTEN_ON) stay marked "removed"
	// on a bridged remote.
	if colorterm != "" {
		args = append(args, "COLORTERM="+shellQuote(colorterm))
	}
	if termProgram != "" {
		args = append(args, "TERM_PROGRAM="+shellQuote(termProgram))
	}
	args = append(args, tmuxArgv...)
	// ssh space-joins the post-host argv into one string run by the remote
	// login shell, so shell-quote the session name (may contain spaces) to keep
	// it a single target token.
	return append(args, "-C", "attach-session", "-t", shellQuote(session))
}

// newSSHDialCmd builds one ssh-branch dial's *exec.Cmd. Extracted out of the
// newCtlCmd closure so the read of view.Desired() happens on every call —
// through this function's own parameter, evaluated fresh each time it runs —
// rather than being captured once when newCtlCmd itself was built. That
// distinction is the whole point of Viewing's two cells: Desired can change
// between dials (a resolve, a raise), and a version of this that captured it
// early would dial with the SAME termname forever, no matter how many times
// SetDesired ran in between.
func newSSHDialCmd(sshCmd, host, tmpdir, path, colorterm, termProgram, session string, tmuxArgv []string, view *daemon.Viewing) *exec.Cmd {
	return exec.Command(sshCmd, sshControlArgs(path, host, tmpdir, view.Desired(), colorterm, termProgram, session, tmuxArgv)...)
}

// localCtlCmdEnv is the --test-local / no-ssh branches' equivalent of
// sshControlArgs' TERM=... prefix: with no ssh transport there is no argv to
// carry it in, so it rides cmd.Env instead — read from the same cell so both
// branches advertise exactly what the ssh branch's argv would. Appending to
// os.Environ() rather than assigning cmd.Env wholesale: a bare assignment
// would drop TMUX_TMPDIR/HOME/PATH and break the offline bats harness, which
// is exactly what these two branches exist to run under.
func localCtlCmdEnv(view *daemon.Viewing) []string {
	return append(os.Environ(), "TERM="+view.Desired())
}

// remoteStoreScript is the paste upload's remote half (#361): it lands the
// image bytes (stdin) in a fresh 0700 mktemp -d directory and prints the path
// of a plain-named file inside it. The find sweep is the cleanup answer — the
// file must outlive prompt editing (the agent reads the path at SUBMIT,
// possibly minutes later), so delete-on-inject is wrong and a per-paste
// janitor needs no daemon state.
//
// The directory is per-invocation (mktemp -d's random suffix) rather than a
// fixed shared path: a fixed path can be pre-created by another local account
// ahead of the victim's first paste (any owner, any mode, or a symlink), which
// escalates from image substitution to an arbitrary-file overwrite as the
// victim's own uid. mktemp -d's create is atomic and unpredictable, so there
// is nothing to pre-create, and its 0700 mode (by construction, not umask)
// means no other local account can write into it at all. It also sidesteps
// the unverified BSD/macOS mktemp question of a suffix trailing the X's,
// since the extension no longer rides in the template.
//
// It runs under sh -c exactly like graphics' remoteFetch: ssh space-joins the
// post-host argv for the remote LOGIN shell (fish on the normal host), which
// never parses the script because shellQuote makes it one argv element.
// $1 is the extension, validated against pasteExtRe before it is ever
// interpolated.
const remoteStoreScript = `umask 077
d=$(mktemp -d /tmp/og-paste-XXXXXXXX) || exit 1
find /tmp -maxdepth 1 -name "og-paste-*" -type d -mmin +60 -exec rm -rf {} + 2>/dev/null
f="$d/img.$1"
cat > "$f" || { rm -rf "$d"; exit 1; }
printf "%s" "$f"`

// pasteExtRe gates the extension before it reaches the remote mktemp
// template. bmp is absent on purpose: the agent's path-inlining regex
// excludes it, so shipping one would plant a path nothing reads.
var pasteExtRe = regexp.MustCompile(`^(png|jpe?g|gif|webp)$`)

// pasteUploadArgs builds the ssh argv for one image upload, riding the
// control connection's ControlMaster exactly like the graphics fetcher: no
// second handshake, and image bytes never share the control stream with live
// terminal output. Extracted so the argv is assertable.
func pasteUploadArgs(sshCmd, ctlSock, host, ext string) []string {
	return []string{sshCmd, "-S", ctlSock, "-T", host, "--",
		"sh", "-c", shellQuote(remoteStoreScript), "_", ext}
}

func main() {
	// Flags default to OG_BRIDGE_*/OG_DAEMON_* env vars, mirroring
	// M1's remotebridge/main.go: the launcher passes untrusted, remote-derived
	// values through tmux's environment rather than interpolating them into a
	// /bin/sh command string.
	host := flag.String("host", os.Getenv("OG_BRIDGE_HOST"), "ssh host")
	session := flag.String("session", os.Getenv("OG_BRIDGE_SESSION"), "remote session")
	window := flag.Int("window", envInt("OG_BRIDGE_WINDOW"), "initially-selected remote window index (all windows are mirrored)")
	remoteTmux := flag.String("tmux", envDefault("OG_BRIDGE_TMUX", "tmux"), "absolute remote tmux path")
	tmpdir := flag.String("tmpdir", os.Getenv("OG_BRIDGE_TMPDIR"), "remote TMUX_TMPDIR")
	sshCmd := flag.String("ssh", envDefault("OG_BRIDGE_SSH", "ssh"), "control transport command (empty = run tmux locally)")
	term := flag.String("term", os.Getenv("OG_BRIDGE_TERM"), "termname to advertise to the remote (steers the remote viewer's graphics backend)")
	termfeatures := flag.String("termfeatures", os.Getenv("OG_BRIDGE_TERMFEATURES"), "raw #{client_termfeatures} of the client that will paint (diagnostic only; see -sixel)")
	sixelFlag := flag.String("sixel", os.Getenv("OG_BRIDGE_SIXEL"), "tmux's own #{I/f:sixel} verdict for the launching client (gates the initial sixel-relay seed)")
	// A genuinely empty value must stay empty (and be omitted by
	// sshControlArgs' if-non-empty guard) rather than default to "truecolor",
	// since that would be indistinguishable from a real client that has none.
	colorterm := flag.String("colorterm", os.Getenv("OG_BRIDGE_COLORTERM"), "COLORTERM to advertise to the remote (#543)")
	termProgram := flag.String("term-program", os.Getenv("OG_BRIDGE_TERM_PROGRAM"), "TERM_PROGRAM to advertise to the remote (#543)")
	cacheDir := flag.String("gfx-cache", envDefault("OG_BRIDGE_GFX_CACHE", filepath.Join(os.TempDir(), "og-gfx")), "local cache dir for images fetched from the remote")
	gfxMax := flag.Int64("gfx-max-bytes", 8<<20, "largest single image fetched from the remote; bigger stores are dropped")
	gfxRelayMaxBytes := flag.Int64("gfx-relay-max-bytes", graphics.DefaultRasterHold, "byte budget for holding a partial sixel meant for relay; bigger holds are dropped")
	localTmux := flag.String("local-tmux", envDefault("OG_DAEMON_LOCAL_TMUX", "tmux"), "local tmux binary (may carry args, e.g. \"tmux -L sock\")")
	localSess := flag.String("local-sess", os.Getenv("OG_DAEMON_LOCAL_SESS"), `local session name (default "<host>-<session>")`)
	sock := flag.String("sock", os.Getenv("OG_DAEMON_SOCK"), "unix socket path for renderers")
	rendererBin := flag.String("renderer", os.Getenv("OG_DAEMON_RENDERER"), "absolute path to the renderer binary")
	reflowBin := flag.String("reflow", os.Getenv("OG_DAEMON_REFLOW"), "absolute path to tmux-reflow-windows (empty = never force a reflow)")
	remoteOpenBin := flag.String("remote-open", os.Getenv("OG_DAEMON_REMOTE_OPEN"), "absolute path to og-remote-open (empty = a remote switch-client is pinned back but never handed off)")
	pauseAfter := flag.Int("pause-after", envIntDefault("OG_DAEMON_PAUSE_AFTER", 1), "seconds of client-read stall before tmux pauses a pane's %output (0 disables); the daemon answers %pause with a %continue re-seed")
	// --test-local is Task 9's offline seam: instead of ssh, both "remote" and
	// "local" are separate local tmux servers on their own -L sockets, so the
	// bats integration test never touches the network. --session/--window
	// still name the "remote" target (a real session:window on --src-socket);
	// only the transport differs.
	testLocal := flag.Bool("test-local", false, "test only: mirror --session:--window from a local tmux -L --src-socket instead of ssh")
	srcSocket := flag.String("src-socket", "", "test-local: tmux -L socket name standing in for the remote server")
	dstSocket := flag.String("dst-socket", "", "test-local: tmux -L socket name standing in for the local server")
	retryMaxElapsed := flag.Duration("retry-max-elapsed", envDurationDefault("OG_DAEMON_RETRY_MAX_ELAPSED", 0), "test only: bound the reattach retry schedule's MaxElapsed (0 = production schedule)")
	wakeMaxElapsed := flag.Duration("wake-max-elapsed", envDurationDefault("OG_DAEMON_WAKE_MAX_ELAPSED", 0), "test only: bound the parked-wake retry schedule's MaxElapsed (0 = production schedule)")
	flag.Parse()

	if *localSess == "" {
		*localSess = fmt.Sprintf("%s-%s", *host, *session)
	}
	if *sock == "" {
		*sock = fmt.Sprintf("%s/og-daemon-%d.sock", os.TempDir(), os.Getpid())
	}

	// view is the daemon's live view-identity cell (see Viewing),
	// declared here — before newCtlCmd — because a later step's dial argv
	// reads Viewing.Desired() straight from it (H2-A): a closure built further
	// down needs the identifier already in scope. Fully constructed and seeded
	// below, once LocalTmuxOut exists to resolve against.
	var view *daemon.Viewing

	// newCtlCmd is the transport as a *recipe* the daemon can re-run on every
	// reconnect, rather than one already-built command. Every branch gets one,
	// the offline --test-local seam included — that is the seam the reconnect
	// integration tests drive. It also mints the ControlPath for this
	// particular dial (R9): "" on the two branches with no ssh control socket,
	// otherwise a fresh path per call, never a name stable across re-dials.
	var newCtlCmd func() (*exec.Cmd, string)
	var ctlSock string
	var localTmuxArgv []string
	if *testLocal {
		newCtlCmd = func() (*exec.Cmd, string) {
			cmd := exec.Command("tmux", "-L", *srcSocket, "-C", "attach-session", "-t", *session)
			cmd.Env = localCtlCmdEnv(view)
			return cmd, ""
		}
		localTmuxArgv = []string{"tmux", "-L", *dstSocket}
	} else {
		// remoteTmux/localTmux may carry args (e.g. "tmux -L sock" for
		// tests), so split into argv rather than passing as a single token.
		tmuxArgv := strings.Fields(*remoteTmux)
		if *sshCmd == "" {
			newCtlCmd = func() (*exec.Cmd, string) {
				cmd := exec.Command(tmuxArgv[0], append(append([]string{}, tmuxArgv[1:]...),
					"-C", "attach-session", "-t", *session)...)
				cmd.Env = localCtlCmdEnv(view)
				return cmd, ""
			}
		} else {
			// ctlSock itself stays the fixed, pid-derived name — it is kept only
			// as the "an ssh control socket exists" sentinel the pasteUpload and
			// NewGraphics wiring below branch on. The path that actually reaches
			// -S is minted fresh on every call, below: R7 needs two live masters
			// open at once during a replacement's overlap, which a name stable
			// across re-dials cannot give, and a stale ControlPath is what makes
			// ControlMaster=auto silently stop multiplexing — so both consumers
			// read the live path through tr.currentPath instead (wired at their
			// call sites below).
			ctlSock = fmt.Sprintf("%s/og-bridge-%d.sock", os.TempDir(), os.Getpid())
			var dialN int
			newCtlCmd = func() (*exec.Cmd, string) {
				dialN++
				path := fmt.Sprintf("%s/og-bridge-%d-%d.sock", os.TempDir(), os.Getpid(), dialN)
				return newSSHDialCmd(*sshCmd, *host, *tmpdir, path, *colorterm, *termProgram, *session, tmuxArgv, view), path
			}
		}
		localTmuxArgv = strings.Fields(*localTmux)
	}

	tr := &transport{}

	// ControlPersist=no ties each ControlMaster socket's lifetime to its own
	// ssh process, so nothing but this daemon will ever unlink it — ssh cleans
	// up its own ControlPath only when it exits normally or catches a signal,
	// and a SIGKILL (child.end's fallback) it cannot catch. child.Close unlinks
	// the path of the child it closes; cleanup is the backstop for whatever is
	// still tracked (open) when the daemon exits without every child having
	// gone through Close first.
	// Called explicitly at every exit path rather than deferred: fatal() calls
	// os.Exit(1), which skips deferred functions.
	cleanup := func() {
		for _, p := range tr.paths() {
			os.Remove(p)
		}
	}

	dial := func() (io.ReadWriteCloser, error) {
		cmd, path := newCtlCmd()
		c, err := newChild(cmd, path)
		if err != nil {
			return nil, err
		}
		if err := tr.start(c); err != nil {
			return nil, err
		}
		return c, nil
	}

	// On SIGTERM/SIGINT, ask the control transport to exit so daemon.Run's
	// reader hits EOF and its teardown runs (removes the socket + pidfile,
	// kills the local mirror session). See child.Close for how it is ended.
	//
	// stop is closed BEFORE the transport is touched, so the daemon reads the
	// EOF that follows as a detach rather than as a link failure to retry.
	// With reconnect, a transport this handler kills EOFs exactly like a
	// dropped one, so stop is the only thing that tells the daemon "the user
	// asked to detach" from "the link died" — and during a backoff sleep there
	// is no transport for the signal to reach at all.
	stop := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		close(stop)
		tr.stop()
	}()

	runLocalTmux := func(args ...string) error {
		cmd := exec.Command(localTmuxArgv[0], append(append([]string{}, localTmuxArgv[1:]...), args...)...)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	runLocalTmuxOut := func(args ...string) (string, error) {
		cmd := exec.Command(localTmuxArgv[0], append(append([]string{}, localTmuxArgv[1:]...), args...)...)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		return string(out), err
	}
	area := func() (int, int) { return localArea(localTmuxArgv, *localSess) }
	// -b so the reflow's own tmux round-trips stay off the command queue the
	// daemon is about to use again.
	reflow := func() {
		if *reflowBin == "" {
			return
		}
		runLocalTmux(reflowRunShellArgs(*reflowBin, *localSess)...)
	}
	panes := func() map[string]string { return localPaneMap(localTmuxArgv, *localSess) }
	// A remote switch-client that moved this client is pinned back; handOff then
	// opens the session it named as a mirror of its own, so `sesh connect` from a
	// bridged shell lands somewhere instead of no-opping. The launcher switches
	// the local client to the new mirror itself.
	var handOff func(string)
	if *remoteOpenBin != "" {
		handOff = func(sess string) {
			cmd := exec.Command(*remoteOpenBin, *host, sess)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: hand off %s:%s: %v\n", *host, sess, err)
			}
		}
	}

	// The paste upload rides the same ControlMaster as the graphics fetcher and
	// is disabled for the same reason when there is no ssh transport at all —
	// ctlSock (the static, pid-derived sentinel) still gates that decision, but
	// the path actually handed to -S is read through tr.currentPath() on every
	// upload, so a paste started after a reconnect never dials through a
	// ControlPath the daemon has already closed (R9).
	var pasteUpload func(ctx context.Context, ext string, data []byte) (string, error)
	if ctlSock != "" {
		pasteUpload = func(ctx context.Context, ext string, data []byte) (string, error) {
			if !pasteExtRe.MatchString(ext) {
				return "", fmt.Errorf("paste: bad extension %q", ext)
			}
			args := pasteUploadArgs(*sshCmd, tr.currentPath(), *host, ext)
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			cmd.Stdin = bytes.NewReader(data)
			out, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("paste store: %w", err)
			}
			return strings.TrimSpace(string(out)), nil
		}
	}

	// Seeded from the mirror session's own resolved clients when one is
	// attached yet (R1/R3) — the launcher's -term/-termfeatures flags sample
	// only the INVOKING client (og-remote-open.sh:475), while this
	// resolves LocalSess's own AND'd capability and lexicographic termname,
	// which is what makes acceptance 2's "never nacked" a property of the code
	// rather than a coincidence. The flags remain the fallback for the startup
	// instant before the launcher has switched a client onto the mirror
	// session yet.
	view = seedView(func() (daemon.ViewIdentity, bool) {
		return daemon.ResolveLocalViewIdentity(runLocalTmuxOut, *localSess)
	}, *term, *termfeatures, *sixelFlag)

	cfg := daemon.Config{
		Dial:           dial,
		Shutdown:       stop,
		SockPath:       *sock,
		LocalSess:      *localSess,
		RemoteHost:     *host,
		RemoteSession:  *session,
		RemoteWindow:   strconv.Itoa(*window),
		PauseAfterSecs: *pauseAfter,
		RendererBin:    *rendererBin,
		LocalTmux:      runLocalTmux,
		LocalTmuxOut:   runLocalTmuxOut,
		LocalArea:      area,
		Reflow:         reflow,
		LocalPanes:     panes,
		HandOff:        handOff,
		PasteUpload:    pasteUpload,
		View:           view,
		NewGraphics:    newGraphics(ctlSock, tr.currentPath, *host, *cacheDir, *gfxMax, view.Relay, *gfxRelayMaxBytes),
	}
	if *retryMaxElapsed > 0 {
		b := daemon.DefaultBackoff(time.Now)
		b.MaxElapsed = *retryMaxElapsed
		cfg.Retry = &b
	}
	if *wakeMaxElapsed > 0 {
		b := daemon.WakeBackoff(time.Now)
		b.MaxElapsed = *wakeMaxElapsed
		cfg.WakeRetry = &b
	}

	err := daemon.Run(cfg)
	cleanup()
	if err != nil {
		fatal(err)
	}
}

// seedView builds Config.View: the pair every dial and the raise guard will
// share (see Viewing). resolve is tried first — the mirror
// session's own resolved clients, when one is attached yet (R1) — and the
// -term/-termfeatures/-sixel flags stand only when resolve reports nothing
// (R3), which is the startup instant before the launcher has switched a
// client onto the mirror session. sixel is the launching client's own
// tmux-interrogated #{I/f:sixel} verdict (R6) — termfeatures is carried only
// for the diagnostic, never re-derived into a capability here. Desired and
// Advertised seed to the SAME value either way: the very first dial this
// process makes really will carry it, so there is no prior client for the two
// to disagree about (Viewing.Seed's contract).
func seedView(resolve func() (daemon.ViewIdentity, bool), term, termfeatures, sixel string) *daemon.Viewing {
	view := &daemon.Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(sixel == "1", termfeatures))}
	seedTerm := term
	if id, ok := resolve(); ok {
		view.Relay.Store(id.Relay)
		seedTerm = id.Term
	}
	view.Seed(seedTerm)
	return view
}

// newGraphics builds the Config.NewGraphics closure. A proxy is placed on
// EVERY transport (R8): ctlSock (the static, pid-derived sentinel — never the
// live path) is the only thing that distinguishes the two branches — an ssh
// control socket means a real remote filesystem exists to fetch kitty images
// from, so that branch gets the full localising proxy; --test-local / -ssh ""
// has no remote filesystem, so loc stays nil and the proxy runs relay-only
// (R7) — raster policy only, kitty forwarded byte-identically. src and hold
// are the same values on both branches: the relay value must never be what
// tells the branches apart, or sixel relay would work only under
// --test-local and be dead on every real transport.
//
// ctlSockFn is the accessor a constructed fetcher reads from on every fetch
// (tr.currentPath in production) — distinct from ctlSock on purpose: the
// branch decision above is made once, at wiring time, possibly before any
// dial, while ctlSockFn must track whichever ControlPath is live right now.
// Conflating the two would put every real ssh bridge on the relay-only branch
// the instant ctlSock's static sentinel were swapped for a live-but-still-
// empty read.
func newGraphics(ctlSock string, ctlSockFn func() string, host, cacheDir string, gfxMax int64, src *graphics.RelaySource, hold int64) func(string) *graphics.Proxy {
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	return func(string) *graphics.Proxy {
		var loc graphics.Localizer
		if ctlSock != "" {
			loc = graphics.NewSSHFetcher(host, ctlSockFn, cacheDir, gfxMax)
		}
		return graphics.NewRelay(loc, logf, src, hold)
	}
}

// localArea reports the content area the local mirror session can display,
// used as the cap asserted on every mirrored remote window.
// Defaults to 80x24 if neither query yields a size.
func localArea(localTmuxArgv []string, localSess string) (int, int) {
	if w, h := clientArea(localTmuxArgv, localSess); w > 0 && h > 0 {
		return w, h
	}
	// No client on THIS session, which is the ordinary state of every mirror the
	// user is not currently looking at: each bridged session has its own daemon,
	// and only one of them is on screen. Ask the local server instead — the
	// terminal the user is actually sitting at is the size this mirror will be
	// shown at the moment they switch to it.
	//
	// The most recently active client, not the smallest: these are clients on
	// other sessions entirely, and none of them will ever display this mirror.
	// Minimising over them let a second terminal left open at 63 columns cap
	// the remote session's windows — and FitWindowCmd then pinned the mirror to
	// that, so a 197-column terminal drew a 63-column window and padded the
	// rest. Only clientArea's own -t reading has a session's windows to fit.
	//
	// Not sessionWinSize here: FitWindowCmd pins the mirror window to the
	// remote's size (window-size manual), so its dims are this daemon's own last
	// assertion. Reading them back makes every resize look like "no change" to
	// the converger, and the mirror then keeps the stale size until watchLocalClient's
	// 30s fallback poll happens to run with a client attached (#532).
	if w, h := latestClientArea(localTmuxArgv); w > 0 && h > 0 {
		return w, h
	}
	// Nothing attached anywhere. The pin is now the best answer available, and
	// asserting it is a no-op — which is the point: 80x24 below would actively
	// shrink a remote nobody is watching.
	if w, h := sessionWinSize(localTmuxArgv, localSess); w > 0 && h > 0 {
		return w, h
	}
	return 80, 24
}

// localPaneMap reads the mirror session's panes back as remote pane id -> local
// pane id. The daemon stamps @bridge_pane when it spawns each renderer, so tmux
// itself holds the mapping the agent-status shipper needs to re-key the remote's
// state onto local pane ids.
func localPaneMap(localTmuxArgv []string, localSess string) map[string]string {
	out, err := exec.Command(localTmuxArgv[0], append(append([]string{}, localTmuxArgv[1:]...),
		"list-panes", "-s", "-t", localSess, "-F", "#{@bridge_pane} #{pane_id}")...).Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		m[fields[0]] = fields[1]
	}
	return m
}

// clientArea is the smallest attached client's content area: its size minus
// its status lines. It reads the clients rather than #{window_width} because
// the daemon pins each mirror window to its remote's size (window-size
// manual), so a mirror window's own dims no longer track the terminal it is
// shown in. Returns 0,0 when no client is attached.
//
// Minimised because every client it reads is displaying localSess, so the
// session's windows have to fit all of them. An empty localSess is a different
// question with a different answer — see latestClientArea.
func clientArea(localTmuxArgv []string, localSess string) (int, int) {
	w, h := 0, 0
	for _, c := range readClients(localTmuxArgv, localSess) {
		if w == 0 || c.w < w {
			w = c.w
		}
		if h == 0 || c.h < h {
			h = c.h
		}
	}
	return w, h
}

// latestClientArea is the content area of the whole server's most recently
// active client — localArea's fallback when the mirror session has no client
// of its own. Returns 0,0 when nothing is attached anywhere.
//
// An activity tie goes to the larger client. client_activity has one-second
// resolution, so ties are ordinary rather than exotic, and resolving one toward
// the smaller would reinstate exactly the cap this function exists to avoid.
func latestClientArea(localTmuxArgv []string) (int, int) {
	var best clientDims
	for _, c := range readClients(localTmuxArgv, "") {
		switch {
		case c.activity > best.activity:
			best = c
		case c.activity == best.activity && c.w*c.h > best.w*best.h:
			best = c
		}
	}
	return best.w, best.h
}

// clientDims is one attached client's content area and last-activity stamp.
type clientDims struct {
	w, h     int
	activity int64
}

// readClients returns every attached client's content area — its size minus
// its status lines — paired with the stamp of its last activity. An empty
// localSess drops the -t and reads every client on the local server.
func readClients(localTmuxArgv []string, localSess string) []clientDims {
	args := []string{"list-clients", "-F", "#{client_width} #{client_height} #{status} #{client_activity}"}
	if localSess != "" {
		args = append(args, "-t", localSess)
	}
	out, err := exec.Command(localTmuxArgv[0],
		append(append([]string{}, localTmuxArgv[1:]...), args...)...).Output()
	if err != nil {
		return nil
	}
	var clients []clientDims
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		cw, errW := strconv.Atoi(fields[0])
		ch, errH := strconv.Atoi(fields[1])
		if errW != nil || errH != nil {
			continue
		}
		ch -= statusLines(fields[2])
		if cw < 1 || ch < 1 {
			continue
		}
		activity, _ := strconv.ParseInt(fields[3], 10, 64)
		clients = append(clients, clientDims{w: cw, h: ch, activity: activity})
	}
	return clients
}

// sessionWinSize is the detached fallback: the local session's active-window
// content dims. Returns 0,0 if the session doesn't exist yet or the query
// fails.
func sessionWinSize(localTmuxArgv []string, localSess string) (int, int) {
	out, err := exec.Command(localTmuxArgv[0], append(append([]string{}, localTmuxArgv[1:]...),
		"display-message", "-p", "-t", localSess, "-F", "#{window_width} #{window_height}")...).Output()
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0
	}
	w, errW := strconv.Atoi(fields[0])
	h, errH := strconv.Atoi(fields[1])
	if errW != nil || errH != nil {
		return 0, 0
	}
	return w, h
}

// statusLines converts tmux's `status` option value — off, on, or 2-5 — into
// the number of rows the status bar takes off a client's height.
func statusLines(v string) int {
	switch v {
	case "off":
		return 0
	case "on":
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 1
	}
	return n
}

// transport holds every control-mode child the daemon is currently talking
// to. Normally that is one, but R7's replacement dials, verifies and converges
// a new master BEFORE closing the old one, so for the span of that overlap two
// are alive at once — and a SIGTERM landing in that window must still end
// both, or the survivor outlives the daemon holding the mirror's
// ControlMaster open. The signal handler and the dialer race over it
// otherwise: the handler is started once and closes over whatever is current,
// while the dialer adds to the set on every dial and reconnect.
type transport struct {
	mu sync.Mutex
	// children holds every live (started, not yet closed) child, oldest
	// first. A child drops itself the instant Close is called (see
	// child.Close), never merely when it finishes reaping — so the LAST
	// entry is always "the most recently started child that is still open",
	// currentPath's whole contract, with no separate liveness flag to keep
	// in sync against it.
	children []*child
	stopping bool
}

// start runs c and publishes it into the set, atomically against stop. A
// signal landing between the two would otherwise reach only the children
// already published — already dead and reaped, so signalling them is a
// no-op — and the ssh child just started would outlive the daemon, holding
// its own ControlMaster open. Once stop has run, a start that still wins the
// lock never publishes: it ends its own child instead of leaving a survivor
// stop can no longer see.
//
// c.tr is wired here rather than at newChild so a child that never starts
// (Start failed) never gains a back-reference into a set it was never added
// to. It is wired unconditionally, publish or not, so the Close call below —
// on the stopping path, made only after this function has released mu, to
// avoid child.Close re-entering this mutex through remove — can always drop
// itself safely, even from a child that was never published.
func (t *transport) start(c *child) error {
	t.mu.Lock()
	if err := c.cmd.Start(); err != nil {
		t.mu.Unlock()
		// No pipes to release: Start closes the parent's ends of the ones it
		// opened when it fails. And c.Close is not the tool if that ever
		// changes — it schedules end(), which signals c.cmd.Process, nil until
		// Start succeeds.
		return err
	}
	c.tr = t
	stopping := t.stopping
	if !stopping {
		t.children = append(t.children, c)
	}
	t.mu.Unlock()
	if stopping {
		c.Close()
	}
	return nil
}

// stop ends every currently tracked child and bars any later start from
// publishing a survivor. Only the signal handler calls it: stopping means
// "the user asked to detach", which a child ended because it was superseded
// or timed out is not — reading one as the other would stop the daemon on a
// reconnect. The snapshot is taken under the lock and Close is called outside
// it: child.Close calls back into remove, which takes the same lock, and
// mu is not reentrant.
func (t *transport) stop() {
	t.mu.Lock()
	t.stopping = true
	cs := append([]*child(nil), t.children...)
	t.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

// remove drops c from the tracked set. Called from child.Close so a child
// ends its own membership rather than the transport reaching back into it —
// the shape start's stopping path also depends on (see there). c not being
// present (already removed, or never published because start saw stopping)
// is a silent no-op; Close's sync.Once is what keeps this from running twice
// for the same child regardless.
func (t *transport) remove(c *child) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.children {
		if x == c {
			t.children = append(t.children[:i], t.children[i+1:]...)
			return
		}
	}
}

// currentPath returns the ControlPath of the most recently started child that
// is still open — the single accessor every consumer of the control socket
// reads through (R9). Deliberately NOT "the last dialled path": during a
// replacement's overlap this is the newer, connected master; once an aborted
// replacement's new child closes (dropping itself via remove), this falls
// back to the surviving older path; once a successful swap closes the old
// child, this reports the newer one. Getting this wrong is not cosmetic — a
// consumer stuck on a dead child's path passes ssh a -S that no longer
// exists, and ControlMaster=auto then silently stops multiplexing and
// re-authenticates per fetch with no tty.
func (t *transport) currentPath() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.children) == 0 {
		return ""
	}
	return t.children[len(t.children)-1].path
}

// paths reports the ControlPath of every currently tracked child, for
// cleanup's use at process exit: a child that never went through Close (the
// daemon returning before its own teardown reaches every connection) would
// otherwise leave its socket behind, since ControlPersist=no only unlinks it
// on a clean ssh exit.
func (t *transport) paths() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var paths []string
	for _, c := range t.children {
		if c.path != "" {
			paths = append(paths, c.path)
		}
	}
	return paths
}

// transportKillGrace is how long a control transport gets to exit on its own
// after SIGTERM before it is killed.
const transportKillGrace = 2 * time.Second

// child is one control-transport process as the io.ReadWriteCloser
// daemon.Config wants: read the control stream off its stdout, write commands
// to its stdin.
type child struct {
	cmd  *exec.Cmd
	out  io.ReadCloser
	in   io.WriteCloser
	once sync.Once
	// ended is closed once the process has been signalled and reaped.
	ended chan struct{}
	// path is this child's own ControlPath (R9); "" for a transport with no
	// ssh control socket (--test-local, or a bare-tmux remote). Close unlinks
	// it, which is why the value lives here rather than at the dial call site
	// — dial mints a fresh one per attempt, so there is no single "the" path
	// left for a shared unlink-before-dial to collect.
	path string
	// tr is the transport this child was (or would have been) published
	// under, wired by start regardless of whether it actually published —
	// see start's doc for why. Nil only for a child whose Start failed, which
	// never reaches Close through any live path.
	tr *transport
}

func newChild(cmd *exec.Cmd, path string) (*child, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	return &child{cmd: cmd, out: out, in: in, path: path, ended: make(chan struct{})}, nil
}

func (c *child) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *child) Write(p []byte) (int, error) { return c.in.Write(p) }

// Close ends this transport for good, and is the whole of what makes closing a
// connection mean anything. Closing stdin alone does not end one: ssh keeps
// reading until the REMOTE closes its side, so a far end that accepts the
// connection and then goes silent leaves the control reader parked forever —
// which is exactly the case the identity deadline exists for. Closing our own
// read end is what unparks the reader, and it does so whether or not the
// process cooperates.
//
// The unlink and the set removal happen here, synchronously, rather than from
// end()'s goroutine: end() can take up to transportKillGrace to reap a wedged
// process, but currentPath must stop seeing this child the instant Close is
// called, not once the process eventually exits (R9's "still open" is a
// membership question, not a process-liveness one).
//
// Never blocks: the grace window and the reap run on their own goroutine, so
// the deadline timer, a drop and teardown can each call this and get on with
// it. Idempotent, because the drop path and teardown both do.
func (c *child) Close() error {
	c.once.Do(func() {
		c.in.Close()
		c.out.Close()
		if c.path != "" {
			os.Remove(c.path)
		}
		if c.tr != nil {
			c.tr.remove(c)
		}
		go c.end()
	})
	return nil
}

// end signals the process and reaps it. SIGTERM first: ssh can catch it and
// unlink its own ControlPath, which main's cleanup only guarantees as a
// backstop. Kill after the grace window covers a wedged ssh — on an already
// exited process it reports ErrProcessDone rather than reaching a pid the
// kernel has since recycled, since os.Process holds a handle, which is also why
// the ordinary EOF case costs nothing here. Wait is what keeps a daemon that
// reconnects from leaving a zombie per attempt; it cannot outlast the grace
// window, since the kill is already scheduled when it starts.
func (c *child) end() {
	defer close(c.ended)
	kill := time.AfterFunc(transportKillGrace, func() { c.cmd.Process.Kill() })
	defer kill.Stop()
	c.cmd.Process.Signal(syscall.SIGTERM)
	c.cmd.Wait()
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "og-remote-daemon: %v\n", err)
	os.Exit(1)
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string) int {
	n, _ := strconv.Atoi(os.Getenv(key))
	return n
}

func envIntDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDurationDefault(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// shellQuote single-quotes s for a POSIX shell, escaping embedded single
// quotes. Used for the session name in the ssh remote-command argv.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// reflowRunShellArgs builds the run-shell argv that forces a reflow.
// run-shell format-expands its whole shell-command string before /bin/sh
// ever sees it, so a value embedded directly in that string is exposed to
// the tmux format layer — "#(...)" is a job introducer there, not just a
// POSIX shell metacharacter — and shellQuote's escaping (the shell layer)
// does nothing to stop that (#368). Passing reflowBin/localSess as trailing
// run-shell arguments and referencing them only via #{1}/#{2} keeps them out
// of the string tmux format-expands: argv values are substituted in
// literally, after format expansion has already run over the command
// template, so a "#(" embedded in either value can never be read back as a
// job introducer. shellQuote still runs first — argv closes the format-layer
// hole, it does not exempt the value from needing to be a single shell token.
func reflowRunShellArgs(reflowBin, localSess string) []string {
	return []string{"run-shell", "-b", "#{1} --force #{2}", shellQuote(reflowBin), shellQuote(localSess)}
}
