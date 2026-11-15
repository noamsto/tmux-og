package ogcli

import (
	"fmt"
	"io"
	"strings"
)

// Action is what the resolver decided the arguments ask for.
type Action int

const (
	ActionHelp     Action = iota // the full verb list
	ActionNounHelp               // the subverbs of Resolution.Noun
	ActionRun                    // exec Resolution.Verb with Resolution.Remaining
	ActionUnknown                // no verb matches
)

// Resolution is the resolver's verdict.
type Resolution struct {
	Action    Action
	Noun      string
	Verb      *Verb
	Remaining []string
}

// fprintf and fprintln drop the write error: a closed pipe leaves nothing to
// report to.
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func fprintln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func isHelpWord(s string) bool { return s == "help" || s == "--help" || s == "-h" }

// Resolve maps argv (without the program name) to a verb. The conditions and
// their order are those of the bash dispatcher this replaced.
func (t *Table) Resolve(args []string) Resolution {
	if len(args) == 0 || (len(args) == 1 && isHelpWord(args[0])) {
		return Resolution{Action: ActionHelp}
	}

	if len(args) >= 2 {
		if v, ok := t.byName[args[0]+" "+args[1]]; ok {
			return Resolution{Action: ActionRun, Verb: v, Remaining: args[2:]}
		}
	}

	// A noun with subverbs (status, notify) that also owns a bare verb takes
	// the bare match whenever a second token cannot be an attempted subverb:
	// the whole command, or a flag (subverbs are bare words, so a leading "-"
	// can only be a passthrough argument). --help/-h stay out of the flag case
	// because they are how a noun's own help is requested; a bare second word
	// is ambiguous between an argument and a misspelled subverb, so it falls
	// through to unknown.
	if v, ok := t.byName[args[0]]; ok && args[0] != "" {
		if len(args) == 1 || !t.hasSubverbs(args[0]) || (strings.HasPrefix(args[1], "-") && args[1] != "--help" && args[1] != "-h") {
			return Resolution{Action: ActionRun, Verb: v, Remaining: args[1:]}
		}
	}

	if t.hasSubverbs(args[0]) && (len(args) == 1 || (len(args) == 2 && isHelpWord(args[1]))) {
		return Resolution{Action: ActionNounHelp, Noun: args[0]}
	}
	return Resolution{Action: ActionUnknown}
}

// writeAllHelp prints every verb, a blank line between nouns.
func (t *Table) writeAllHelp(w io.Writer) {
	fprintln(w, "Usage: og <noun> [<verb>] [args...]")
	fprintln(w)
	prev := ""
	for _, v := range t.verbs {
		noun := v.Tokens()[0]
		if prev != "" && noun != prev {
			fprintln(w)
		}
		writeVerbLine(w, v)
		prev = noun
	}
}

// writeNounHelp prints the subverbs of noun.
func (t *Table) writeNounHelp(w io.Writer, noun string) {
	fprintf(w, "Usage: og %s <verb> [args...]\n\n", noun)
	for _, v := range t.verbs {
		if strings.HasPrefix(v.Verb, noun+" ") {
			writeVerbLine(w, v)
		}
	}
}

func writeVerbLine(w io.Writer, v Verb) {
	fprintf(w, "  %-22s %s\n", v.Verb, v.Summary)
}

func writeUnknown(w io.Writer, args []string) {
	fprintf(w, "og: unknown command: %s\n", strings.Join(args, " "))
	fprintln(w, "Run 'og help' for a list of commands.")
}
