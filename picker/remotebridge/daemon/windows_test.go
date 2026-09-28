package daemon

import (
	"reflect"
	"testing"
)

func TestRegistryLookup(t *testing.T) {
	r := newRegistry()
	w := r.add("@5", "@101")
	w.remotePanes = []string{"%3", "%4"}
	if got, ok := r.byRemoteID("@5"); !ok || got.localWin != "@101" {
		t.Fatalf("byRemoteID(@5) = %+v %v", got, ok)
	}
	if _, ok := r.byRemoteID("@99"); ok {
		t.Fatal("byRemoteID(@99) should be false")
	}
	if r.empty() {
		t.Fatal("registry with one window must not be empty")
	}
	if _, ok := r.remove("@5"); !ok || !r.empty() {
		t.Fatal("remove(@5) then empty() should be true")
	}
}

func TestParseWindowList(t *testing.T) {
	// index and id are distinct namespaces: window at index 3 has id @5.
	// window_active sits before the name, which is the only free-form field.
	// A name may contain spaces; a `|` is preserved here (sanitized at write time).
	got := parseWindowList("1 @1 0 shell\n2 @2 1 my window\n3 @5 0 a|b\n4 @7 0\n")
	want := []remoteWindow{
		{"1", "@1", false, "shell"},
		{"2", "@2", true, "my window"},
		{"3", "@5", false, "a|b"},
		{"4", "@7", false, ""}, // no name field -> empty
	}
	if len(got) != len(want) {
		t.Fatalf("parseWindowList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseWindowList[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(parseWindowList("  \n\n")) != 0 {
		t.Fatal("blank body must yield no windows")
	}
}

// TestInitialWindowSelectsByIndexNotID pins the blocking fix: --window carries a
// window INDEX, not a window id. A remote session where index 2 has id @7 must
// select the local window mirroring @7 — never "@2" (which here is a different
// window at index 1).
func TestInitialWindowSelectsByIndexNotID(t *testing.T) {
	wins := []remoteWindow{{"1", "@2", false, ""}, {"2", "@7", false, ""}} // index 1 -> @2, index 2 -> @7
	reg := newRegistry()
	reg.add("@2", "@101")
	reg.add("@7", "@102")
	if got, ok := localWinForRemoteIndex(wins, reg, "2"); !ok || got != "@102" {
		t.Fatalf("--window 2 selected %q ok=%v, want @102 (mirror of @7, not @2)", got, ok)
	}
	if got, ok := localWinForRemoteIndex(wins, reg, "1"); !ok || got != "@101" {
		t.Fatalf("--window 1 selected %q ok=%v, want @101", got, ok)
	}
	if _, ok := localWinForRemoteIndex(wins, reg, "9"); ok {
		t.Fatal("out-of-range index must not select")
	}
}

func TestSanitizeWindowName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"shell", "shell"},
		{"my window", "my window"}, // spaces preserved
		{"a|b", "ab"},              // FMT delimiter stripped
		{"a\nb\r", "ab"},           // newlines stripped
		{"tab\tend", "tabend"},     // control char stripped
		// Observed remote name: style sequences stripped, bare # escaped.
		{"[nix-amd-ai 🧠 #[fg=#94e2d5]󰪣#[fg=default] 󰘭 #46]", "[nix-amd-ai 🧠 󰪣 󰘭 ##46]"},
		{"PR #46", "PR ##46"},                       // bare # with no style sequence
		{"#[fg=red]PR #46#[fg=default]", "PR ##46"}, // styles + bare #
	}
	for _, tc := range cases {
		if got := sanitizeWindowName(tc.in); got != tc.want {
			t.Errorf("sanitizeWindowName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// nameFixtures are raw remote names r with their measured sanitize (E) and
// strip (S) images. Deliberately raw inputs rather than "the image of E": the
// round trip that matters starts at whatever the remote reports. Rows 4-8 are
// the '#'/'['/'|' adjacency cases that drift without the drop-before-strip
// order and the fixed-point iteration.
var nameFixtures = []struct {
	r, e, s string
}{
	{"pr#367", "pr##367", "pr#367"},
	{"a##b", "a####b", "a##b"},
	{"plain-name", "plain-name", "plain-name"},
	{"x#[fg=red", "x", "x"},
	{"a##[x]b", "a##b", "a#b"},
	{"#|[x]", "", ""},
	{"a#|[x]b", "ab", "ab"},
	{"##[a][", "", ""},
	{"a|b\nc", "abc", "abc"},
	{"|||", "", ""},
	{"it's", "it's", "it's"},
	{"~/src", "~/src", "~/src"},
	{"[nix-amd-ai 🧠 #[fg=#94e2d5]󰪣#[fg=default] 󰘭 #46]", "[nix-amd-ai 🧠 󰪣 󰘭 ##46]", "[nix-amd-ai 🧠 󰪣 󰘭 #46]"},
}

// TestWindowNameFixtures pins E and S against the measured table. The
// tmux-level inverse (rename-window of E(r) yielding S(r)) hardcodes these same
// strings, so pinning them here in Go is what keeps that pair from being
// tautological.
func TestWindowNameFixtures(t *testing.T) {
	for _, f := range nameFixtures {
		if got := sanitizeWindowName(f.r); got != f.e {
			t.Errorf("sanitizeWindowName(%q) = %q, want %q", f.r, got, f.e)
		}
		if got := stripWindowName(f.r); got != f.s {
			t.Errorf("stripWindowName(%q) = %q, want %q", f.r, got, f.s)
		}
	}
}

// TestWindowNameRoundTrip is the property the prompt prefill depends on: a name
// read back out of @window_bridge_name and re-sanitized must land where it
// started. Without the decode, re-escaping doubles every '#' each pass — which
// is what any escape applied twice does, not anything specific to '#['.
func TestWindowNameRoundTrip(t *testing.T) {
	for _, f := range nameFixtures {
		e := sanitizeWindowName(f.r)
		if got := sanitizeWindowName(decodeWindowName(e)); got != e {
			t.Errorf("E(D(E(%q))) = %q, want %q", f.r, got, e)
		}
	}
}

// TestSanitizedNameHasNoHashBeforeBracket is the invariant behind the round
// trip: format_expand collapses '##' pairwise but passes a '#'-run followed by
// '[' through verbatim, so such a run must never survive sanitizing.
func TestSanitizedNameHasNoHashBeforeBracket(t *testing.T) {
	for _, f := range nameFixtures {
		e := sanitizeWindowName(f.r)
		for i := 0; i < len(e); i++ {
			if e[i] != '#' {
				continue
			}
			j := i
			for j < len(e) && e[j] == '#' {
				j++
			}
			if j < len(e) && e[j] == '[' {
				t.Errorf("sanitizeWindowName(%q) = %q has a #-run before '[' at %d", f.r, e, i)
			}
			i = j - 1
		}
	}
}

func TestStripWindowNameIdempotent(t *testing.T) {
	for _, f := range nameFixtures {
		s := stripWindowName(f.r)
		if got := stripWindowName(s); got != s {
			t.Errorf("stripWindowName(stripWindowName(%q)) = %q, want %q", f.r, got, s)
		}
	}
}

func TestDecodeWindowName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"pr##367", "pr#367"},
		{"a####b", "a##b"},
		{"###", "##"}, // odd run: ceil(n/2), same either scan direction
		{"#", "#"},    // a lone # is not an escape
		{"", ""},
	}
	for _, tc := range cases {
		if got := decodeWindowName(tc.in); got != tc.want {
			t.Errorf("decodeWindowName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestActiveFirst(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	reg.add("@3", "@103")
	ids := []string{"@1", "@2", "@3"}

	got := activeFirst(reg, "@102", ids)
	if !reflect.DeepEqual(got, []string{"@2", "@1", "@3"}) {
		t.Fatalf("activeFirst = %v, want @2 first, rest in order", got)
	}
	// Already-first, unknown window, and "" all leave the slice untouched.
	if got := activeFirst(reg, "@101", ids); !reflect.DeepEqual(got, ids) {
		t.Fatalf("activeFirst(active already first) = %v, want %v", got, ids)
	}
	if got := activeFirst(reg, "@999", ids); !reflect.DeepEqual(got, ids) {
		t.Fatalf("activeFirst(unknown) = %v, want %v", got, ids)
	}
	if got := activeFirst(reg, "", ids); !reflect.DeepEqual(got, ids) {
		t.Fatalf("activeFirst(\"\") = %v, want %v", got, ids)
	}
}

// A local drag reshapes the window behind the reconcile's back, so the dedup
// key must go stale unless it already equals what the drag sent (the
// %layout-change for it already applied this settle round, or nothing
// actually moved).
func TestNoteLocalLayout(t *testing.T) {
	tests := []struct {
		layout, sent, want string
	}{
		{"A", "", "A"},
		{"A", "A", "A"},
		{"A", "B", ""},
		{"", "B", ""},
	}
	for _, tc := range tests {
		w := &mirrorWindow{layout: tc.layout}
		w.noteLocalLayout(tc.sent)
		if w.layout != tc.want {
			t.Errorf("layout=%q noteLocalLayout(%q) = %q, want %q", tc.layout, tc.sent, w.layout, tc.want)
		}
	}
}
