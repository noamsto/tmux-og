package main

import (
	"os/exec"
	"runtime"
	"testing"
)

// fakeLookup returns nil for any name in found, exec.ErrNotFound otherwise.
func fakeLookup(found map[string]bool) func(string) (string, error) {
	return func(name string) (string, error) {
		if found[name] {
			return "/fake/bin/" + name, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

func TestCheckLocal(t *testing.T) {
	opener := openerName()
	found := map[string]bool{
		"tmux":  true,
		"ssh":   true,
		"xclip": true,
		opener:  true,
	}
	results := CheckLocal(fakeLookup(found))

	byName := make(map[string]Result, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	if r := byName["tmux"]; !r.OK {
		t.Errorf("tmux: OK = false, want true")
	}
	if r := byName["gh"]; r.OK || r.Detail != "not found" {
		t.Errorf("gh = %+v, want OK=false Detail=%q", r, "not found")
	}
	if r := byName["xclip/wl-paste/osascript"]; !r.OK {
		t.Errorf("xclip/wl-paste/osascript = %+v, want OK=true (xclip present)", r)
	}
	if r := byName[opener]; !r.OK {
		t.Errorf("%s = %+v, want OK=true", opener, r)
	}
}

func TestCheckClipboardEither(t *testing.T) {
	tests := []struct {
		name  string
		found map[string]bool
		want  bool
	}{
		{"both present", map[string]bool{"xclip": true, "wl-paste": true}, true},
		{"only xclip", map[string]bool{"xclip": true}, true},
		{"only wl-paste", map[string]bool{"wl-paste": true}, true},
		{"only osascript", map[string]bool{"osascript": true}, true},
		{"neither", map[string]bool{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := checkClipboard(fakeLookup(tt.found))
			if r.OK != tt.want {
				t.Fatalf("OK = %v, want %v", r.OK, tt.want)
			}
			if !tt.want && r.Detail != "paste forwards the raw byte instead (documented degrade)" {
				t.Fatalf("Detail = %q", r.Detail)
			}
			if tt.want && r.Detail != "" {
				t.Fatalf("Detail = %q, want empty", r.Detail)
			}
		})
	}
}

func TestCheckOpenerPlatform(t *testing.T) {
	wantName := "xdg-open"
	if runtime.GOOS == "darwin" {
		wantName = "open"
	}
	if got := openerName(); got != wantName {
		t.Fatalf("openerName() = %q, want %q", got, wantName)
	}

	present := checkOpener(fakeLookup(map[string]bool{wantName: true}))
	if !present.OK || present.Name != wantName {
		t.Fatalf("present = %+v", present)
	}

	absent := checkOpener(fakeLookup(map[string]bool{}))
	if absent.OK || absent.Detail != "not found" {
		t.Fatalf("absent = %+v", absent)
	}
}
