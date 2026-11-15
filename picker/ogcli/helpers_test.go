package ogcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The test binary doubles as the og binary: syscall.Exec and
// kongcompletion.Register replace or end the process, so those paths run in a
// child of this executable instead of in-process.
const (
	envTable = "OGCLI_TEST_TABLE"
	envStub  = "OGCLI_STUB_OUT"
)

func TestMain(m *testing.M) {
	if table := os.Getenv(envTable); table != "" {
		os.Exit(Main(table, os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// stubVerbs is the table most tests share. Targets are filled by withTarget.
func stubVerbs(target string) []Verb {
	return []Verb{
		{Verb: "q", Target: target, Summary: `Quote 'it' (now) -- \ done`},
		{Verb: "q sub", Target: target, Summary: "Sub of q"},
		{Verb: "g a", Target: target, Summary: "Alpha"},
		{Verb: "g b", Target: target, Summary: "Beta"},
		{Verb: "s", Target: target, Summary: "Status"},
		{Verb: "s x", Target: target, Summary: "Sub x"},
		{Verb: "plain", Target: target, Summary: "Just a verb"},
		{Verb: "remote open", Target: target, Summary: "Open a remote", Model: "remote-open"},
	}
}

func mustTable(t *testing.T, verbs []Verb) *Table {
	t.Helper()
	data, err := json.Marshal(verbs)
	if err != nil {
		t.Fatal(err)
	}
	table, err := ParseTable(data)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// stubTarget writes an executable that records its argv to $OGCLI_STUB_OUT,
// one "$#" line then one "[arg]" line per argument.
func stubTarget(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "stub")
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' \"$#\"; for a in \"$@\"; do printf '[%s]\\n' \"$a\"; done; } > \"$" + envStub + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test executable must be executable
		t.Fatal(err)
	}
	return path
}

type ogRun struct {
	stdout, stderr string
	code           int
	stubOut        string // what the stub target recorded; empty if it never ran
}

// runOg runs the test binary as og over verbs and returns what it printed.
func runOg(t *testing.T, verbs []Verb, env []string, args ...string) ogRun {
	t.Helper()
	dir := t.TempDir()
	tablePath := filepath.Join(dir, "table.json")
	data, err := json.Marshal(verbs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tablePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stubOut := filepath.Join(dir, "stub.out")
	cmd := exec.Command(self, args...) //nolint:gosec // G204: re-executes the test binary
	// The race runtime sleeps a second at exit by default.
	cmd.Env = append(os.Environ(), envTable+"="+tablePath, envStub+"="+stubOut, "GORACE=atexit_sleep_ms=0")
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	rec, _ := os.ReadFile(stubOut) //nolint:gosec // G304: a path inside t.TempDir
	return ogRun{stdout: out.String(), stderr: errb.String(), code: code, stubOut: string(rec)}
}
