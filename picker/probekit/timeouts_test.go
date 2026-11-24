package probekit

import (
	"slices"
	"testing"
)

func TestSSHArgv(t *testing.T) {
	got := SSHArgv("lab", "echo hi")
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=2", "-T", "lab", "--", "echo hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("SSHArgv = %q, want %q", got, want)
	}
}
