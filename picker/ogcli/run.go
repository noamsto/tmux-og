package ogcli

import (
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
	kongcompletion "github.com/jotaen/kong-completion"
)

// exitCode carries kong's Exit (help, completion) out of its parse as a panic:
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
		kong.Help(helpPrinter(table)),
	)
	if err != nil {
		fprintf(stderr, "og: %v\n", err)
		return 1
	}
	kongcompletion.Register(parser) // answers COMP_LINE completion requests, then exits

	if len(args) > 0 && args[0] == completionCommand {
		return runCompletion(parser, args, stdout, stderr)
	}
	return run(table, parser, args, stdout, stderr)
}

func run(table *Table, parser *kong.Kong, args []string, stdout, stderr io.Writer) int {
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
		return runVerb(parser, res, stdout, stderr)
	default:
		panic(fmt.Sprintf("ogcli: unhandled action %d", res.Action))
	}
}

func runVerb(parser *kong.Kong, res Resolution, stdout, stderr io.Writer) int {
	v := res.Verb
	sole := len(res.Remaining) == 1 && (res.Remaining[0] == "--help" || res.Remaining[0] == "-h")
	if v.Model == "" && sole {
		writeVerbHelpHeader(stdout, v)
		fprintln(stdout, v.Target)
		return 0
	}
	if v.Model != "" {
		// Validation and --help only: the target gets the original argv below.
		if _, err := parser.Parse(append(v.Tokens(), res.Remaining...)); err != nil {
			fprintf(stderr, "og: %v\n", err)
			fprintf(stderr, "Run 'og %s --help' for usage.\n", v.Verb)
			return 2
		}
	}
	argv := append([]string{v.Target}, res.Remaining...)
	err := syscall.Exec(v.Target, argv, environ()) //nolint:gosec // G204: the target is a table entry, argv passed without a shell
	fprintf(stderr, "og: exec %s: %v\n", v.Target, err)
	return 126
}

func writeVerbHelpHeader(w io.Writer, v *Verb) {
	fprintf(w, "og %s — %s\n", v.Verb, v.Summary)
}

// helpPrinter prints a table verb's help as its summary, kong's usage and
// flags, and the target path last; anything else gets kong's own help.
func helpPrinter(table *Table) kong.HelpPrinter {
	return func(options kong.HelpOptions, ctx *kong.Context) error {
		sel := ctx.Selected()
		if sel == nil {
			return kong.DefaultHelpPrinter(options, ctx)
		}
		v, ok := table.byName[strings.TrimPrefix(sel.FullPath(), ctx.Model.Name+" ")]
		if !ok {
			return kong.DefaultHelpPrinter(options, ctx)
		}
		writeVerbHelpHeader(ctx.Stdout, v)
		fprintln(ctx.Stdout)
		if err := kong.DefaultHelpPrinter(options, ctx); err != nil {
			return err
		}
		fprintln(ctx.Stdout, v.Target)
		return nil
	}
}
