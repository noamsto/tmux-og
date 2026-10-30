package daemon

import (
	"slices"
	"testing"
)

// move-window/link-window into the bridged session makes tmux send
// %window-add @N and then %window-close @N (the source session's unlink,
// reported as a close because the client's session now holds the window).
func TestCloseWindowIfGone(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantClosed bool
	}{
		{"window still listed", "%begin 1 1 1\n1 @2 1 moved\n%end 1 1 1\n", false},
		{"window gone", "%begin 1 1 1\n1 @3 1 other\n%end 1 1 1\n", true},
		{"list fails", "%begin 1 1 1\nnope\n%error 1 1 1\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			cfg := Config{
				RemoteSession: "rem",
				LocalTmux: func(args ...string) error {
					calls = append(calls, args)
					return nil
				},
			}
			reg := newRegistry()
			reg.add("@2", "@102")
			rt, _ := scriptedRT(tc.script)

			got := closeWindowIfGone(cfg, NewRouter(), newCtlState(), reg, newConverger(), rt, "@2")

			if got != tc.wantClosed {
				t.Errorf("closed = %v, want %v", got, tc.wantClosed)
			}
			if _, ok := reg.byRemoteID("@2"); ok == tc.wantClosed {
				t.Errorf("@2 registered = %v, want %v", ok, !tc.wantClosed)
			}
			killed := slices.Contains(killedWindows(calls), "@102")
			if killed != tc.wantClosed {
				t.Errorf("kill-window @102 = %v, want %v (calls %v)", killed, tc.wantClosed, calls)
			}
		})
	}
}

// A listed-but-unselected window is closed; a malformed flag is no evidence
// either way, so it is kept (as reconcileWindows keeps it).
func TestCloseWindowIfGoneHonoursTheSelector(t *testing.T) {
	for _, tc := range []struct {
		flag       string
		wantClosed bool
	}{{"0", true}, {"1", false}, {"x", false}} {
		t.Run("flag "+tc.flag, func(t *testing.T) {
			_, cfg := newStartupFake()
			cfg = selectorCfg(t, cfg, "@crew=x")
			cfg.RemoteSession = "rem"
			reg := newRegistry()
			reg.add("@2", "@102")
			rt, _ := scriptedRT("%begin 1 1 1\n1 @2 1 " + tc.flag + " moved\n%end 1 1 1\n")

			got := closeWindowIfGone(cfg, NewRouter(), newCtlState(), reg, newConverger(), rt, "@2")

			if got != tc.wantClosed {
				t.Errorf("closed = %v, want %v", got, tc.wantClosed)
			}
		})
	}
}
