package graphics

import (
	"sync"
	"testing"
)

func TestRelayStringGrammar(t *testing.T) {
	if got := NewRelayFromClient(true, "sixel").String(); got != "sixel" {
		t.Fatalf("String() = %q, want %q", got, "sixel")
	}
	if got := NewRelayFromClient(false, "bpaste").String(); got != "" {
		t.Fatalf("String() = %q, want empty", got)
	}
	if zero := (Relay{}); zero.String() != "" || zero.Sixel() {
		t.Fatal("zero value must mean no relayable capability")
	}
}

func TestRelaySourceNilReceiverIsRelayOff(t *testing.T) {
	var s *RelaySource
	if got := s.Load(); got.Sixel() {
		t.Fatal("nil *RelaySource must Load() the zero (relay-off) Relay")
	}
}

func TestRelaySourceZeroValueIsRelayOff(t *testing.T) {
	var s RelaySource
	if got := s.Load(); got.Sixel() {
		t.Fatal("zero-value RelaySource must Load() the zero (relay-off) Relay")
	}
}

func TestRelaySourceStoreLoadRoundTripsRaw(t *testing.T) {
	s := NewRelaySource(NewRelayFromClient(false, "bpaste"))

	want := NewRelayFromClient(true, "bpaste,focus,sixel,title")
	s.Store(want)

	got := s.Load()
	if got.Sixel() != want.Sixel() {
		t.Fatalf("Sixel() = %v, want %v", got.Sixel(), want.Sixel())
	}
	if got.raw != want.raw {
		t.Fatalf("raw = %q, want %q — raw must round-trip for Proxy.Filter's drop diagnostic", got.raw, want.raw)
	}
}

func TestRelaySourceConcurrentLoadStore(t *testing.T) {
	s := NewRelaySource(Relay{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			feats := "bpaste"
			sixel := false
			if i%2 == 0 {
				feats = "bpaste,sixel"
				sixel = true
			}
			s.Store(NewRelayFromClient(sixel, feats))
			_ = s.Load()
		}(i)
	}
	wg.Wait()
}
