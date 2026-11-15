package ogcli

import (
	"fmt"
	"io"
	"syscall"

	"github.com/alecthomas/kong"
	kongcompletion "github.com/jotaen/kong-completion"
)

// exitCode carries kong's Exit (completion) out of its parse as a panic:
// kong calls Exit and then keeps going, so the override must not return.
type exitCode int

// Main runs og over the table at tablePath and returns the exit status. A
// resolved verb replaces the process, so it returns only on failure.
func Main(tablePath string, args []string, stdout, stderr io.Writer) (code int) {
	table, err := Load(tablePath)
	if err != nil {
		fprintf(stderr, "og: %v\n", err)
		return 1
	}

	defer func() {
		if r := recover(); r != nil {
			c, ok := r.(exitCode)
			if !ok {
				panic(r)
			}
			code = int(c)
		}
	}()

	parser, err := buildParser(table,
		kong.Writers(stdout, stderr),
		kong.Exit(func(c int) { panic(exitCode(c)) }),
	)
	if err != nil {
		fprintf(stderr, "og: %v\n", err)
		return 1
	}
	kongcompletion.Register(parser) // answers COMP_LINE completion requests, then exits

	if len(args) > 0 && args[0] == completionCommand {
		return runCompletion(parser, args, stdout, stderr)
	}
	return run(table, args, stdout, stderr)
}

func run(table *Table, args []string, stdout, stderr io.Writer) int {
	res := table.Resolve(args)
	switch res.Action {
	case ActionHelp:
		table.writeAllHelp(stdout)
		return 0
	case ActionNounHelp:
		table.writeNounHelp(stdout, res.Noun)
		return 0
	case ActionUnknown:
		writeUnknown(stderr, args)
		return 2
	case ActionRun:
		return runVerb(res, stdout, stderr)
	default:
		panic(fmt.Sprintf("ogcli: unhandled action %d", res.Action))
	}
}

// runVerb execs the target with the argv untouched. The only argv og reads is a
// sole --help/-h: the target, not og, decides what every other argument means.
func runVerb(res Resolution, stdout, stderr io.Writer) int {
	v := res.Verb
	if len(res.Remaining) == 1 && (res.Remaining[0] == "--help" || res.Remaining[0] == "-h") {
		writeVerbHelp(stdout, v)
		return 0
	}
	argv := append([]string{v.Target}, res.Remaining...)
	err := syscall.Exec(v.Target, argv, environ()) //nolint:gosec // G204: the target is a table entry, argv passed without a shell
	fprintf(stderr, "og: exec %s: %v\n", v.Target, err)
	return 126
}

// writeVerbHelp prints the summary, a Go-backed verb's usage, and the target
// path last.
func writeVerbHelp(w io.Writer, v *Verb) {
	fprintf(w, "og %s — %s\n", v.Verb, v.Summary)
	if v.Model != "" {
		fprintln(w)
		fprintf(w, "%s", models[v.Model].help)
	}
	fprintln(w, v.Target)
}
