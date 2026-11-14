package hostcolor

import (
	"slices"
	"testing"
)

func TestParseHosts(t *testing.T) {
	if got := ParseHosts("  b   a\tb "); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("got %v", got)
	}
	if ParseHosts("  ") != nil {
		t.Fatal("empty input must yield nil")
	}
}

func TestNameEdgeCases(t *testing.T) {
	if Name([]string{"a"}, "") != "" {
		t.Fatal("empty host must yield empty name")
	}
	if got, want := Name(nil, "lab"), Palette[preferredSlot("lab")]; got != want {
		t.Fatalf("unlisted host: got %q want %q", got, want)
	}
	if got, want := Name([]string{"a"}, "lab"), Palette[preferredSlot("lab")]; got != want {
		t.Fatalf("host outside list: got %q want %q", got, want)
	}
}

func TestNameProbesCollisions(t *testing.T) {
	var a, b string
	for i := 0; i < 200 && b == ""; i++ {
		h := "h" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if a == "" {
			a = h
			continue
		}
		if preferredSlot(h) == preferredSlot(a) {
			b = h
		}
	}
	if b == "" {
		t.Fatal("no colliding pair found")
	}
	hosts := []string{b, a}
	na, nb := Name(hosts, a), Name(hosts, b)
	if na == nb {
		t.Fatalf("colliding hosts %s,%s share colour %s", a, b, na)
	}
	first := min(a, b)
	if got := Name(hosts, first); got != Palette[preferredSlot(first)] {
		t.Fatalf("first sorted host must keep its preferred slot, got %s", got)
	}
}

func TestNameDistinctWithinPalette(t *testing.T) {
	hosts := []string{"tp-g6", "mbp", "halo", "lab", "nas"}
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[Name(hosts, h)] = true
	}
	if len(seen) != len(Palette) {
		t.Fatalf("expected %d distinct colours, got %d", len(Palette), len(seen))
	}
}
