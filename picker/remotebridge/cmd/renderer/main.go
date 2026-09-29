package main

import (
	"fmt"
	"net"
	"os"

	"github.com/noamsto/tmux-og/picker/remotebridge/render"
)

func sockAndPane(args []string) (sock, pane string, ok bool) {
	if len(args) != 3 {
		return "", "", false
	}
	return args[1], args[2], true
}

func main() {
	sock, pane, ok := sockAndPane(os.Args)
	if !ok {
		fmt.Fprintf(os.Stderr, "usage: %s <sock> <remote-pane>\n", os.Args[0])
		os.Exit(1)
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // dials a local unix socket path we own, not a network address
	if err != nil {
		fmt.Fprintf(os.Stderr, "renderer: dial %s: %v\r\n", sock, err)
		os.Exit(1)
	}
	defer conn.Close() //nolint:errcheck // deferred close before process exit
	if err := render.Run(conn, pane, os.Stdin, os.Stdout,
		func() (func() error, error) { return render.MakeRaw(0) },
		func(int, int) {}); err != nil {
		fmt.Fprintf(os.Stderr, "renderer: %v\r\n", err)
		os.Exit(1)
	}
}
