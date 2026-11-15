package remoteopen

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestRunnerCaptureStripsTrailingNewlines(t *testing.T) {
	tests := []struct {
		name, script, want string
	}{
		{"all trailing newlines", `printf 'a\n\n'`, "a"},
		{"inner newline kept", `printf 'a\nb\n'`, "a\nb"},
		{"no newline", `printf a`, "a"},
	}
	var r execRunner
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", tt.script}, Capture: true})
			if err != nil || status != 0 {
				t.Fatalf("status %d, err %v", status, err)
			}
			if out != tt.want {
				t.Fatalf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestRunnerNonZeroStatusIsNotAnError(t *testing.T) {
	var r execRunner
	out, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", "echo partial; exit 3"}, Capture: true})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != 3 || out != "partial" {
		t.Fatalf("got (%q, %d), want (\"partial\", 3)", out, status)
	}
}

func TestRunnerSignalledChildStatus(t *testing.T) {
	var r execRunner
	_, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", "kill -TERM $$"}})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != 128+int(syscall.SIGTERM) {
		t.Fatalf("status = %d, want %d", status, 128+int(syscall.SIGTERM))
	}
}

func TestRunnerMissingProgram(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var r execRunner
	_, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", "true"}})
	if status != 127 {
		t.Fatalf("status = %d, want 127", status)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err = %v, want one wrapping exec.ErrNotFound", err)
	}
}

// Swaps os.Stderr for a pipe, so this test must not run in parallel.
func TestRunnerDiscardedStderr(t *testing.T) {
	runWithStderrPipe := func(mode stderrMode) string {
		t.Helper()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pr.Close() }()
		saved := os.Stderr
		os.Stderr = pw
		var r execRunner
		_, status, runErr := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", "echo x >&2; exit 3"}, Stderr: mode})
		os.Stderr = saved
		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}
		if runErr != nil || status != 3 {
			t.Fatalf("status %d, err %v", status, runErr)
		}
		got, err := io.ReadAll(pr)
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}
	if got := runWithStderrPipe(stderrInherit); got != "x\n" {
		t.Fatalf("inherited stderr = %q, want %q", got, "x\n")
	}
	if got := runWithStderrPipe(stderrDiscard); got != "" {
		t.Fatalf("discarded stderr reached os.Stderr: %q", got)
	}
}

func TestRunnerStdinString(t *testing.T) {
	var r execRunner
	in := "line one\nline two\n"
	out, status, err := r.Run(cmdSpec{Name: "cat", Stdin: &in, Capture: true})
	if err != nil || status != 0 {
		t.Fatalf("status %d, err %v", status, err)
	}
	if out != "line one\nline two" {
		t.Fatalf("out = %q", out)
	}
}

func TestRunnerMergedStderr(t *testing.T) {
	var r execRunner
	out, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}, Stderr: stderrMerge})
	if err != nil || status != 0 {
		t.Fatalf("status %d, err %v", status, err)
	}
	if out != "out\nerr" {
		t.Fatalf("out = %q, want %q", out, "out\nerr")
	}
}

func TestRunnerHooksOrder(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	var events []string
	r := execRunner{
		before: func() {
			if _, err := os.Stat(marker); err == nil {
				t.Error("before hook ran after the child")
			}
			events = append(events, "before")
		},
		after: func() {
			if _, err := os.Stat(marker); err != nil {
				t.Error("after hook ran before the child finished")
			}
			events = append(events, "after")
		},
	}
	if _, status, err := r.Run(cmdSpec{Name: "sh", Args: []string{"-c", `: > "$1"`, "sh", marker}}); err != nil || status != 0 {
		t.Fatalf("status %d, err %v", status, err)
	}
	if want := []string{"before", "after"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestSignalWatchObservesSelfSignal(t *testing.T) {
	w := watchSignals()
	if _, ok := w.pending(); ok {
		t.Fatal("pending before any signal")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sig, ok := w.pending(); ok {
			if sig != syscall.SIGHUP {
				t.Fatalf("pending = %v, want SIGHUP", sig)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("SIGHUP not observed within 2s")
}

func TestExitReleasesBeforeOsExit(t *testing.T) {
	var events []string
	e := exiter{
		release: func() { events = append(events, "release") },
		osExit:  func(code int) { events = append(events, "exit "+strconv.Itoa(code)) },
	}
	e.exit(143)
	if want := []string{"release", "exit 143"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestExitWithoutRelease(t *testing.T) {
	got := -1
	e := exiter{osExit: func(code int) { got = code }}
	e.exit(2)
	if got != 2 {
		t.Fatalf("osExit code = %d, want 2", got)
	}
}
