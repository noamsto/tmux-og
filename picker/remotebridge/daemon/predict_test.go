package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestParsePredictMode(t *testing.T) {
	tests := []struct {
		in      string
		want    PredictMode
		wantErr bool
	}{
		{"", PredictAuto, false},
		{"auto", PredictAuto, false},
		{"on", PredictOn, false},
		{"off", PredictOff, false},
		{"yes", PredictAuto, true},
	}
	for _, tt := range tests {
		got, err := ParsePredictMode(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePredictMode(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), `"yes"`) {
			t.Errorf("ParsePredictMode(%q) err = %q, want it to quote the input", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParsePredictMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestPredictSharedOff(t *testing.T) {
	if s := newPredictShared(PredictOff); s != nil {
		t.Fatalf("newPredictShared(PredictOff) = %v, want nil", s)
	}
}

func TestPredictSharedNilReceiver(t *testing.T) {
	var s *predictShared
	s.sample(time.Second / 2)
	if s.want() {
		t.Error("nil predictShared wants prediction")
	}
	if got := s.estimate(); got != 0 {
		t.Errorf("nil estimate = %v, want 0", got)
	}
}

func TestPredictSharedOnAlwaysWanted(t *testing.T) {
	s := newPredictShared(PredictOn)
	if !s.want() {
		t.Error("PredictOn not wanted before any sample")
	}
	for range predictSampleWindow {
		s.sample(time.Millisecond)
	}
	if !s.want() {
		t.Error("PredictOn not wanted at low latency")
	}
}

func TestPredictSharedSample(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	repeat := func(d time.Duration, n int) []time.Duration {
		out := make([]time.Duration, n)
		for i := range out {
			out[i] = d
		}
		return out
	}
	tests := []struct {
		name     string
		samples  []time.Duration
		wantWant bool
		wantEst  time.Duration
	}{
		{"starts off", nil, false, 0},
		{"over max discarded", []time.Duration{2 * time.Second}, false, 0},
		{"zero and negative discarded", []time.Duration{0, -ms(5)}, false, 0},
		{"exactly max kept", []time.Duration{predictSampleMax}, true, predictSampleMax},
		{"eight slow samples turn on", repeat(ms(50), 8), true, ms(50)},
		{"single outlier amid fast never turns on", append(repeat(ms(2), 7), ms(900)), false, ms(2)},
		{"drops below hide threshold", append(repeat(ms(50), 8), repeat(ms(5), 8)...), false, ms(5)},
		{"inside the band keeps on", append(repeat(ms(50), 8), repeat(ms(25), 8)...), true, ms(25)},
		{"inside the band keeps off", append(repeat(ms(5), 8), repeat(ms(25), 8)...), false, ms(25)},
		{"even count uses lower middle", []time.Duration{ms(10), ms(50)}, false, ms(10)},
		{"window forgets old samples", append(repeat(ms(900), 8), repeat(ms(1), 8)...), false, ms(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newPredictShared(PredictAuto)
			for _, d := range tt.samples {
				s.sample(d)
			}
			if got := s.want(); got != tt.wantWant {
				t.Errorf("want() = %v, want %v", got, tt.wantWant)
			}
			if got := s.estimate(); got != tt.wantEst {
				t.Errorf("estimate() = %v, want %v", got, tt.wantEst)
			}
		})
	}
}

func TestPredictSharedConcurrentSample(t *testing.T) {
	s := newPredictShared(PredictAuto)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				s.sample(40 * time.Millisecond)
				s.want()
				s.estimate()
			}
		})
	}
	wg.Wait()
	if !s.want() {
		t.Error("want() = false after concurrent slow samples")
	}
}

func TestPredictModesCmd(t *testing.T) {
	want := "display-message -p -t %7 -F '#{cursor_flag}|#{insert_flag}|#{origin_flag}|#{wrap_flag}|#{scroll_region_upper}|#{scroll_region_lower}'"
	if got := predictModesCmd("%7"); got != want {
		t.Errorf("predictModesCmd = %q, want %q", got, want)
	}
}

func TestParsePredictModes(t *testing.T) {
	reply := func(s string) controlmode.Line { return controlmode.Line{Kind: controlmode.End, Data: []byte(s)} }
	tests := []struct {
		name   string
		line   controlmode.Line
		ok     bool
		want   predictModes
		wantOK bool
	}{
		{
			name:   "valid",
			line:   reply("1|0|0|1|0|23"),
			ok:     true,
			want:   predictModes{cursorVisible: true, wrap: true, top: 0, bottom: 23},
			wantOK: true,
		},
		{
			name:   "all flags set with region and trailing newline",
			line:   reply("1|1|1|1|3|10\n"),
			ok:     true,
			want:   predictModes{cursorVisible: true, insert: true, origin: true, wrap: true, top: 3, bottom: 10},
			wantOK: true,
		},
		{name: "missing field", line: reply("1|0|0|1|0"), ok: true},
		{name: "extra field", line: reply("1|0|0|1|0|23|9"), ok: true},
		{name: "empty reply", line: reply(""), ok: true},
		{name: "flag 2", line: reply("2|0|0|1|0|23"), ok: true},
		{name: "empty flag", line: reply("1||0|1|0|23"), ok: true},
		{name: "top below bottom violated", line: reply("1|0|0|1|10|3"), ok: true},
		{name: "non-numeric region", line: reply("1|0|0|1|x|23"), ok: true},
		{name: "negative region", line: reply("1|0|0|1|-1|23"), ok: true},
		{name: "error line", line: controlmode.Line{Kind: controlmode.Error, Data: []byte("1|0|0|1|0|23")}, ok: true},
		{name: "dead stream", line: reply("1|0|0|1|0|23"), ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePredictModes(tt.line, tt.ok)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("modes = %+v, want %+v", got, tt.want)
			}
		})
	}
}
