package main

import (
	"testing"
	"time"
)

func TestUnescape(t *testing.T) {
	got := unescape(`a\033b\134c`)
	want := "a\x1bb\\c"
	if got != want {
		t.Errorf("unescape = %q, want %q", got, want)
	}
}

func TestUnescapeTrailingLoneBackslash(t *testing.T) {
	got := unescape(`ab\`)
	want := `ab\`
	if got != want {
		t.Errorf("unescape = %q, want %q", got, want)
	}
}

func TestPercentile(t *testing.T) {
	var sorted []time.Duration
	for i := 1; i <= 100; i++ {
		sorted = append(sorted, time.Duration(i)*time.Millisecond)
	}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.5, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
		{1.0, 100 * time.Millisecond},
	}
	for _, c := range cases {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("percentile(1..100, %v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestPercentileEmpty(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil, 0.5) = %v, want 0", got)
	}
}
