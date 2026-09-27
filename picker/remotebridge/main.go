package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/render"
)

var paneIDRe = regexp.MustCompile(`^%[0-9]+$`)

func main() {
	// Flags default to OG_BRIDGE_* env vars: og-remote-open passes
	// the (untrusted, remote-derived) host/session/window into tmux's
	// environment rather than interpolating them into the /bin/sh command
	// string, so a crafted remote session name can't break out into local
	// shell execution.
	host := flag.String("host", os.Getenv("OG_BRIDGE_HOST"), "ssh host")
	session := flag.String("session", os.Getenv("OG_BRIDGE_SESSION"), "remote session")
	window := flag.Int("window", envInt("OG_BRIDGE_WINDOW"), "remote window index")
	remoteTmux := flag.String("tmux", envDefault("OG_BRIDGE_TMUX", "tmux"), "absolute remote tmux path")
	tmpdir := flag.String("tmpdir", os.Getenv("OG_BRIDGE_TMPDIR"), "remote TMUX_TMPDIR")
	sshCmd := flag.String("ssh", envDefault("OG_BRIDGE_SSH", "ssh"), "control transport command (empty = run tmux locally)")
	flag.Parse()

	// remoteTmux may carry args (e.g. "tmux -S <sock>" for tests), so split
	// it into argv rather than passing it as a single token.
	tmuxArgv := strings.Fields(*remoteTmux)
	var ctl *exec.Cmd
	if *sshCmd == "" {
		ctl = exec.Command(tmuxArgv[0], append(append([]string{}, tmuxArgv[1:]...),
			"-C", "attach-session", "-t", *session)...)
	} else {
		// ssh space-joins the post-host argv into one string run by the
		// remote login shell, so shell-quote the session name (may contain
		// spaces) to keep it a single target token.
		args := append([]string{"-T", "-e", "none", *host, "--", "env", "TMUX_TMPDIR=" + *tmpdir}, tmuxArgv...)
		args = append(args, "-C", "attach-session", "-t", shellQuote(*session))
		ctl = exec.Command(*sshCmd, args...)
	}
	stdin, err := ctl.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "og-remote-bridge: %v\r\n", err)
		os.Exit(1)
	}
	stdout, err := ctl.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "og-remote-bridge: %v\r\n", err)
		os.Exit(1)
	}
	cmds := bufio.NewWriter(stdin)
	// send is called from the main setup path plus the input and resize
	// goroutines once the loops start; serialize so command lines never
	// interleave on the wire, and so a send racing teardown's stdin.Close
	// sees "closed" instead of writing to a closed pipe.
	var sendMu sync.Mutex
	closed := false
	send := func(s string) {
		sendMu.Lock()
		defer sendMu.Unlock()
		if closed {
			return
		}
		fmt.Fprintf(cmds, "%s\n", s)
		cmds.Flush()
	}

	if err := ctl.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "og-remote-bridge: %v\r\n", err)
		os.Exit(1)
	}
	reader := controlmode.NewReader(stdout)

	w, h, sizeErr := render.Size(0)
	hasTTY := sizeErr == nil

	s, err := seedFlow(reader, send, *session, *window, hasTTY, w, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "og-remote-bridge: %v\r\n", err)
		os.Exit(1)
	}
	pane := s.pane

	// Raw mode clears OPOST, so the kernel stops mapping bare "\n" to "\r\n";
	// without this the seeded snapshot staircases until the first repaint.
	// capture-pane output uses bare "\n" line separators. Enter raw mode
	// BEFORE writing the seed so its pre-converted "\r\n" aren't doubled to
	// "\r\r\n" by an OPOST that's still on.
	captured := bytes.ReplaceAll(s.captured, []byte("\n"), []byte("\r\n"))
	restore, _ := render.MakeRaw(0)
	os.Stdout.Write(render.Seed(captured, s.cx, s.cy, s.alt, s.appck, nil, nil))

	teardown := func() {
		if restore != nil {
			restore()
		}
		sendMu.Lock()
		closed = true
		stdin.Close()
		sendMu.Unlock()
	}

	// input goroutine
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				for _, cmd := range controlmode.SendKeysArgs(pane, buf[:n], controlmode.InputChunkBytes) {
					send(quoteArgs(cmd))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// resize goroutine
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		var t *time.Timer
		for range winch {
			if t != nil {
				t.Stop()
			}
			t = time.AfterFunc(50*time.Millisecond, func() {
				if w, h, err := render.Size(0); err == nil {
					send(fmt.Sprintf("refresh-client -C %dx%d", w, h))
				}
			})
		}
	}()

	// render loop (main goroutine)
	for {
		l, ok := reader.Next()
		if !ok {
			teardown()
			return
		}
		switch l.Kind {
		case controlmode.Output:
			if l.Pane == pane {
				os.Stdout.Write(l.Data)
			}
		case controlmode.WindowClose, controlmode.Exit:
			teardown()
			return
		}
	}
}

// seed holds the snapshot state resolved from the remote window before the
// bridge enters raw mode.
type seed struct {
	pane       string
	cx, cy     int
	alt, appck bool
	captured   []byte
}

// seedFlow issues the startup commands (list-panes, optional refresh-client,
// display-message, capture-pane) and reads exactly one reply block per
// command. list-panes resolves the pane id the other two commands target, so
// it keeps its own wait; refresh-client (tty only) keeps its own wait too,
// since pipelining it with the capture would make the captured screen size
// depend on tmux having applied the client resize by the time the queued
// capture-pane runs. display-message and capture-pane depend on neither each
// other nor anything between them, so both are written before either reply is
// read.
func seedFlow(reader *controlmode.Reader, send func(string), session string, window int, hasTTY bool, w, h int) (seed, error) {
	send(fmt.Sprintf("list-panes -t %s:%d -F '#{pane_active} #{pane_id}'", tmuxQuote(session), window))
	pane := readActivePane(reader)
	if !paneIDRe.MatchString(pane) {
		return seed{}, fmt.Errorf("no active pane for %s:%d", session, window)
	}

	if hasTTY {
		send(fmt.Sprintf("refresh-client -C %dx%d", w, h))
		readReply(reader) // consume refresh-client's own (empty) reply
	}

	send(fmt.Sprintf("display-message -p -t %s -F '#{cursor_x} #{cursor_y} #{alternate_on} #{keypad_cursor_flag}'", pane))
	send(fmt.Sprintf("capture-pane -e -p -t %s", pane))
	cursorLine, cursorOK := readReply(reader)
	captureLine, _ := readReply(reader)
	cx, cy, alt, appck := readCursor(cursorLine, cursorOK)

	return seed{pane, cx, cy, alt, appck, readCapture(captureLine)}, nil
}

// readReply returns the next reply block to a command we sent (Kind End or
// Error carrying ClientCommandFlag), skipping %output, async notifications and
// the blocks tmux emits for commands we did not send — the implicit attach
// reply, and anything a remote hook runs in our command queue (#276). ok is
// false at EOF.
func readReply(reader *controlmode.Reader) (controlmode.Line, bool) {
	for {
		l, ok := reader.Next()
		if !ok {
			return controlmode.Line{}, false
		}
		if (l.Kind == controlmode.End || l.Kind == controlmode.Error) && l.Flags == controlmode.ClientCommandFlag {
			return l, true
		}
	}
}

// readActivePane reads the list-panes reply and picks the "1 %id" row
// (pane_active == 1).
func readActivePane(reader *controlmode.Reader) string {
	l, ok := readReply(reader)
	if !ok || l.Kind == controlmode.Error {
		return ""
	}
	for _, row := range strings.Split(string(l.Data), "\n") {
		fields := strings.Fields(row)
		if len(fields) == 2 && fields[0] == "1" {
			return fields[1]
		}
	}
	return ""
}

// readCursor parses a display-message reply already read via readReply:
// "cursor_x cursor_y alternate_on keypad_cursor_flag". An error reply or a
// lost stream (ok == false) degrades to (0,0,false,false) rather than failing
// the seed.
func readCursor(l controlmode.Line, ok bool) (cx, cy int, alt, appCursorKeys bool) {
	if !ok || l.Kind == controlmode.Error {
		return 0, 0, false, false
	}
	fields := strings.Fields(string(l.Data))
	if len(fields) != 4 {
		return 0, 0, false, false
	}
	cx, _ = strconv.Atoi(fields[0])
	cy, _ = strconv.Atoi(fields[1])
	return cx, cy, fields[2] == "1", fields[3] == "1"
}

// readCapture returns a capture-pane reply's body (pane content, already
// newline-joined by the Reader) already read via readReply. It ignores ok and
// Kind: an %error reply (the pane closed between list-panes and the capture)
// is not distinguished from a genuinely blank pane, and a lost stream yields
// the zero Line's nil Data.
func readCapture(l controlmode.Line) []byte {
	return l.Data
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

// shellQuote single-quotes s for a POSIX shell, escaping embedded single
// quotes. Used for the session name in the ssh remote-command argv.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tmuxQuote single-quotes s for a tmux control-mode command line, escaping
// any embedded single quote the tmux-safe way. Needed for the session name
// in the initial list-panes target, since session names may contain spaces;
// every other command below targets the resolved pane id (%N), which never
// contains spaces and needs no quoting.
func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteArgs joins a send-keys arg slice into one control-mode command line.
// Every token here is a pane id or hex byte pair — both safe, unquoted
// tokens — so a plain space-join is sufficient.
func quoteArgs(args []string) string {
	return strings.Join(args, " ")
}
