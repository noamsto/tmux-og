// Command ctl is the local half of the bridge's structural-input path. A tmux
// keybind inside a @bridge_win window runs it instead of acting on the mirror;
// it hands the verb and the pane's remote id to the bridge daemon over the
// daemon's unix socket, and the daemon runs the equivalent command on the remote.
//
// Usage: ctl --sock <path> <verb> <remote-pane-id> [args...]
//
//	ctl --sock <path> drag <local-pane-id>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// Deadlines exist so a dead or wedged daemon can never hold tmux's command
// queue. A stale socket with no listener fails instantly (ECONNREFUSED); a
// daemon that accepts but never answers fails at the overall deadline. The
// daemon acks as soon as it has queued the request — it absorbs all ssh latency
// behind its own fire-and-forget send — so this never waits on the network.
const (
	dialTimeout    = 250 * time.Millisecond
	overallTimeout = 2 * time.Second
	errorPrefix    = "og-remote-bridge-ctl: "
)

var runTmux = func(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

var runTmuxOut = func(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return string(out), err
}

var panePattern = regexp.MustCompile(`^%[0-9]+$`)

const dragFormat = "#{@bridge_pane}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{window_width}|#{window_height}|#{P:#{pane_id}=#{@bridge_pane} }|#{window_layout}"

// resolveDrag turns the local pane id a drag-end bind stashed into a request
// for the daemon. The bind cannot resolve this itself: its format context is
// wherever the button was released, and a `-t '#{…}'` target is never
// format-expanded — so ctl re-reads the stashed pane directly.
//
// A single display-message call reads the pane's own geometry, every local
// pane's `@bridge_pane` mapping (as P: pairs) and the local window's layout
// together, so the map and the layout describe the same instant — no risk of
// a mapping read here and a layout read a moment later racing a reflow.
//
// A floating pane returns today's float-geom request unchanged. A tiled pane
// instead maps the window's layout onto remote pane ids via @bridge_pane and
// returns a tile-layout request: the daemon reshapes the remote mirror to
// match. Routing the remap through @bridge_pane rather than assuming local
// and remote ids line up means a desynced mirror errors out here instead of
// reshaping the wrong remote panes.
func resolveDrag(localPane string) ([]string, error) {
	if !panePattern.MatchString(localPane) {
		return nil, fmt.Errorf("drag: bad local pane %q", localPane)
	}
	out, err := runTmuxOut("display-message", "-p", "-t", localPane, dragFormat)
	if err != nil {
		return nil, fmt.Errorf("drag: resolve %s: %w", localPane, err)
	}
	fields := strings.SplitN(strings.TrimSpace(out), "|", 10)
	if len(fields) != 10 {
		return nil, fmt.Errorf("drag: %s: want 10 fields, got %q", localPane, out)
	}
	bridgePane, floating := fields[0], fields[1]
	if !panePattern.MatchString(bridgePane) {
		return nil, fmt.Errorf("drag: %s: not a mirror pane (bad @bridge_pane %q)", localPane, bridgePane)
	}
	if floating == "1" {
		return append([]string{"float-geom", bridgePane, localPane}, fields[2:8]...), nil
	}

	remoteByLocal := make(map[string]string)
	for _, pair := range strings.Fields(fields[8]) {
		local, remote, ok := strings.Cut(pair, "=")
		if !ok || !panePattern.MatchString(remote) {
			continue
		}
		remoteByLocal[local] = remote
	}
	lookup := func(id string) (string, bool) {
		remote, ok := remoteByLocal[id]
		return remote, ok
	}
	L, err := controlmode.TiledLayout(fields[9], lookup)
	if err != nil {
		return nil, fmt.Errorf("drag: %s: %w", localPane, err)
	}
	return []string{"tile-layout", bridgePane, L.Raw}, nil
}

func main() {
	sock := flag.String("sock", os.Getenv("OG_DAEMON_SOCK"), "bridge daemon unix socket")
	displayError := flag.String("display-error", "", "tmux client to show request failures in")
	flag.Parse()

	if *sock == "" {
		fail("no --sock (is this a bridge window?)")
	}
	if flag.NArg() < 2 {
		fail("usage: ctl --sock <path> <verb> <remote-pane-id> [args...] | drag <local-pane-id>")
	}

	args := flag.Args()
	// The focus hook runs backgrounded, so its requests can reach the daemon out
	// of order. Stamp a sequence here rather than in the keybind — tmux formats
	// have no monotonic counter — so the daemon can drop a stale one.
	if args[0] == "focus" && len(args) == 2 {
		args = append(args, strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	if args[0] == "drag" && len(args) == 2 {
		resolved, err := resolveDrag(args[1])
		if err != nil {
			if *displayError != "" && showError(*displayError, err) == nil {
				return
			}
			fail(err.Error())
		}
		args = resolved
	}

	if err := run(*sock, args); err != nil {
		if *displayError != "" && showError(*displayError, err) == nil {
			return
		}
		fail(err.Error())
	}
}

func showError(client string, err error) error {
	return runTmux("display-message", "-d", "5000", "-t", client, errorPrefix+err.Error())
}

func run(sock string, args []string) error {
	conn, err := net.DialTimeout("unix", sock, dialTimeout)
	if err != nil {
		return fmt.Errorf("bridge daemon unreachable: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(overallTimeout)); err != nil {
		return err
	}

	argv := append([]string{wire.CtlProtocolVersion}, args...)
	if err := wire.WriteFrame(conn, wire.FrameCtl, wire.EncodeArgv(argv)); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	f, err := wire.ReadFrame(conn)
	if err != nil {
		// An older daemon does not know FrameCtl: it reads the first frame,
		// sees it is not a Hello, and closes. That arrives here as EOF, so say
		// what it means rather than reporting a bare read error.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("this bridge daemon does not speak the ctl protocol — reopen the bridge")
		}
		return fmt.Errorf("no ack: %w", err)
	}
	if f.Type != wire.FrameCtlAck {
		return fmt.Errorf("unexpected reply frame %d", f.Type)
	}
	if len(f.Payload) > 0 {
		return errors.New(string(f.Payload))
	}
	return nil
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, errorPrefix+msg)
	os.Exit(1)
}
