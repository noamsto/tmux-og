package daemon

import (
	"errors"
	"reflect"
	"testing"
)

func placedNudgeCfg(out string, err error) (Config, *[][]string) {
	var calls [][]string
	return Config{
		SockPath:    "/s/mine.sock",
		LocalSess:   "host-sess",
		Placed:      true,
		LocalTarget: "$4",
		LocalTmux: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) { return out, err },
	}, &calls
}

func TestRegisterResizeNudgePlacedIssuesNoHookClear(t *testing.T) {
	cfg, calls := placedNudgeCfg("", nil)
	registerResizeNudge(cfg, "/s/og-nudge-1-4.resize")
	want := [][]string{{"set-option", "-t", "$4", "@bridge_nudge", "/s/og-nudge-1-4.resize"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

func TestRegisterResizeNudgeDedicatedStillClearsLegacyHooks(t *testing.T) {
	var calls [][]string
	cfg := Config{LocalSess: "m", LocalTmux: func(args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	registerResizeNudge(cfg, "/s/a.sock.resize")
	if len(calls) != len(legacyResizeHooks)+1 || calls[0][0] != "set-hook" {
		t.Errorf("calls = %v", calls)
	}
}

func TestUnregisterResizeNudgePlaced(t *testing.T) {
	unset := [][]string{{"set-option", "-u", "-t", "$4", "@bridge_nudge"}}
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want [][]string
	}{
		{"another daemon's placed window keeps it", "@2|1|/s/mine.sock\n@6|1|/s/other.sock\n", nil, nil},
		{"only ours unsets it", "@2|1|/s/mine.sock\n@3||\n", nil, unset},
		{"a local window with a foreign sock stamp is not a placed daemon", "@2|1|/s/mine.sock\n@3||/s/other.sock\n", nil, unset},
		{"no windows unsets it", "", nil, unset},
		{"a failed read leaves it set", "", errors.New("no server"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, calls := placedNudgeCfg(tc.out, tc.err)
			unregisterResizeNudge(cfg)
			if !reflect.DeepEqual(*calls, tc.want) {
				t.Errorf("calls = %v, want %v", *calls, tc.want)
			}
		})
	}
}

func TestUnregisterResizeNudgeDedicatedUnsetsTheSessionOption(t *testing.T) {
	var calls [][]string
	cfg := Config{LocalSess: "m", LocalTmux: func(args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	unregisterResizeNudge(cfg)
	want := [][]string{{"set-option", "-u", "-t", "m", "@bridge_nudge"}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
