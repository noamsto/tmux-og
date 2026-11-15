package ogcli

import (
	"fmt"
	"strings"

	"github.com/alecthomas/kong"
)

// fishQuote single-quotes s; fish honours only \\ and \' inside them.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}

// fishScript renders the static fish completion script for the model: nouns
// with their summaries, subverbs gated on the noun already being typed, and
// one line per flag of a verb that has any, gated on that verb being typed.
// Static rather than COMP_LINE-driven so every candidate carries its summary.
func fishScript(app *kong.Application) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("complete -c og -f -n '__fish_use_subcommand' -a help -d 'List every verb'")
	nouns := visibleChildren(app.Node)
	for _, n := range nouns {
		line("complete -c og -f -n '__fish_use_subcommand' -a %s -d %s", fishQuote(n.Name), fishQuote(n.Help))
	}
	for _, n := range nouns {
		cond := fmt.Sprintf("set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = %s", fishQuote(n.Name))
		for _, sub := range visibleChildren(n) {
			line("complete -c og -f -n %s -a %s -d %s", fishQuote(cond), fishQuote(sub.Name), fishQuote(sub.Help))
		}
	}
	for _, n := range nouns {
		writeFlagLines(line, []*kong.Node{n})
		for _, sub := range visibleChildren(n) {
			writeFlagLines(line, []*kong.Node{n, sub})
		}
	}
	return b.String()
}

// writeFlagLines emits the flags of the last node in path, gated on the
// commandline's words being exactly path followed by anything.
func writeFlagLines(line func(string, ...any), path []*kong.Node) {
	node := path[len(path)-1]
	if len(node.Children) > 0 {
		return
	}
	var cond strings.Builder
	fmt.Fprintf(&cond, "set -l t (commandline -opc); test (count $t) -ge %d", len(path)+1)
	for i, n := range path {
		fmt.Fprintf(&cond, "; and test $t[%d] = %s", i+2, fishQuote(n.Name))
	}
	for _, f := range node.Flags {
		if f.Hidden || f.Name == "help" {
			continue
		}
		line("complete -c og -n %s -l %s -d %s", fishQuote(cond.String()), f.Name, fishQuote(f.Help))
	}
}

func visibleChildren(n *kong.Node) []*kong.Node {
	var out []*kong.Node
	for _, c := range n.Children {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	return out
}
