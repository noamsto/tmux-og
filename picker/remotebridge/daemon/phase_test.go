package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSetPhaseWritesOneLineBesideTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "og-daemon-h-s.sock")
	cfg := Config{SockPath: sock}

	setPhase(cfg, "attached to %s", "halo")
	got, err := os.ReadFile(sock + ".phase")
	if err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if string(got) != "attached to halo\n" {
		t.Fatalf("phase = %q", got)
	}

	// A later phase replaces the earlier one rather than appending: the pane
	// reads the first line and would otherwise be stuck on the first caption.
	setPhase(cfg, "mirroring window %d/%d", 1, 3)
	got, err = os.ReadFile(sock + ".phase")
	if err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if string(got) != "mirroring window 1/3\n" {
		t.Fatalf("phase = %q", got)
	}

	clearPhase(cfg)
	if _, err := os.Stat(sock + ".phase"); !os.IsNotExist(err) {
		t.Fatalf("phase file survived clear: %v", err)
	}
}

// A newline would put the rest of the caption on a line the pane never renders
// and, worse, leave the visible half looking complete.
func TestSetPhaseCollapsesNewlines(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	setPhase(Config{SockPath: sock}, "a\nb")
	got, _ := os.ReadFile(sock + ".phase")
	if string(got) != "a b\n" {
		t.Fatalf("phase = %q", got)
	}
}

// The --test-local harness has no socket, so a phase write must not drop a
// file into whatever directory the daemon happens to have been started in.
func TestSetPhaseWithoutSocketWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	setPhase(Config{}, "attached")
	clearPhase(Config{})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote %d entries with no socket path", len(entries))
	}
}

func TestWriteNoFollowRefusesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x.phase", "x.pid"} {
		link := filepath.Join(dir, name)
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		if err := writeNoFollow(link, []byte("x"), 0o600); err == nil {
			t.Fatalf("%s: write through symlink succeeded", name)
		}
	}
	setPhase(Config{SockPath: filepath.Join(dir, "x")}, "hello")
	got, _ := os.ReadFile(victim)
	if string(got) != "precious" {
		t.Fatalf("victim = %q", got)
	}
}

func TestWriteNoFollowRefusesPlantedHardLink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.phase")
	if err := os.Link(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := writeNoFollow(path, []byte("x"), 0o600); err == nil {
		t.Fatal("write through hard link succeeded")
	}
	got, _ := os.ReadFile(victim)
	if string(got) != "precious" {
		t.Fatalf("victim = %q", got)
	}
}

func TestWriteNoFollowRefusesPlantedFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.phase")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := writeNoFollow(path, []byte("x"), 0o600); err == nil {
		t.Fatal("write to FIFO succeeded")
	}
	buf := make([]byte, 8)
	if n, _ := r.Read(buf); n != 0 {
		t.Fatalf("reader received %q", buf[:n])
	}
}

func TestPrivateRegular(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  os.FileMode
		uid   uint32
		nlink uint64
		want  bool
	}{
		{"own regular single link", 0o600, 1000, 1, true},
		{"foreign uid", 0o600, 0, 1, false},
		{"extra hard link", 0o600, 1000, 2, false},
		{"fifo", os.ModeNamedPipe | 0o600, 1000, 1, false},
		{"symlink", os.ModeSymlink | 0o600, 1000, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := privateRegular(tc.mode, tc.uid, tc.nlink, 1000); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWriteNoFollowCreatesAndOverwritesOwnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.phase")
	for _, want := range []string{"a long first value", "b"} {
		if err := writeNoFollow(path, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
