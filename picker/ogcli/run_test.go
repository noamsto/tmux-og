package ogcli

import (
	"os"
	"slices"
	"strings"
	"testing"
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
		{"go-backed verb keeps flag position", []string{"remote", "open", "--into", "x", "host", "sess"}, "4\n[--into]\n[x]\n[host]\n[sess]\n"},
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
			{"remote", "open", "host", "--help"},
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

func TestRunGoBackedValidation(t *testing.T) {
	target := stubTarget(t, t.TempDir())
	verbs := stubVerbs(target)

	for _, args := range [][]string{
		{"remote", "open", "host", "--bogus"},
		{"remote", "open", "--bogus", "host"},
		{"remote", "open"},
		{"remote", "open", "a", "b", "c", "d"},
	} {
		got := runOg(t, verbs, nil, args...)
		if got.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, got.code)
		}
		if got.stubOut != "" {
			t.Errorf("%v: target ran: %q", args, got.stubOut)
		}
		if got.stdout != "" || !strings.HasPrefix(got.stderr, "og: ") {
			t.Errorf("%v: stdout %q stderr %q", args, got.stdout, got.stderr)
		}
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
