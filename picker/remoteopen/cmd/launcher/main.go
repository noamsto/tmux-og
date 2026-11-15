// Command launcher is og-remote-launcher, the Go side of og-remote-open: the
// shim passes its store paths as flags, then the public argv after "--".
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/noamsto/tmux-og/picker/remoteopen"
)

const usage = "og-remote-launcher: usage: og-remote-launcher --self <path> --ctl <path> --daemon <path> --renderer <path> --reflow <path> --loading <path> -- <og-remote-open args>"

func main() {
	args := os.Args[1:]
	sep := slices.Index(args, "--")
	if sep < 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var p remoteopen.Paths
	fs := flag.NewFlagSet("og-remote-launcher", flag.ContinueOnError)
	fs.StringVar(&p.Self, "self", "", "path the daemon's hand-off re-enters")
	fs.StringVar(&p.Ctl, "ctl", "", "og-remote-bridge-ctl")
	fs.StringVar(&p.Daemon, "daemon", "", "og-remote-bridge-daemon")
	fs.StringVar(&p.Renderer, "renderer", "", "og-remote-bridge-renderer")
	fs.StringVar(&p.Reflow, "reflow", "", "tmux-reflow-windows")
	fs.StringVar(&p.Loading, "loading", "", "og-remote-loading")
	if err := fs.Parse(args[:sep]); err != nil {
		os.Exit(2)
	}
	os.Exit(remoteopen.Main(p, args[sep+1:]))
}
