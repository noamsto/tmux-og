// Command project is og-project-bin, the Go side of og project: the shim passes its store paths as
// flags, then the public argv after "--".
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/noamsto/tmux-og/picker/project"
)

// options holds what the shim pinned.
type options struct{ remoteOpen string }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	var opts options
	if len(args) >= 1 && args[0] == "--remote-open" {
		if len(args) < 2 {
			_, _ = fmt.Fprint(stderr, project.Usage)
			return 2
		}
		opts.remoteOpen, args = args[1], args[2:]
	}
	if len(args) >= 1 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, project.Usage)
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, project.Usage)
		return 0
	case "status":
		return status(args[1:], opts, stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, project.Usage)
	return 2
}

func status(args []string, _ options, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = fmt.Fprint(stdout, project.Usage)
		return 0
	}
	if len(args) > 1 {
		_, _ = fmt.Fprint(stderr, project.Usage)
		return 2
	}
	cfg, err := project.LoadConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "og project: %v\n", err)
		return 1
	}
	var opt project.StatusOptions
	if len(args) == 1 {
		opt.Query = args[0]
	}
	return project.Status(context.Background(), stdout, stderr, cfg, project.ExecRunner{}, opt)
}
