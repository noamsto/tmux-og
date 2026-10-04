package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session row's kill command carries the probe-validated id as its final ssh
// arg; the hostile remote name never reaches it.
func TestKillRunSessionRowCommand(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + argvFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	r := newKillRun([]listItem{{isRemoteRow: true, remoteHost: "lab", remoteSess: `x\';id;#`, remoteSessionID: "$7"}})
	r.run()
	<-r.done

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake ssh never ran: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	t.Logf("ssh argv: %q", argv)
	if got, want := argv[len(argv)-1], remoteKillSessionBody("$7"); got != want {
		t.Errorf("final arg = %q, want %q", got, want)
	}
	if joined := string(raw); strings.Contains(joined, `x\`) || strings.Contains(joined, "id;#") {
		t.Errorf("argv leaks the remote session name: %q", joined)
	}
}
