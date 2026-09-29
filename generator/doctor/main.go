package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/generator/config"
)

func main() {
	code, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(args []string, stdout io.Writer) (int, error) {
	fs := flag.NewFlagSet("og-doctor", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: og-doctor [--config FILE]")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "path to config.toml (default: XDG config dir)")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	if *configPath == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			return 1, err
		}
		*configPath = defaultPath
	}

	if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
		_, _ = fmt.Fprintf(stdout, "no config found at %s — run 'og init'\n", *configPath)
		return 1, nil
	} else if statErr != nil {
		return 1, statErr
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return 1, err
	}

	ok := true

	_, _ = fmt.Fprintln(stdout, "Local:")
	for _, r := range CheckLocal(exec.LookPath) {
		printResult(stdout, r)
		if !r.OK {
			ok = false
		}
	}

	hosts := splitHosts(cfg.Remote.Hosts)
	hostResults := make([][]Result, len(hosts))
	hostErrs := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, runErr := RunRemote(ctx, host)
			if runErr != nil {
				hostErrs[i] = runErr
				return
			}
			hostResults[i] = ParseRemote(out)
		}(i, host)
	}
	wg.Wait()

	for i, host := range hosts {
		_, _ = fmt.Fprintln(stdout)
		if hostErrs[i] != nil {
			_, _ = fmt.Fprintf(stdout, "  %s: unreachable — %v\n", host, hostErrs[i])
			ok = false
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s:\n", host)
		for _, r := range hostResults[i] {
			printResult(stdout, r)
			if !r.OK {
				ok = false
			}
		}
	}

	_, _ = fmt.Fprintln(stdout)
	_, _ = fmt.Fprintln(stdout, "Not checked:")
	_, _ = fmt.Fprintln(stdout, "  - tmux-startup.service / launchd-agent enabled state (systemd/launchd introspection, not a PATH check)")
	_, _ = fmt.Fprintln(stdout, "  - the @crew_name/@crew_color fan-out harness (no fixed binary name to check)")

	if !ok {
		return 1, nil
	}
	return 0, nil
}

// splitHosts turns the config's space-separated host list into names; an
// empty Hosts string yields zero hosts.
func splitHosts(s string) []string {
	return strings.Fields(s)
}

func printResult(w io.Writer, r Result) {
	if r.OK {
		_, _ = fmt.Fprintf(w, "  [ok] %s\n", r.Name)
		return
	}
	_, _ = fmt.Fprintf(w, "  [MISSING] %s — %s (%s)\n", r.Name, r.Detail, r.Feature)
}
