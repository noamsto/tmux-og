// Command og-init writes a fresh, commented config.toml for a non-Nix
// install.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/initcfg"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fail(err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("og-init", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: og-init [--out FILE] [--force]")
		fs.PrintDefaults()
	}
	out := fs.String("out", "", "path to write config.toml to (default: XDG config dir)")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *out == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			return err
		}
		*out = defaultPath
	}

	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("og-init: %s already exists — pass --force to overwrite, or remove it first", *out)
	}

	content, err := initcfg.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil { //nolint:gosec // generated config is meant to be world-readable
		return fmt.Errorf("og-init: %w", err)
	}
	if err := os.WriteFile(*out, []byte(content), 0o644); err != nil { //nolint:gosec // generated config is meant to be world-readable
		return fmt.Errorf("og-init: %w", err)
	}

	fmt.Println(*out)
	fmt.Printf("run 'og generate --config %s --prefix <dir>' to build tmux.conf\n", *out)
	return nil
}

// fail prints without adding a newline the message already carries.
func fail(err error) {
	msg := err.Error()
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	_, _ = os.Stderr.WriteString(msg)
	os.Exit(1)
}
