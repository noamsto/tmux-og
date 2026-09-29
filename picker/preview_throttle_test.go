package main

import (
	"sync"
	"testing"
	"time"
)

func TestPreviewThrottleFirstRequestRunsAtOnce(t *testing.T) {
	g := &previewThrottle{}
	ticket := g.request()
	start := time.Now()
	if !g.admit(ticket) {
		t.Fatal("a lone request was not admitted")
	}
	if d := time.Since(start); d > previewMinGap/2 {
		t.Errorf("a lone request waited %v; a single cursor move must preview at once", d)
	}
}

func TestPreviewThrottleKeepsOnlyTheNewestOfABurst(t *testing.T) {
	g := &previewThrottle{}
	// A capture ran just now, so the burst below lands inside the gap.
	g.admit(g.request())

	const n = 6
	tickets := make([]uint64, n)
	for i := range tickets {
		tickets[i] = g.request() // Update issues these in key order
	}
	admitted := make([]bool, n)
	var wg sync.WaitGroup
	for i := range tickets {
		wg.Go(func() {
			admitted[i] = g.admit(tickets[i])
		})
	}
	wg.Wait()
	for i, ok := range admitted {
		if want := i == n-1; ok != want {
			t.Errorf("request %d admitted = %v, want %v (only the newest may run)", i, ok, want)
		}
	}
}

func TestPreviewThrottleSpacesCaptures(t *testing.T) {
	g := &previewThrottle{}
	g.admit(g.request())
	start := time.Now()
	if !g.admit(g.request()) {
		t.Fatal("the newest request was dropped")
	}
	if d := time.Since(start); d < previewMinGap-5*time.Millisecond {
		t.Errorf("second capture started after %v, want the %v gap kept", d, previewMinGap)
	}
}

// TestLoadPreviewCoalescesAHeldKey drives the real loadPreviewCmd: cursor moves
// that all land before any capture runs capture the newest row only.
func TestLoadPreviewCoalescesAHeldKey(t *testing.T) {
	var mu sync.Mutex
	var captured []string
	orig := previewCapture
	previewCapture = func(target string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, target)
		return []byte("x"), nil
	}
	t.Cleanup(func() { previewCapture = orig })
	*previewGate = previewThrottle{}

	m := newPickerModel(false, false, false, map[string]string{}, "dark",
		[]listItem{{target: "a:0"}, {target: "b:0"}, {target: "c:0"}, {target: "d:0"}, {target: "e:0"}}, "")
	m.showPreview = true
	m.cursor = m.firstSelectable(0)

	var cmds []func() any
	for range 5 {
		cmd := m.loadPreviewCmd()
		if cmd == nil {
			t.Fatal("loadPreviewCmd returned no command")
		}
		cmds = append(cmds, func() any { return cmd() })
		m = m.moveCursor(1)
	}
	var wg sync.WaitGroup
	for _, run := range cmds {
		wg.Go(func() {
			run()
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 || captured[0] != "e:0" {
		t.Errorf("captured %v, want only the newest row [e:0]", captured)
	}
}
