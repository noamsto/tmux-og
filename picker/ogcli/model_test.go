package ogcli

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/alecthomas/kong"
)

func childNames(n *kong.Node) []string {
	var names []string
	for _, c := range n.Children {
		names = append(names, c.Name)
	}
	return names
}

func child(t *testing.T, n *kong.Node, name string) *kong.Node {
	t.Helper()
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no child %q (has %v)", n.Name, name, childNames(n))
	return nil
}

func positionals(n *kong.Node) []string {
	var names []string
	for _, p := range n.Positional {
		names = append(names, p.Name)
	}
	return names
}

func flagNames(n *kong.Node) []string {
	var names []string
	for _, f := range n.Flags {
		if f.Name != "help" {
			names = append(names, f.Name)
		}
	}
	return names
}

func TestModelShape(t *testing.T) {
	parser, err := buildParser(mustTable(t, stubVerbs("/stub")))
	if err != nil {
		t.Fatal(err)
	}
	root := parser.Model.Node

	if got, want := childNames(root), []string{"q", "g", "s", "plain", "remote", "completion"}; !slices.Equal(got, want) {
		t.Errorf("root children = %v, want %v", got, want)
	}
	if !child(t, root, "completion").Hidden {
		t.Error("completion is not hidden")
	}

	q := child(t, root, "q")
	if got := childNames(q); !slices.Equal(got, []string{"sub"}) {
		t.Errorf("q children = %v", got)
	}
	if len(q.Positional) != 0 {
		t.Errorf("q has positionals %v; a noun with subverbs cannot take one", positionals(q))
	}
	if q.Help != `Quote 'it' (now) -- \ done` {
		t.Errorf("q help = %q", q.Help)
	}
	s := child(t, root, "s")
	if got := childNames(s); !slices.Equal(got, []string{"x"}) || len(s.Positional) != 0 {
		t.Errorf("s children = %v positionals = %v", got, positionals(s))
	}

	g := child(t, root, "g")
	if got := childNames(g); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("g children = %v", got)
	}
	if g.Help != "2 subcommands" {
		t.Errorf("g help = %q", g.Help)
	}
	if got := child(t, root, "remote").Help; got != "1 subcommand" {
		t.Errorf("remote help = %q", got)
	}

	plain := child(t, root, "plain")
	if got := positionals(plain); !slices.Equal(got, []string{"args"}) {
		t.Errorf("plain positionals = %v, want the passthrough arg", got)
	}
	if len(flagNames(plain)) != 0 {
		t.Errorf("a bash-backed verb has flags %v", flagNames(plain))
	}

	open := child(t, child(t, root, "remote"), "open")
	if got := flagNames(open); !slices.Equal(got, []string{"into", "window"}) {
		t.Errorf("remote open flags = %v", got)
	}
	if got := positionals(open); !slices.Equal(got, []string{"host", "sess", "win"}) {
		t.Errorf("remote open positionals = %v", got)
	}
}

func TestBuildParserRejectsBadTables(t *testing.T) {
	tests := map[string][]Verb{
		"completion verb": {{Verb: "completion", Target: "/t", Summary: "x"}},
		"completion noun": {{Verb: "completion x", Target: "/t", Summary: "x"}},
		"three tokens":    {{Verb: "a b c", Target: "/t", Summary: "x"}},
		"empty verb":      {{Verb: "", Target: "/t", Summary: "x"}},
		"double space":    {{Verb: "a  b", Target: "/t", Summary: "x"}},
		"duplicate":       {{Verb: "a", Target: "/t", Summary: "x"}, {Verb: "a", Target: "/t", Summary: "y"}},
		"relative target": {{Verb: "a", Target: "t", Summary: "x"}},
		"unknown model":   {{Verb: "a", Target: "/t", Summary: "x", Model: "nope"}},
		"model on noun":   {{Verb: "a", Target: "/t", Summary: "x", Model: "remote-open"}, {Verb: "a b", Target: "/t", Summary: "y"}},
	}
	for name, verbs := range tests {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(verbs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseTable(data); err == nil {
				t.Fatal("ParseTable accepted the table")
			}
		})
	}
}
