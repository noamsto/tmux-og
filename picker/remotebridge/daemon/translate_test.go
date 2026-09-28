package daemon

import (
	"reflect"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestTranslateWindowNotification(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")

	cases := []struct {
		name string
		line controlmode.Line
		argv []string
		ok   bool
	}{
		// A rename is two argvs (applyMirrorName), so Run's loop owns it.
		{"rename is handled in Run's loop", controlmode.Line{Kind: controlmode.WindowRenamed, Args: []string{"@2"}, Data: []byte("my name")},
			nil, false},
		{"active-changed in registry", controlmode.Line{Kind: controlmode.SessionWindowChanged, Args: []string{"$1", "@1"}},
			[]string{"select-window", "-t", "@101"}, true},
		{"active-changed out of registry (B2)", controlmode.Line{Kind: controlmode.SessionWindowChanged, Args: []string{"$1", "@9"}},
			nil, false},
		{"pane-changed is a no-op (M2.2)", controlmode.Line{Kind: controlmode.WindowPaneChanged, Args: []string{"@1", "%3"}},
			nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv, ok := translateWindowNotification(c.line, reg)
			if ok != c.ok || !reflect.DeepEqual(argv, c.argv) {
				t.Errorf("got (%v,%v), want (%v,%v)", argv, ok, c.argv, c.ok)
			}
		})
	}
}

// applyMirrorName must write BOTH the option reflow labels from and the window
// name itself: with automatic-rename off on a mirror window, a path that wrote
// only the option would leave the tab frozen at the previous name. An empty
// remote name must clear both instead of no-opping (#845).
func TestApplyMirrorName(t *testing.T) {
	var got [][]string
	cfg := Config{LocalTmux: func(args ...string) error {
		got = append(got, args)
		return nil
	}}

	applyMirrorName(cfg, "h-s:2", "a|b")
	want := [][]string{
		{"set-option", "-w", "-t", "h-s:2", "@window_bridge_name", "ab"},
		{"rename-window", "-t", "h-s:2", "ab"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applyMirrorName = %v, want %v", got, want)
	}

	got = nil
	applyMirrorName(cfg, "h-s:2", "")
	want = [][]string{
		{"set-option", "-w", "-t", "h-s:2", "-u", "@window_bridge_name"},
		{"rename-window", "-t", "h-s:2", "#{b:pane_current_path}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applyMirrorName(empty) = %v, want %v", got, want)
	}
}
