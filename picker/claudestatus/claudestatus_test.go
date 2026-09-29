package claudestatus

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultDirMatchesVector(t *testing.T) {
	vector, err := os.ReadFile("testdata/default-dir.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(strings.TrimSpace(string(vector)), "{uid}", strconv.Itoa(os.Getuid()))
	if got := DefaultDir(); got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}
}

func TestDirHonoursOverride(t *testing.T) {
	t.Setenv("CLAUDE_STATUS_DIR", "/tmp/custom-status-dir")
	if got := Dir(); got != "/tmp/custom-status-dir" {
		t.Fatalf("Dir() = %q, want override", got)
	}

	t.Setenv("CLAUDE_STATUS_DIR", "")
	if got := Dir(); got != DefaultDir() {
		t.Fatalf("Dir() = %q, want default %q", got, DefaultDir())
	}
}

func TestEnsureFreshNestedPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "dir")
	if !Ensure(root) {
		t.Fatal("Ensure() on a fresh nested path should return true")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("mode = %o, want 0700", perm)
	}
}

func TestEnsureExistingLooseDirNotRepaired(t *testing.T) {
	dir := t.TempDir() // 0755 by default (0777 &^ umask)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if Ensure(dir) {
		t.Fatal("Ensure() on an existing 0755 dir should return false")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode = %o, want unchanged 0755 (no repair)", perm)
	}
}

func TestEnsureSymlinkToPrivateDir(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if Ensure(link) {
		t.Fatal("Ensure() on a symlink to an owner-only dir should return false")
	}
}

func TestEnsureRegularFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Ensure(file) {
		t.Fatal("Ensure() on a regular file should return false")
	}
}

func TestTrustedDelegatesToOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !Trusted(dir) {
		t.Fatal("Trusted() on an owner-only dir should return true")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if Trusted(dir) {
		t.Fatal("Trusted() on a 0755 dir should return false")
	}
}
