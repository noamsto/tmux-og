package remoteopen

import (
	"bytes"
	"testing"
)

// Expected values are from coreutils `cksum` (first field, then byte count).
func TestCksum(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want uint32
	}{
		{"empty", []byte{}, 4294967295},
		{"abc", []byte("abc"), 1219131554},
		{"nul separated", []byte("h\x00s\x00"), 4200595469},
		{"300 bytes", bytes.Repeat([]byte("x"), 300), 3786917833},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cksum(tt.data); got != tt.want {
				t.Fatalf("cksum(%q) = %d, want %d", tt.data, got, tt.want)
			}
		})
	}
}

func TestIntoSockPath(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		host     string
		sess     string
		pid      string
		intoID   string
		selector string
		want     string
	}{
		{
			name:   "no selector",
			base:   "/x",
			host:   "tp-g6",
			sess:   "workstation",
			pid:    "4242",
			intoID: "$3",
			want:   "/x/og-into-4266494792.sock",
		},
		{
			name:     "with selector",
			base:     "/x",
			host:     "tp-g6",
			sess:     "workstation",
			pid:      "4242",
			intoID:   "$3",
			selector: "@crew_branch=B",
			want:     "/x/og-into-2060509569.sock",
		},
		{
			name:   "different key",
			base:   "/x",
			host:   "h",
			sess:   "s",
			pid:    "1",
			intoID: "$1",
			want:   "/x/og-into-2049584062.sock",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := intoSockPath(tt.base, tt.host, tt.sess, tt.pid, tt.intoID, tt.selector); got != tt.want {
				t.Fatalf("intoSockPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIntoBaseDir(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"tmux tmpdir wins", map[string]string{"TMUX_TMPDIR": "/t", "XDG_RUNTIME_DIR": "/r"}, "/t"},
		{"xdg when tmpdir unset", map[string]string{"XDG_RUNTIME_DIR": "/r"}, "/r"},
		{"empty tmpdir falls through", map[string]string{"TMUX_TMPDIR": "", "XDG_RUNTIME_DIR": "/r"}, "/r"},
		{"empty xdg falls through", map[string]string{"TMUX_TMPDIR": "", "XDG_RUNTIME_DIR": ""}, "/tmp"},
		{"empty tmpdir, xdg unset", map[string]string{"TMUX_TMPDIR": ""}, "/tmp"},
		{"nothing set", map[string]string{}, "/tmp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			if got := intoBaseDir(getenv); got != tt.want {
				t.Fatalf("intoBaseDir() = %q, want %q", got, tt.want)
			}
		})
	}
}
