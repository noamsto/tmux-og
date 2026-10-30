package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// v1 builds a v1 layout string from a body with tmux's checksum.
func v1(body string) string {
	var csum uint16
	for i := range len(body) {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += uint16(body[i])
	}
	return fmt.Sprintf("%04x,%s", csum, body)
}

const (
	// Three even columns (%0 %1 %2) at 200x50, as a v2 dump, and the window
	// after tmux killed %1 (v1, as #{window_layout} prints it).
	prevV2     = `{"V":2,"L":{"t":"h","w":200,"h":50,"x":0,"y":0,"c":[{"t":"p","w":66,"h":50,"x":0,"y":0,"l":1,"i":0,"I":"%0"},{"t":"p","w":66,"h":50,"x":67,"y":0,"l":0,"i":1,"I":"%1"},{"t":"p","w":66,"h":50,"x":134,"y":0,"a":true,"i":2,"I":"%2"}]}}`
	prevV1Body = "200x50,0,0{66x50,0,0,0,66x50,67,0,1,66x50,134,0,2}"
	curV1Body  = "200x50,0,0{66x50,0,0,0,133x50,67,0,2}"
	evenBody   = "200x50,0,0{99x50,0,0,0,100x50,100,0,2}"
	// The P/i field as read: floats skipped, a trailing space after each pane.
	geom = "%0.0.0.66.50 %2.67.0.133.50 "
)

// fakeTmux records every call and answers the first one (the state read).
type fakeTmux struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeTmux) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

func state(dwindle, zoomed, grid, bridge, size, panes, layout string) string {
	return strings.Join([]string{dwindle, zoomed, grid, bridge, size, panes, layout}, "|")
}

func goodState() string {
	return state("on", "0", "", "", "200x50", geom, v1(curV1Body))
}

func cache(prev string) string { return "200x50|" + prev }

func TestBalanceHappyPath(t *testing.T) {
	for name, prev := range map[string]string{"v2": prevV2, "v1": v1(prevV1Body)} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{out: goodState()}
			if code := run([]string{"balance", "@1", cache(prev)}, f.run); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(f.calls) != 2 {
				t.Fatalf("calls = %v, want state read + if-shell", f.calls)
			}
			read := f.calls[0]
			if strings.Join(read[:4], " ") != "display-message -p -t @1" || read[4] != stateFormat {
				t.Errorf("read call = %q", read)
			}
			apply := f.calls[1]
			if len(apply) != 6 || strings.Join(apply[:4], " ") != "if-shell -t @1 -F" {
				t.Fatalf("apply call = %q", apply)
			}
			guard, cmd := apply[4], apply[5]
			for _, want := range []string{
				"#{==:#{@dwindle},on}",
				"#{==:#{window_zoomed_flag},0}",
				"#{!=:#{@crew_grid},1}",
				"#{!:#{@bridge_win}}",
				"#{window_width}x#{window_height}|#{P/i:",
				",200x50|" + geom + "}",
			} {
				if !strings.Contains(guard, want) {
					t.Errorf("guard %q lacks %q", guard, want)
				}
			}
			if want := "select-layout -t @1 '" + v1(evenBody) + "'"; cmd != want {
				t.Errorf("cmd = %q, want %q", cmd, want)
			}
		})
	}
}

func TestBalancePrechecks(t *testing.T) {
	tests := map[string]string{
		"dwindle off":   state("off", "0", "", "", "200x50", geom, v1(curV1Body)),
		"dwindle unset": state("", "0", "", "", "200x50", geom, v1(curV1Body)),
		"zoomed":        state("on", "1", "", "", "200x50", geom, v1(curV1Body)),
		"crew grid":     state("on", "0", "1", "", "200x50", geom, v1(curV1Body)),
		"bridge mirror": state("on", "0", "", "1", "200x50", geom, v1(curV1Body)),
		"wrong fields":  "on|0|",
		"empty":         "",
	}
	for name, out := range tests {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{out: out}
			if code := run([]string{"balance", "@1", cache(prevV2)}, f.run); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(f.calls) != 1 {
				t.Errorf("calls = %v, want only the read", f.calls)
			}
		})
	}
}

func TestBalanceOrderMismatch(t *testing.T) {
	f := &fakeTmux{out: state("on", "0", "", "", "200x50", "%2.67.0.133.50 %0.0.0.66.50 ", v1(curV1Body))}
	if code := run([]string{"balance", "@1", cache(prevV2)}, f.run); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want only the read", f.calls)
	}
}

func TestBalanceUnchangedLayout(t *testing.T) {
	f := &fakeTmux{out: state("on", "0", "", "", "200x50", "%0.0.0.99.50 %2.100.0.100.50 ", v1(evenBody))}
	if code := run([]string{"balance", "@1", cache(prevV2)}, f.run); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want only the read", f.calls)
	}
}

func TestBalanceUnparsableLayout(t *testing.T) {
	f := &fakeTmux{out: goodState()}
	if code := run([]string{"balance", "@1", cache("garbage")}, f.run); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want only the read", f.calls)
	}
}

func TestBalanceReadError(t *testing.T) {
	f := &fakeTmux{err: errors.New("no server")}
	if code := run([]string{"balance", "@1", cache(prevV2)}, f.run); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v, want only the read", f.calls)
	}
}

func TestBalanceNoTmuxCall(t *testing.T) {
	tests := map[string][]string{
		"bad window":    {"balance", "1", cache(prevV2)},
		"window suffix": {"balance", "@1;kill-server", cache(prevV2)},
		"no separator":  {"balance", "@1", "200x50"},
		"empty layout":  {"balance", "@1", "200x50|"},
		"empty cache":   {"balance", "@1", ""},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{out: goodState()}
			if code := run(args, f.run); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(f.calls) != 0 {
				t.Errorf("calls = %v, want none", f.calls)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	tests := map[string][]string{
		"no args":  nil,
		"bad verb": {"unbalance", "@1", "x|y"},
		"too few":  {"balance", "@1"},
		"too many": {"balance", "@1", "x|y", "z"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{}
			if code := run(args, f.run); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if len(f.calls) != 0 {
				t.Errorf("calls = %v, want none", f.calls)
			}
		})
	}
}
