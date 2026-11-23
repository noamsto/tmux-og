package probekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"

	"github.com/noamsto/tmux-og/picker/ownerdir"
)

// WriteOwnerOnlyJSON atomically writes v as dir/name, creating dir 0700. A
// dir that is not owner-only is never written into.
func WriteOwnerOnlyJSON(dir, name string, v any) {
	if dir == "" || os.MkdirAll(dir, 0o700) != nil || !ownerdir.OwnerOnly(dir) {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return
	}
	// Rename, so a picker reading concurrently never sees a torn file.
	if os.Rename(f.Name(), filepath.Join(dir, name)) != nil {
		_ = os.Remove(f.Name())
	}
}

// ReadOwnerOnlyJSON decodes dir/name into v, trusting it only when dir is
// owner-only and the entry is a regular file owned by this uid.
func ReadOwnerOnlyJSON(dir, name string, v any) bool {
	if dir == "" || !ownerdir.OwnerOnly(dir) {
		return false
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: value is non-negative and fits uint32
		return false
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path built from trusted local state, not user input
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}
