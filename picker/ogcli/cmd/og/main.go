// Command og is the tmux-og dispatcher: `og <noun> [<verb>] [args...]`.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/noamsto/tmux-og/picker/ogcli"
)

func main() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "og: locate executable: %v\n", err)
		os.Exit(1)
	}
	table := filepath.Join(filepath.Dir(exe), "..", "share", "og", "table.json")
	os.Exit(ogcli.Main(table, os.Args[1:], os.Stdout, os.Stderr))
}
