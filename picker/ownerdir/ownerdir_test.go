package ownerdir

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakeFileInfo lets ownedPrivate be exercised against a foreign uid, which
// isn't reachable without root in a real filesystem test.
type fakeFileInfo struct {
	perm os.FileMode
	sys  any
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.perm }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return true }
func (f fakeFileInfo) Sys() any           { return f.sys }

func TestOwnedPrivate(t *testing.T) {
	uid := os.Getuid()
	tests := []struct {
		name string
		info fs.FileInfo
		uid  int
		want bool
	}{
		{"owner match 0700", fakeFileInfo{perm: 0o700, sys: &syscall.Stat_t{Uid: uint32(uid)}}, uid, true},
		{"foreign uid", fakeFileInfo{perm: 0o700, sys: &syscall.Stat_t{Uid: uint32(uid + 1)}}, uid, false},
		{"own uid 0770", fakeFileInfo{perm: 0o770, sys: &syscall.Stat_t{Uid: uint32(uid)}}, uid, false},
		{"own uid 0707", fakeFileInfo{perm: 0o707, sys: &syscall.Stat_t{Uid: uint32(uid)}}, uid, false},
		{"sys not Stat_t", fakeFileInfo{perm: 0o700, sys: "nope"}, uid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownedPrivate(tt.info, tt.uid); got != tt.want {
				t.Fatalf("ownedPrivate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOwnerOnly(t *testing.T) {
	// t.TempDir() is created 0777&^umask, not owner-only.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !OwnerOnly(dir) {
		t.Fatal("owner-only dir should be owner-only")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if OwnerOnly(dir) {
		t.Fatal("0755 dir should not be owner-only")
	}
	if OwnerOnly(filepath.Join(dir, "missing")) {
		t.Fatal("missing path should not be owner-only")
	}

	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if OwnerOnly(file) {
		t.Fatal("regular file should not be owner-only")
	}

	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(real), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if OwnerOnly(link) {
		t.Fatal("symlink to an owner-only dir should not be owner-only")
	}
}
