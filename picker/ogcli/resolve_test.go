package ogcli

import (
	"bytes"
	"slices"
	"testing"
)

func TestResolve(t *testing.T) {
	table := mustTable(t, stubVerbs("/stub"))
	tests := []struct {
		name   string
		args   []string
		action Action
		noun   string
		verb   string
		rest   []string
	}{
		{"bare", nil, ActionHelp, "", "", nil},
		{"help", []string{"help"}, ActionHelp, "", "", nil},
		{"--help", []string{"--help"}, ActionHelp, "", "", nil},
		{"-h", []string{"-h"}, ActionHelp, "", "", nil},
		{"help with extra is unknown", []string{"help", "x"}, ActionUnknown, "", "", nil},

		{"two-token match", []string{"g", "a"}, ActionRun, "", "g a", []string{}},
		{"two-token match keeps the remainder", []string{"g", "a", "--flag", "--", "bar", ""}, ActionRun, "", "g a", []string{"--flag", "--", "bar", ""}},
		{"two-token match, sole --help", []string{"g", "a", "--help"}, ActionRun, "", "g a", []string{"--help"}},
		{"joined single arg matches like bash", []string{"g a"}, ActionRun, "", "g a", []string{}},

		{"one-token verb with no subverbs", []string{"plain"}, ActionRun, "", "plain", []string{}},
		{"one-token verb passes everything through", []string{"plain", "x", "--y"}, ActionRun, "", "plain", []string{"x", "--y"}},
		{"one-token verb passes --help through", []string{"plain", "--help"}, ActionRun, "", "plain", []string{"--help"}},

		{"status shape bare runs the bare verb", []string{"s"}, ActionRun, "", "s", []string{}},
		{"status shape flag passes through", []string{"s", "--flag", "foo"}, ActionRun, "", "s", []string{"--flag", "foo"}},
		{"status shape short flag passes through", []string{"s", "-x"}, ActionRun, "", "s", []string{"-x"}},
		{"status shape subverb wins", []string{"s", "x"}, ActionRun, "", "s x", []string{}},
		{"status shape bare word is unknown", []string{"s", "frobnicate"}, ActionUnknown, "", "", nil},
		{"status shape --help is noun help", []string{"s", "--help"}, ActionNounHelp, "s", "", nil},
		{"status shape -h is noun help", []string{"s", "-h"}, ActionNounHelp, "s", "", nil},
		{"status shape help is noun help", []string{"s", "help"}, ActionNounHelp, "s", "", nil},
		{"status shape empty second arg is unknown", []string{"s", ""}, ActionUnknown, "", "", nil},

		{"empty first arg", []string{""}, ActionUnknown, "", "", nil},
		{"empty first arg and more", []string{"", "x"}, ActionUnknown, "", "", nil},
		{"unknown", []string{"nope"}, ActionUnknown, "", "", nil},
		{"unknown with args", []string{"nope", "x", "y"}, ActionUnknown, "", "", nil},

		{"subverb-only noun bare", []string{"g"}, ActionNounHelp, "g", "", nil},
		{"subverb-only noun --help", []string{"g", "--help"}, ActionNounHelp, "g", "", nil},
		{"subverb-only noun -h", []string{"g", "-h"}, ActionNounHelp, "g", "", nil},
		{"subverb-only noun help", []string{"g", "help"}, ActionNounHelp, "g", "", nil},
		{"subverb-only noun frobnicate", []string{"g", "frobnicate"}, ActionUnknown, "", "", nil},
		{"subverb-only noun flag is unknown", []string{"g", "--flag"}, ActionUnknown, "", "", nil},
		{"help forms on a non-noun are unknown", []string{"nope", "--help"}, ActionUnknown, "", "", nil},
		{"help forms on a plain verb pass through", []string{"plain", "help"}, ActionRun, "", "plain", []string{"help"}},

		{"go-backed verb", []string{"remote", "open", "h", "s", "--into", "x"}, ActionRun, "", "remote open", []string{"h", "s", "--into", "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Resolve(tc.args)
			if got.Action != tc.action {
				t.Fatalf("action = %v, want %v", got.Action, tc.action)
			}
			if got.Noun != tc.noun {
				t.Errorf("noun = %q, want %q", got.Noun, tc.noun)
			}
			name := ""
			if got.Verb != nil {
				name = got.Verb.Verb
			}
			if name != tc.verb {
				t.Errorf("verb = %q, want %q", name, tc.verb)
			}
			if tc.action == ActionRun && !slices.Equal(got.Remaining, tc.rest) {
				t.Errorf("remaining = %q, want %q", got.Remaining, tc.rest)
			}
		})
	}
}

const allHelp = `Usage: og <noun> [<verb>] [args...]

  q                      Quote 'it' (now) -- \ done
  q sub                  Sub of q

  g a                    Alpha
  g b                    Beta

  s                      Status
  s x                    Sub x

  plain                  Just a verb

  remote open            Open a remote
`

func TestHelpText(t *testing.T) {
	table := mustTable(t, stubVerbs("/stub"))

	var out bytes.Buffer
	table.writeAllHelp(&out)
	if out.String() != allHelp {
		t.Errorf("all help:\n%s\nwant:\n%s", out.String(), allHelp)
	}
	if bytes.Contains(out.Bytes(), []byte("completion")) {
		t.Error("completion leaked into og help")
	}

	out.Reset()
	table.writeNounHelp(&out, "s")
	want := "Usage: og s <verb> [args...]\n\n  s x                    Sub x\n"
	if out.String() != want {
		t.Errorf("noun help = %q, want %q", out.String(), want)
	}

	out.Reset()
	writeUnknown(&out, []string{"s", "frob"})
	want = "og: unknown command: s frob\nRun 'og help' for a list of commands.\n"
	if out.String() != want {
		t.Errorf("unknown = %q, want %q", out.String(), want)
	}

	out.Reset()
	writeUnknown(&out, []string{""})
	if want := "og: unknown command: \nRun 'og help' for a list of commands.\n"; out.String() != want {
		t.Errorf("unknown empty = %q, want %q", out.String(), want)
	}
}
