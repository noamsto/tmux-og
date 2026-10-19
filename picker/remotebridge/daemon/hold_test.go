package daemon

import (
	"reflect"
	"testing"
)

func TestHoldReadArgs(t *testing.T) {
	got := holdReadArgs([]string{"%1", "%2"})
	want := []string{
		"display-message", "-p", "-t", "%1", holdHeaderFormat, ";",
		"capture-pane", "-e", "-p", "-t", "%1", ";",
		"display-message", "-p", "-t", "%2", holdHeaderFormat, ";",
		"capture-pane", "-e", "-p", "-t", "%2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holdReadArgs = %q, want %q", got, want)
	}
}

func TestHoldPollArgs(t *testing.T) {
	got := holdPollArgs([]string{"%1", "%2"})
	want := []string{
		"display-message", "-p", "-t", "%1", "#{alternate_on}|#{cursor_y}|#{cursor_x}", ";",
		"display-message", "-p", "-t", "%2", "#{alternate_on}|#{cursor_y}|#{cursor_x}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holdPollArgs = %q, want %q", got, want)
	}
}

func TestAllAlt(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
		ok   bool
	}{
		{"parked on the alternate screen", "1|0|0\n1|0|3\n", []string{"1|0|0", "1|0|3"}, true},
		{"cursor not parked", "1|5|0\n", []string{"1|0|0"}, false},
		{"parked at an earlier hold's column", "1|0|0\n", []string{"1|0|1"}, false},
		{"not on the alternate screen", "0|0|0\n", []string{"1|0|0"}, false},
		{"one pane lagging", "1|0|0\n0|0|0\n", []string{"1|0|0", "1|0|0"}, false},
		{"replies swapped between panes", "1|0|3\n1|0|0\n", []string{"1|0|0", "1|0|3"}, false},
		{"short reply", "1|0|0\n", []string{"1|0|0", "1|0|0"}, false},
		{"long reply", "1|0|0\n1|0|0\n", []string{"1|0|0"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allAlt(tc.out, tc.want); got != tc.ok {
				t.Errorf("allAlt(%q, %q) = %v, want %v", tc.out, tc.want, got, tc.ok)
			}
		})
	}
}

func TestParseHoldRead(t *testing.T) {
	locals := []string{"%10", "%11"}
	remotes := []string{"%1", "%2"}
	tests := []struct {
		name string
		out  string
		want []holdRead
	}{
		{
			name: "two panes with blank rows",
			out:  "%1|1|4|2|0|1\nab\n\n%2|1|3|3|0|1\nx\n\nz\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 2, wrap: true, rows: []string{"ab", ""}},
				{local: "%11", holdable: true, w: 3, h: 3, wrap: true, rows: []string{"x", "", "z"}},
			},
		},
		{
			name: "trailing blank row survives the trailing newline",
			out:  "%1|1|4|2|0|1\nab\n\n%2|1|3|1|0|1\n\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 2, wrap: true, rows: []string{"ab", ""}},
				{local: "%11", holdable: true, w: 3, h: 1, wrap: true, rows: []string{""}},
			},
		},
		{
			name: "no trailing newline",
			out:  "%1|1|4|1|0|1\nab",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, wrap: true, rows: []string{"ab"}},
			},
		},
		{
			name: "id mismatch on the second pane keeps the first",
			out:  "%1|1|4|1|0|1\nab\n%9|1|3|1|0|1\nx\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, wrap: true, rows: []string{"ab"}},
			},
		},
		{
			name: "short capture keeps only complete panes",
			out:  "%1|1|4|1|0|1\nab\n%2|1|3|3|0|1\nx\ny\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, wrap: true, rows: []string{"ab"}},
			},
		},
		{
			name: "four-field header stops",
			out:  "%1|1|4|1\nab\n",
		},
		{
			name: "autowrap off",
			out:  "%1|1|4|1|0|0\nab\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "five-field header stops",
			out:  "%1|1|4|1|0\nab\n",
		},
		{
			name: "seven-field header stops",
			out:  "%1|1|4|1|0|0|1\nab\n",
		},
		{
			name: "non-numeric geometry stops",
			out:  "%1|1|four|1|0|1\nab\n",
		},
		{
			name: "negative geometry stops",
			out:  "%1|1|4|-1|0|1\nab\n",
		},
		{
			name: "dead pane is not holdable",
			out:  "%1|1|4|1|1|1\nab\n",
			want: []holdRead{
				{local: "%10", w: 4, h: 1, wrap: true, rows: []string{"ab"}},
			},
		},
		{
			name: "alternate-screen option off is not holdable",
			out:  "%1|0|4|1|0|1\nab\n",
			want: []holdRead{
				{local: "%10", w: 4, h: 1, wrap: true, rows: []string{"ab"}},
			},
		},
		{name: "empty output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHoldRead(tc.out, locals, remotes)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseHoldRead = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHoldBytes(t *testing.T) {
	got := string(holdBytes([]string{"ab", "\x1b[1mc\x1b[0m", ""}, 3, 4, true))
	want := "\x1b\\" + "\x1b[?1049h" + "\x1b[?7l" + "\x1b[3;1H" +
		"\x1b[3;1H\x1b[m\x1b[2K" +
		"\x1b[2;1H\x1b[m\x1b[2K" + "\x1b[1mc\x1b[0m" +
		"\x1b[1;1H\x1b[m\x1b[2K" + "ab" +
		"\x1b[?7h" + "\x1b[1;4H"
	if got != want {
		t.Errorf("holdBytes = %q, want %q", got, want)
	}
}

// TestHoldBytesKeepsAutowrapOff: a pane whose screen had DECAWM off keeps it
// off, since tmux carries the mode across ?1049l onto the main screen.
func TestHoldBytesKeepsAutowrapOff(t *testing.T) {
	got := string(holdBytes([]string{"ab"}, 1, 1, false))
	want := "\x1b\\" + "\x1b[?1049h" + "\x1b[?7l" + "\x1b[1;1H" +
		"\x1b[1;1H\x1b[m\x1b[2K" + "ab" +
		"\x1b[1;1H"
	if got != want {
		t.Errorf("holdBytes = %q, want %q", got, want)
	}
}

// A pane already shorter than h clamps every CUP past its bottom onto the last
// row; the bottom-up order leaves that row holding its own content.
func TestHoldBytesFewerRowsThanHeight(t *testing.T) {
	got := string(holdBytes([]string{"ab"}, 3, 1, true))
	want := "\x1b\\" + "\x1b[?1049h" + "\x1b[?7l" + "\x1b[3;1H" +
		"\x1b[3;1H\x1b[m\x1b[2K" +
		"\x1b[2;1H\x1b[m\x1b[2K" +
		"\x1b[1;1H\x1b[m\x1b[2K" + "ab" +
		"\x1b[?7h" + "\x1b[1;1H"
	if got != want {
		t.Errorf("holdBytes = %q, want %q", got, want)
	}
}

func TestParkCol(t *testing.T) {
	tests := []struct {
		tok, w, want int
	}{
		{0, 80, 1},
		{1, 80, 2},
		{7, 80, 8},
		{8, 80, 1},
		{5, 3, 3},
		{5, 0, 1},
	}
	for _, tc := range tests {
		if got := parkCol(tc.tok, tc.w); got != tc.want {
			t.Errorf("parkCol(%d, %d) = %d, want %d", tc.tok, tc.w, got, tc.want)
		}
	}
}
