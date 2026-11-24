package ogcli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remoteopen"
)

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestRunPassthrough(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"one-token verb", []string{"plain", "--flag", "--", "bar", ""}, "4\n[--flag]\n[--]\n[bar]\n[]\n"},
		{"two-token verb", []string{"g", "a", "--flag", "--", "bar", ""}, "4\n[--flag]\n[--]\n[bar]\n[]\n"},
		{"no arguments", []string{"plain"}, "0\n"},
		{"status shape flag", []string{"s", "--flag", "foo"}, "2\n[--flag]\n[foo]\n"},
		{"leading --", []string{"plain", "--", "--help"}, "2\n[--]\n[--help]\n"},
		{"go-backed verb forwards the raw argv", []string{"remote", "open", "host", "sess", "--into", "x"}, "4\n[host]\n[sess]\n[--into]\n[x]\n"},
		// remoteopen.ParseArgs alone judges these; og forwards them verbatim.
		{"go-backed verb keeps flag position", []string{"remote", "open", "--into", "x", "host", "sess"}, "4\n[--into]\n[x]\n[host]\n[sess]\n"},
		{"go-backed verb forwards the = form", []string{"remote", "open", "h", "s", "--into=loc"}, "3\n[h]\n[s]\n[--into=loc]\n"},
		{"go-backed verb forwards --into first", []string{"remote", "open", "--into", "x", "1"}, "3\n[--into]\n[x]\n[1]\n"},
		{"go-backed verb forwards unknown flags", []string{"remote", "open", "host", "--bogus"}, "2\n[host]\n[--bogus]\n"},
		{"go-backed verb with no arguments", []string{"remote", "open"}, "0\n"},
		{"go-backed verb forwards extra arguments", []string{"remote", "open", "a", "b", "c", "d"}, "4\n[a]\n[b]\n[c]\n[d]\n"},
		{"dash-leading session", []string{"remote", "open", "host", "-sess"}, "2\n[host]\n[-sess]\n"},
		{"dash-leading host", []string{"remote", "open", "-host", "sess"}, "2\n[-host]\n[sess]\n"},
		{"dash-leading local session", []string{"remote", "open", "host", "sess", "--into", "-scratch"}, "4\n[host]\n[sess]\n[--into]\n[-scratch]\n"},
		{"help token past a host is a name", []string{"remote", "open", "host", "--help"}, "2\n[host]\n[--help]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runOg(t, verbs, nil, tc.args...)
			if got.code != 0 || got.stubOut != tc.want {
				t.Errorf("code %d, stub saw %q, want %q (stderr %q)", got.code, got.stubOut, tc.want, got.stderr)
			}
		})
	}
}

func TestRunVerbHelpLastLineIsTarget(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)

	t.Run("bash-backed", func(t *testing.T) {
		for _, flag := range []string{"--help", "-h"} {
			got := runOg(t, verbs, nil, "g", "a", flag)
			if want := "og g a — Alpha\n" + target + "\n"; got.stdout != want || got.code != 0 {
				t.Errorf("%s: code %d stdout %q, want %q", flag, got.code, got.stdout, want)
			}
			if got.stubOut != "" {
				t.Errorf("%s ran the target", flag)
			}
		}
		got := runOg(t, verbs, nil, "plain", "--help")
		if want := "og plain — Just a verb\n" + target + "\n"; got.stdout != want {
			t.Errorf("one-token: stdout %q, want %q", got.stdout, want)
		}
	})

	t.Run("go-backed", func(t *testing.T) {
		for _, args := range [][]string{
			{"remote", "open", "--help"},
			{"remote", "open", "-h"},
		} {
			got := runOg(t, verbs, nil, args...)
			if got.code != 0 || got.stubOut != "" {
				t.Fatalf("%v: code %d, ran target %q (stderr %q)", args, got.code, got.stubOut, got.stderr)
			}
			if first, _, _ := strings.Cut(got.stdout, "\n"); first != "og remote open — Open a remote" {
				t.Errorf("%v: first line %q", args, first)
			}
			if last := lastLine(got.stdout); last != target {
				t.Errorf("%v: last line %q, want the target", args, last)
			}
			for _, want := range []string{"--into", "--window", "<host>"} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("%v: help lacks %q:\n%s", args, want, got.stdout)
				}
			}
		}
	})
}

func TestRunGoBackedHelpStatesTheTargetGrammar(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	got := runOg(t, stubVerbs(target), nil, "remote", "open", "--help")
	for _, banned := range []string{"--into=", "[flags]", "STRING"} {
		if strings.Contains(got.stdout, banned) {
			t.Errorf("help advertises %q:\n%s", banned, got.stdout)
		}
	}
}

// Every flag the kong model declares must be a token ParseArgs accepts in a
// literal position, or completion would offer a flag the launcher rejects.
func TestRemoteOpenModelFlagsAreParseArgsTokens(t *testing.T) {
	accepted := map[string][]string{
		"into":   {"h", "s", "--into", "loc"},
		"window": {"h", "s", "--into", "loc", "--window", "@1"},
	}
	parser, err := buildParser(mustTable(t, stubVerbs("/stub")))
	if err != nil {
		t.Fatal(err)
	}
	flags := 0
	for _, f := range flagNames(child(t, child(t, parser.Model.Node, "remote"), "open")) {
		flags++
		args, ok := accepted[f]
		if !ok {
			t.Errorf("flag --%s has no ParseArgs-accepted argv in this test", f)
			continue
		}
		if !slices.Contains(args, "--"+f) {
			t.Errorf("argv %v lacks the literal token --%s", args, f)
		}
		if _, err := remoteopen.ParseArgs(args, remoteopen.Env{}); err != nil {
			t.Errorf("ParseArgs(%v): %v", args, err)
		}
	}
	if flags != len(accepted) {
		t.Errorf("model declares %d flags, test covers %d", flags, len(accepted))
	}
}

func TestRunHelpAndUnknown(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)

	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		got := runOg(t, verbs, nil, args...)
		if got.code != 0 || got.stdout != allHelp || got.stderr != "" {
			t.Errorf("%v: code %d stdout %q stderr %q", args, got.code, got.stdout, got.stderr)
		}
		if strings.Contains(got.stdout, "completion") {
			t.Errorf("%v: completion leaked into help", args)
		}
	}

	got := runOg(t, verbs, nil, "g", "--help")
	if want := "Usage: og g <verb> [args...]\n\n  g a                    Alpha\n  g b                    Beta\n"; got.stdout != want || got.code != 0 {
		t.Errorf("noun help: code %d stdout %q", got.code, got.stdout)
	}
	if strings.Contains(got.stdout, "completion") {
		t.Error("completion leaked into noun help")
	}

	for _, args := range [][]string{{"nope"}, {"g", "frob"}, {"s", "frob"}, {""}} {
		got := runOg(t, verbs, nil, args...)
		want := "og: unknown command: " + strings.Join(args, " ") + "\nRun 'og help' for a list of commands.\n"
		if got.code != 2 || got.stdout != "" || got.stderr != want {
			t.Errorf("%q: code %d stdout %q stderr %q", args, got.code, got.stdout, got.stderr)
		}
	}
}

func TestRunMissingTable(t *testing.T) {
	var stderr strings.Builder
	if code := Main(t.TempDir()+"/absent.json", []string{"help"}, os.Stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "og: ") {
		t.Errorf("code %d stderr %q", code, stderr.String())
	}
}

func TestCompletionBashZsh(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	bash := runOg(t, verbs, nil, "completion", "bash", "-c")
	if bash.code != 0 || !strings.Contains(bash.stdout, "-C "+self) || !strings.HasSuffix(strings.TrimSpace(bash.stdout), " og") {
		t.Errorf("bash: code %d stdout %q (stderr %q)", bash.code, bash.stdout, bash.stderr)
	}
	zsh := runOg(t, verbs, nil, "completion", "zsh", "-c")
	if zsh.code != 0 || !strings.Contains(zsh.stdout, "#compdef og") {
		t.Errorf("zsh: code %d stdout %q", zsh.code, zsh.stdout)
	}
}

func TestCompletionCompLine(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)

	got := runOg(t, verbs, []string{"COMP_LINE=og remote open --"})
	if got.code != 0 {
		t.Fatalf("code %d stderr %q", got.code, got.stderr)
	}
	fields := strings.Fields(got.stdout)
	for _, want := range []string{"--into", "--window"} {
		if !slices.Contains(fields, want) {
			t.Errorf("COMP_LINE completion lacks %s: %q", want, got.stdout)
		}
	}
	if got.stubOut != "" {
		t.Error("completion ran the target")
	}

	got = runOg(t, verbs, []string{"COMP_LINE=og re"})
	if strings.TrimSpace(got.stdout) != "remote" {
		t.Errorf("noun completion = %q", got.stdout)
	}
	got = runOg(t, verbs, []string{"COMP_LINE=og comp"})
	if strings.TrimSpace(got.stdout) != "" {
		t.Errorf("hidden completion command offered: %q", got.stdout)
	}
}

func projectVerbs(target string) []Verb {
	return []Verb{{Verb: "project", Target: target, Summary: "Multi-host projects", Model: "project"}}
}

func TestRunProjectModel(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := projectVerbs(target)

	for _, flag := range []string{"--help", "-h"} {
		got := runOg(t, verbs, nil, "project", flag)
		if got.code != 0 || got.stubOut != "" {
			t.Fatalf("%s: code %d, ran target %q (stderr %q)", flag, got.code, got.stubOut, got.stderr)
		}
		if first, _, _ := strings.Cut(got.stdout, "\n"); first != "og project — Multi-host projects" {
			t.Errorf("%s: first line %q", flag, first)
		}
		for _, want := range []string{"Usage: og project <subcommand>", "  status [<project>]"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("%s: help lacks %q:\n%s", flag, want, got.stdout)
			}
		}
		if last := lastLine(got.stdout); last != target {
			t.Errorf("%s: last line %q, want the target", flag, last)
		}
	}

	got := runOg(t, verbs, nil, "project", "status", "--help")
	if want := "2\n[status]\n[--help]\n"; got.code != 0 || got.stubOut != want {
		t.Errorf("code %d, stub saw %q, want %q (stderr %q)", got.code, got.stubOut, want, got.stderr)
	}
}

func TestProjectModelShapeAndCompletion(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := projectVerbs(target)

	parser, err := buildParser(mustTable(t, verbs))
	if err != nil {
		t.Fatal(err)
	}
	project := child(t, parser.Model.Node, "project")
	if got := positionals(project); !slices.Equal(got, []string{"args"}) {
		t.Errorf("project positionals = %v, want the passthrough arg", got)
	}
	if len(flagNames(project)) != 0 || len(project.Children) != 0 {
		t.Errorf("project has flags %v / children %v", flagNames(project), childNames(project))
	}

	got := runOg(t, verbs, []string{"COMP_LINE=og project status "})
	if got.code != 0 || got.stubOut != "" {
		t.Errorf("completion of `og project status`: code %d, ran target %q (stderr %q)", got.code, got.stubOut, got.stderr)
	}
	got = runOg(t, verbs, []string{"COMP_LINE=og pro"})
	if strings.TrimSpace(got.stdout) != "project" {
		t.Errorf("noun completion = %q", got.stdout)
	}
}
