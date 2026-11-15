package ogcli

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/alecthomas/kong"
	kongcompletion "github.com/jotaen/kong-completion"
)

// passthroughVerb is a bash-backed verb: og never parses its arguments, the
// positional only gives the model (completion) a place for them.
type passthroughVerb struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"Arguments passed to the target unchanged."`
}

// buildParser builds the kong model for the table: a command per noun, a
// subcommand per two-word verb and the hidden completion command. A noun that
// also owns a bare verb (status, notify) is a command with subcommands only,
// because kong cannot give one node both subcommands and positionals; the
// resolver, not kong, decides what its arguments mean.
func buildParser(t *Table, opts ...kong.Option) (*kong.Kong, error) {
	var nouns []string
	subverbs := map[string][]Verb{}
	for _, v := range t.verbs {
		tokens := v.Tokens()
		if _, seen := subverbs[tokens[0]]; !seen {
			nouns = append(nouns, tokens[0])
			subverbs[tokens[0]] = nil
		}
		if len(tokens) == 2 {
			subverbs[tokens[0]] = append(subverbs[tokens[0]], v)
		}
	}

	var fields []reflect.StructField
	for i, noun := range nouns {
		typ, help, err := nounNode(t, noun, subverbs[noun])
		if err != nil {
			return nil, err
		}
		fields = append(fields, commandField(i, noun, help, typ, false))
	}
	fields = append(fields, commandField(len(nouns), completionCommand,
		"Print shell completion code", reflect.TypeFor[kongcompletion.Completion](), true))

	root := reflect.New(reflect.StructOf(fields))
	parser, err := kong.New(root.Interface(), append([]kong.Option{kong.Name("og")}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("build og command model: %w", err)
	}
	return parser, nil
}

// nounNode returns the node type and help text for a first word of the table.
func nounNode(t *Table, noun string, subs []Verb) (reflect.Type, string, error) {
	own := t.byName[noun]
	if len(subs) == 0 {
		if own == nil {
			return nil, "", fmt.Errorf("verb table: %q has neither a verb nor subverbs", noun)
		}
		return leafType(*own), own.Summary, nil
	}
	fields := make([]reflect.StructField, 0, len(subs))
	for i, v := range subs {
		fields = append(fields, commandField(i, v.Tokens()[1], v.Summary, leafType(v), false))
	}
	help := fmt.Sprintf("%d subcommand%s", len(subs), plural(len(subs)))
	if own != nil {
		help = own.Summary
	}
	return reflect.StructOf(fields), help, nil
}

func leafType(v Verb) reflect.Type {
	if v.Model == "" {
		return reflect.TypeFor[passthroughVerb]()
	}
	return reflect.TypeOf(models[v.Model].new()).Elem()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func commandField(i int, name, help string, typ reflect.Type, hidden bool) reflect.StructField {
	tag := `cmd:"" name:` + strconv.Quote(name) + ` help:` + strconv.Quote(escapeHelp(help))
	if hidden {
		tag += ` hidden:""`
	}
	return reflect.StructField{Name: "C" + strconv.Itoa(i), Type: typ, Tag: reflect.StructTag(tag)}
}

// escapeHelp doubles "$": kong interpolates ${var} in help text.
func escapeHelp(s string) string { return strings.ReplaceAll(s, "$", "$$") }
