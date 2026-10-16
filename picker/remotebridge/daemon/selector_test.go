package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestParseWindowSelector(t *testing.T) {
	tests := []struct {
		in    string
		valid bool
	}{
		{"@0", true},
		{"@12", true},
		{"@crew_branch=feat/989-x", true},
		{"@a=b:c@d+e.f_g-h", true},
		{"@" + strings.Repeat("n", 64) + "=v", true},
		{"@a=" + strings.Repeat("v", 255), true},
		{"@", false},
		{"@01", false},
		{"@1x", false},
		{"@=x", false},
		{"@a=", false},
		{"@1a=x", false},
		{"@a b=x", false},
		{"@a=b c", false},
		{"@a=$(x)", false},
		{"@a=#{pid}", false},
		{"@a=b;c", false},
		{"@a=b'c", false},
		{"@a=b,c", false},
		{"@a=b}c", false},
		{"@a=b|c", false},
		{"a=b", false},
		{"", false},
		{"@a=" + strings.Repeat("v", 256), false},
		{"@" + strings.Repeat("n", 65) + "=v", false},
		{"@a=b\nc", false},
	}
	for _, tt := range tests {
		sel, err := ParseWindowSelector(tt.in)
		if (err == nil) != tt.valid {
			t.Errorf("ParseWindowSelector(%q) err = %v, want valid=%v", tt.in, err, tt.valid)
			continue
		}
		if !tt.valid {
			if !sel.IsZero() {
				t.Errorf("ParseWindowSelector(%q) = %+v with an error, want zero", tt.in, sel)
			}
			continue
		}
		if sel.String() != tt.in || sel.IsZero() {
			t.Errorf("ParseWindowSelector(%q): String() = %q, IsZero() = %v", tt.in, sel.String(), sel.IsZero())
		}
	}
}

func TestWindowSelectorFormats(t *testing.T) {
	tests := []struct {
		in         string
		flag       string
		listFormat string
		subscribes bool
	}{
		{
			"@12",
			"#{==:#{window_id},@12}",
			"'#{window_index} #{window_id} #{window_active} #{==:#{window_id},@12} #{window_name}'",
			false,
		},
		{
			"@crew_branch=feat/989-x",
			"#{==:#{@crew_branch},feat/989-x}",
			"'#{window_index} #{window_id} #{window_active} #{==:#{@crew_branch},feat/989-x} #{window_name}'",
			true,
		},
	}
	for _, tt := range tests {
		sel, err := ParseWindowSelector(tt.in)
		if err != nil {
			t.Fatalf("ParseWindowSelector(%q): %v", tt.in, err)
		}
		if got := sel.flag(); got != tt.flag {
			t.Errorf("%q flag() = %q, want %q", tt.in, got, tt.flag)
		}
		if got := sel.listFormat(); got != tt.listFormat {
			t.Errorf("%q listFormat() = %q, want %q", tt.in, got, tt.listFormat)
		}
		if got := sel.subscribes(); got != tt.subscribes {
			t.Errorf("%q subscribes() = %v, want %v", tt.in, got, tt.subscribes)
		}
	}
}

func TestWindowFilterListCmd(t *testing.T) {
	var none *windowFilter
	if got, want := none.listCmd("my sess"), "list-windows -t 'my sess' -F "+windowListFormat; got != want {
		t.Errorf("nil listCmd = %q, want %q", got, want)
	}
	sel, _ := ParseWindowSelector("@crew_branch=x")
	got := newWindowFilter(sel).listCmd("it's")
	want := `list-windows -t 'it'\''s' -F ` + sel.listFormat()
	if got != want {
		t.Errorf("filtered listCmd = %q, want %q", got, want)
	}
}

func TestNewWindowFilterZero(t *testing.T) {
	if newWindowFilter(WindowSelector{}) != nil {
		t.Fatal("a zero selector must yield a nil filter")
	}
}

func TestParseSelectedWindowList(t *testing.T) {
	body := strings.Join([]string{
		"1 @1 0 1 shell",
		"2 @2 1 0 my window",
		"3 @3 0 x odd",
		"4 @4 0  blank",
		"5 @5 0 1",
		"6 @6 0",
		"",
	}, "\n")
	want := []remoteWindow{
		{"1", "@1", false, "shell", selYes},
		{"2", "@2", true, "my window", selNo},
		{"3", "@3", false, "odd", selUnknown},
		{"4", "@4", false, "blank", selUnknown},
		{"5", "@5", false, "", selYes},
	}
	if got := parseSelectedWindowList(body); !reflect.DeepEqual(got, want) {
		t.Errorf("parseSelectedWindowList = %+v, want %+v", got, want)
	}
}

func TestWindowFilterParse(t *testing.T) {
	var none *windowFilter
	for _, w := range none.parse("1 @1 0 shell") {
		if w.sel != selYes {
			t.Errorf("unfiltered row %+v must be selYes", w)
		}
	}
	sel, _ := ParseWindowSelector("@a=b")
	got := newWindowFilter(sel).parse("1 @1 0 0 shell")
	if len(got) != 1 || got[0].sel != selNo || got[0].name != "shell" {
		t.Errorf("filtered parse = %+v", got)
	}
}

func TestWindowFilterDue(t *testing.T) {
	opt, _ := ParseWindowSelector("@a=b")
	id, _ := ParseWindowSelector("@1")
	base := time.Unix(1000, 0)
	at := func(f *windowFilter, subscribed bool) *windowFilter {
		f.last, f.subscribed = base, subscribed
		return f
	}

	tests := []struct {
		name string
		f    *windowFilter
		now  time.Time
		want bool
	}{
		{"nil", nil, base.Add(time.Hour), false},
		{"id never due", at(newWindowFilter(id), false), base.Add(time.Hour), false},
		{"id never due even dirty", func() *windowFilter { f := at(newWindowFilter(id), false); f.note(); return f }(), base, false},
		{"unsubscribed before tick", at(newWindowFilter(opt), false), base.Add(mainLoopTickInterval - time.Nanosecond), false},
		{"unsubscribed at tick", at(newWindowFilter(opt), false), base.Add(mainLoopTickInterval), true},
		{"subscribed before backstop", at(newWindowFilter(opt), true), base.Add(windowLabelBackstopInterval - time.Nanosecond), false},
		{"subscribed at backstop", at(newWindowFilter(opt), true), base.Add(windowLabelBackstopInterval), true},
		{"dirty", func() *windowFilter { f := at(newWindowFilter(opt), true); f.note(); return f }(), base, true},
	}
	for _, tt := range tests {
		if got := tt.f.due(tt.now); got != tt.want {
			t.Errorf("%s: due = %v, want %v", tt.name, got, tt.want)
		}
	}

	f := at(newWindowFilter(opt), true)
	f.note()
	now := base.Add(time.Second)
	f.reconciled(now)
	if f.dirty || !f.last.Equal(now) || f.due(now) {
		t.Errorf("reconciled left dirty=%v last=%v due=%v", f.dirty, f.last, f.due(now))
	}
	var none *windowFilter
	none.reconciled(now)
	none.note()
}

func TestWindowFilterObserve(t *testing.T) {
	sel, _ := ParseWindowSelector("@a=b")
	tests := []struct {
		name string
		line controlmode.Line
		want bool
	}{
		{"og_select", controlmode.Line{Kind: controlmode.SubscriptionChanged, Args: []string{selectSubName, "$0", "@3", "2", "-"}, Data: []byte("@3|1")}, true},
		{"og_labels", controlmode.Line{Kind: controlmode.SubscriptionChanged, Args: []string{labelSubName, "$0", "@3", "2", "-"}, Data: []byte("@3|nova")}, false},
		{"output", controlmode.Line{Kind: controlmode.Output, Args: []string{"%1"}, Data: []byte("x")}, false},
	}
	for _, tt := range tests {
		f := newWindowFilter(sel)
		if f == nil {
			t.Fatal("newWindowFilter(@a=b) = nil")
		}
		f.observe(tt.line)
		if f.dirty != tt.want {
			t.Errorf("%s: dirty = %v, want %v", tt.name, f.dirty, tt.want)
		}
	}
	var none *windowFilter
	none.observe(tests[0].line)
}
