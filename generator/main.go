// Command og-generate renders tmux.conf from a serialized config and a
// resolved set of paths.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
	"github.com/noamsto/tmux-og/generator/render"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fail(err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("og-generate", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: og-generate --config FILE (--paths FILE | --prefix DIR) --out DIR --template FILE")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "path to config.toml")
	pathsPath := fs.String("paths", "", "path to paths.toml")
	prefix := fs.String("prefix", "", "install prefix to synthesize paths from")
	outDir := fs.String("out", "", "directory to write tmux.conf into")
	templatePath := fs.String("template", "", "path to tmux.conf.tmpl")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *configPath == "":
		return fmt.Errorf("og-generate: --config is required")
	case *outDir == "":
		return fmt.Errorf("og-generate: --out is required")
	case *templatePath == "":
		return fmt.Errorf("og-generate: --template is required")
	case (*pathsPath == "") == (*prefix == ""):
		return fmt.Errorf("og-generate: exactly one of --paths or --prefix is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	var resolved *paths.Paths
	if *pathsPath != "" {
		resolved, err = paths.Load(*pathsPath)
	} else {
		resolved, err = paths.FromPrefix(*prefix)
	}
	if err != nil {
		return err
	}

	out, err := render.Execute(*templatePath, render.Build(cfg, resolved))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil { //nolint:gosec // tmux.conf lands in the nix store, which must be world-readable
		return fmt.Errorf("og-generate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(*outDir, "tmux.conf"), out, 0o644); err != nil { //nolint:gosec // tmux.conf lands in the nix store, which must be world-readable
		return fmt.Errorf("og-generate: %w", err)
	}
	return nil
}

// fail prints without adding a newline the message already carries — the VS16
// rejection is a multi-line literal shared with Nix and must not gain bytes.
func fail(err error) {
	msg := err.Error()
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	_, _ = os.Stderr.WriteString(msg)
	os.Exit(1)
}
