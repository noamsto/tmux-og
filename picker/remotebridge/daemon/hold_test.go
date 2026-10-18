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
		"display-message", "-p", "-t", "%1", "#{alternate_on}", ";",
		"display-message", "-p", "-t", "%2", "#{alternate_on}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holdPollArgs = %q, want %q", got, want)
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
			out:  "%1|0|1|4|2|1|0|0\nab\n\n%2|0|1|3|3|2|1|0\nx\n\nz\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 2, cx: 1, cy: 0, rows: []string{"ab", ""}},
				{local: "%11", holdable: true, w: 3, h: 3, cx: 2, cy: 1, rows: []string{"x", "", "z"}},
			},
		},
		{
			name: "trailing blank row survives the trailing newline",
			out:  "%1|0|1|4|2|0|0|0\nab\n\n%2|0|1|3|1|0|0|0\n\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 2, rows: []string{"ab", ""}},
				{local: "%11", holdable: true, w: 3, h: 1, rows: []string{""}},
			},
		},
		{
			name: "no trailing newline",
			out:  "%1|0|1|4|1|0|0|0\nab",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "id mismatch on the second pane keeps the first",
			out:  "%1|0|1|4|1|0|0|0\nab\n%9|0|1|3|1|0|0|0\nx\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "short capture keeps only complete panes",
			out:  "%1|0|1|4|1|0|0|0\nab\n%2|0|1|3|3|0|0|0\nx\ny\n",
			want: []holdRead{
				{local: "%10", holdable: true, w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "seven-field header stops",
			out:  "%1|0|1|4|1|0|0\nab\n",
		},
		{
			name: "non-numeric geometry stops",
			out:  "%1|0|1|four|1|0|0|0\nab\n",
		},
		{
			name: "negative geometry stops",
			out:  "%1|0|1|4|1|-1|0|0\nab\n",
		},
		{
			name: "already on the alternate screen is not holdable",
			out:  "%1|1|1|4|1|0|0|0\nab\n",
			want: []holdRead{
				{local: "%10", w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "dead pane is not holdable",
			out:  "%1|0|1|4|1|0|0|1\nab\n",
			want: []holdRead{
				{local: "%10", w: 4, h: 1, rows: []string{"ab"}},
			},
		},
		{
			name: "alternate-screen option off is not holdable",
			out:  "%1|0|0|4|1|0|0|0\nab\n",
			want: []holdRead{
				{local: "%10", w: 4, h: 1, rows: []string{"ab"}},
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
	got := string(holdBytes([]string{"ab", "\x1b[1mc\x1b[0m"}, 3, 1))
	want := "\x1b\\" + "\x1b[?1049h" +
		"\x1b[1;1H" + "ab" +
		"\x1b[2;1H" + "\x1b[1mc\x1b[0m" +
		"\x1b[2;4H"
	if got != want {
		t.Errorf("holdBytes = %q, want %q", got, want)
	}
}
