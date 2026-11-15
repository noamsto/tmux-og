package ogcli

import (
	"os/exec"
	"strings"
	"testing"
)

const stubFish = `complete -c og -f -n '__fish_use_subcommand' -a help -d 'List every verb'
complete -c og -f -n '__fish_use_subcommand' -a 'q' -d 'Quote \'it\' (now) -- \\ done'
complete -c og -f -n '__fish_use_subcommand' -a 'g' -d '2 subcommands'
complete -c og -f -n '__fish_use_subcommand' -a 's' -d 'Status'
complete -c og -f -n '__fish_use_subcommand' -a 'plain' -d 'Just a verb'
complete -c og -f -n '__fish_use_subcommand' -a 'remote' -d '1 subcommand'
complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = \'q\'' -a 'sub' -d 'Sub of q'
complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = \'g\'' -a 'a' -d 'Alpha'
complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = \'g\'' -a 'b' -d 'Beta'
complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = \'s\'' -a 'x' -d 'Sub x'
complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test $t[2] = \'remote\'' -a 'open' -d 'Open a remote'
complete -c og -n 'set -l t (commandline -opc); test (count $t) -ge 3; and test $t[2] = \'remote\'; and test $t[3] = \'open\'' -l into -d 'Place the remote window into this local session'
complete -c og -n 'set -l t (commandline -opc); test (count $t) -ge 3; and test $t[2] = \'remote\'; and test $t[3] = \'open\'' -l window -d 'With --into: place this remote window (@<id> or @<option>=<value>)'
`

func stubFishScript(t *testing.T) string {
	t.Helper()
	parser, err := buildParser(mustTable(t, stubVerbs("/stub")))
	if err != nil {
		t.Fatal(err)
	}
	return fishScript(parser.Model)
}

func TestFishScript(t *testing.T) {
	if got := stubFishScript(t); got != stubFish {
		t.Errorf("fish script:\n%s\nwant:\n%s", got, stubFish)
	}
}

func TestFishScriptShape(t *testing.T) {
	got := stubFishScript(t)
	if strings.Contains(got, "completion") {
		t.Error("the hidden completion command is offered")
	}
	flagLines := 0
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "' -l ") {
			flagLines++
			if !strings.Contains(line, `test $t[3] = \'open\'`) || !strings.Contains(line, "-ge 3") {
				t.Errorf("flag line is not gated on the verb: %s", line)
			}
			if strings.Contains(line, " -f ") {
				t.Errorf("flag line suppresses files: %s", line)
			}
		}
	}
	if flagLines != 2 {
		t.Errorf("%d flag lines, want 2 (into, window)", flagLines)
	}
}

func TestFishScriptOverMainOutput(t *testing.T) {
	got := runOg(t, stubVerbs("/stub"), nil, "completion", "fish")
	if got.code != 0 || got.stdout != stubFish {
		t.Errorf("code %d stdout:\n%s", got.code, got.stdout)
	}
}

// fishComplete asks a real fish what it offers for line after sourcing the
// script; skipped when fish is not installed (it is not in the build sandbox).
func fishComplete(t *testing.T, script, line string) string {
	t.Helper()
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed")
	}
	cmd := exec.Command(fish, "--no-config", "-c", script+"\ncomplete -C "+"'"+line+"'") //nolint:gosec // G204: test input
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fish: %v", err)
	}
	return string(out)
}

func TestFishGating(t *testing.T) {
	script := stubFishScript(t)

	if out := fishComplete(t, script, "og remote open --"); !strings.Contains(out, "--into") || !strings.Contains(out, "--window") {
		t.Errorf("flags missing after the verb:\n%s", out)
	}
	if out := fishComplete(t, script, "og remote --"); strings.Contains(out, "--into") {
		t.Errorf("flags offered before the verb is typed:\n%s", out)
	}
	if out := fishComplete(t, script, "og remote "); !strings.Contains(out, "open\tOpen a remote") || strings.Contains(out, "--into") {
		t.Errorf("subverb completion:\n%s", out)
	}
	if out := fishComplete(t, script, "og q --"); strings.Contains(out, "--into") {
		t.Errorf("flags offered for another verb:\n%s", out)
	}
	if out := fishComplete(t, script, "og "); !strings.Contains(out, "g\t2 subcommands") || !strings.Contains(out, "help\tList every verb") {
		t.Errorf("noun completion:\n%s", out)
	}
}
