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
	return exec.Command("tmux", args...).Run() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
}

var runTmuxOut = func(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	return string(out), err
}

var panePattern = regexp.MustCompile(`^%[0-9]+$`)

const dragFormat = "#{@bridge_pane}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{window_width}|#{window_height}|#{P:#{pane_id}=#{@bridge_pane} }|#{window_layout}"

// resolveDrag turns the local pane id a drag-end bind stashed into a daemon
// request: float-geom for a float, tile-layout for a tiled pane. The bind
// cannot expand the pane itself: its format context is wherever the button
// was released, and a `-t '#{…}'` target is never format-expanded. One
// display-message reads the id map and the layout, so both describe the same
// instant; mapping through @bridge_pane makes a desynced mirror an error
// rather than a reshape of the wrong remote panes.
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
	for pair := range strings.FieldsSeq(fields[8]) {
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
	conn, err := net.DialTimeout("unix", sock, dialTimeout) //nolint:gosec // dials a local unix socket path we own, not a network address
	if err != nil {
		return fmt.Errorf("bridge daemon unreachable: %w", err)
	}
	defer conn.Close() //nolint:errcheck // deferred close of a request/response conn; failure is not actionable
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
