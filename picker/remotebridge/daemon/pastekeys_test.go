package daemon

import "testing"

func TestSubmitHold(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"plain text", "hello world", 11},
		{"utf-8 text", "héllo ✓ 日本", len("héllo ✓ 日本")},

		// Submit-capable and unknown input waits.
		{"CR", "\r", 0},
		{"LF", "\n", 0},
		{"ESC CR", "\x1b\r", 0},
		{"kitty enter", "\x1b[13u", 0},
		{"kitty shift enter", "\x1b[13;2u", 0},
		{"kitty ctrl+m", "\x1b[109;5u", 0},
		{"any kitty u", "\x1b[97u", 0},
		{"modifyOtherKeys enter", "\x1b[27;2;13~", 0},
		{"modifyOtherKeys other", "\x1b[27;5;99~", 0},
		{"SS3 keypad enter", "\x1bOM", 0},
		{"SS3 unknown final", "\x1bOP", 0},
		{"SS3 incomplete", "\x1bO", 0},
		{"paste start", "\x1b[200~", 0},
		{"paste end", "\x1b[201~", 0},
		{"lone ESC", "\x1b", 0},
		{"incomplete CSI params", "\x1b[1", 0},
		{"incomplete CSI", "\x1b[", 0},
		{"incomplete CSI-u", "\x1b[13;", 0},
		{"ESC x", "\x1bx", 0},
		{"ESC ESC", "\x1b\x1b", 0},
		{"unknown CSI final", "\x1b[5q", 0},
		{"ctrl+v", "\x16", 0},
		{"ctrl+c", "\x03", 0},
		{"ctrl+d", "\x04", 0},
		{"NUL", "\x00", 0},

		// Mouse reports can confirm an agent dialog.
		{"SGR mouse press", "\x1b[<0;10;5M", 0},
		{"SGR mouse release", "\x1b[<0;10;5m", 0},
		{"SGR mouse wheel", "\x1b[<64;10;5M", 0},
		{"X10 mouse", "\x1b[M !!", 0},

		// Editing keys pass.
		{"ctrl+a", "\x01", 1},
		{"ctrl+b", "\x02", 1},
		{"ctrl+e", "\x05", 1},
		{"ctrl+f", "\x06", 1},
		{"backspace", "\x08", 1},
		{"tab", "\x09", 1},
		{"ctrl+k", "\x0b", 1},
		{"ctrl+u", "\x15", 1},
		{"ctrl+w", "\x17", 1},
		{"DEL", "\x7f", 1},
		{"alt+b", "\x1bb", 2},
		{"alt+f", "\x1bf", 2},
		{"alt+d", "\x1bd", 2},
		{"alt+DEL", "\x1b\x7f", 2},
		{"arrow up", "\x1b[A", 3},
		{"arrow down", "\x1b[B", 3},
		{"arrow right", "\x1b[C", 3},
		{"arrow left", "\x1b[D", 3},
		{"CSI E", "\x1b[E", 3},
		{"CSI F", "\x1b[F", 3},
		{"CSI H", "\x1b[H", 3},
		{"delete", "\x1b[3~", 4},
		{"ctrl+right", "\x1b[1;5C", 6},
		{"focus in", "\x1b[I", 3},
		{"focus out", "\x1b[O", 3},
		{"shift tab", "\x1b[Z", 3},
		{"SS3 A", "\x1bOA", 3},
		{"SS3 B", "\x1bOB", 3},
		{"SS3 C", "\x1bOC", 3},
		{"SS3 D", "\x1bOD", 3},
		{"SS3 F", "\x1bOF", 3},
		{"SS3 H", "\x1bOH", 3},
		{"tilde 26 passes", "\x1b[26~", 5},
		{"tilde 2000 passes", "\x1b[2000~", 7},

		// The walk stops at the first held unit.
		{"text then CR", "abc\rdef", 3},
		{"text then incomplete CSI", "ab\x1b[1", 2},
		{"pass then ctrl+v", "\x1b[A\x16x", 3},
		{"arrow then kitty enter", "\x1b[D\x1b[13u", 3},
		{"edit keys then paste start", "\x01\x1bb\x1b[200~x", 3},
		{"utf-8 then LF", "é\n", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := submitHold([]byte(c.in)); got != c.want {
				t.Errorf("submitHold(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
