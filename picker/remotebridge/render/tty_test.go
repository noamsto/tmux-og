package render

import (
	"os"
	"testing"
)

func TestSizeNonTTYErrors(t *testing.T) {
	// A pipe is not a tty; Size must return an error, not panic.
	r, _, _ := os.Pipe()
	defer r.Close() //nolint:errcheck // test cleanup
	if _, _, err := Size(int(r.Fd())); err == nil {
		t.Error("expected error sizing a non-tty")
	}
}
