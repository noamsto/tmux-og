package linkjoin

import "testing"

func TestTrimTrailing(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x.io/a", "https://x.io/a"},
		{"https://x.io/a.", "https://x.io/a"},
		{"https://x.io/a?!", "https://x.io/a"},
		{"https://x.io/a]", "https://x.io/a"},
		{"https://x.io/a}", "https://x.io/a"},
		{"https://x.io/a>", "https://x.io/a"},
		{"https://x.io/a.>]},", "https://x.io/a"},
		{"https://x.io/wiki/Foo_(bar).", "https://x.io/wiki/Foo_(bar)"},
		{"https://x.io/a[0]", "https://x.io/a[0"},
		{"https://x.io/a)", "https://x.io/a)"},
		{"", ""},
		{"...", ""},
	}
	for _, tt := range tests {
		if got := TrimTrailing(tt.in); got != tt.want {
			t.Errorf("TrimTrailing(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMatchesURLShadowsPath(t *testing.T) {
	const in = "https://x.io/a/b"
	got := matches(in)
	if len(got) != 1 || got[0].kind != URL {
		t.Fatalf("matches(%q) = %+v, want one URL span", in, got)
	}
	if got[0].start != 0 || got[0].end != len(in) {
		t.Errorf("span = [%d,%d), want the whole string", got[0].start, got[0].end)
	}
}

func TestMatchesOrderedByStart(t *testing.T) {
	got := matches("edit foo/bar.go then https://x.io/a")
	if len(got) != 2 || got[0].kind != Path || got[1].kind != URL {
		t.Fatalf("matches() = %+v, want path then URL", got)
	}
}

func TestOpenable(t *testing.T) {
	tests := map[string]bool{
		"http://x.io":    true,
		"https://x.io/a": true,
		"file:///tmp/x":  true,
		"file://host/x":  false,
		"git@github.com": false,
		"git://x/y":      false,
		"ssh://x/y":      false,
		"ftp://x/y":      false,
		"foo/bar.go":     false,
	}
	for in, want := range tests {
		if got := Openable(in); got != want {
			t.Errorf("Openable(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestKindString(t *testing.T) {
	if URL.String() != "url" || Path.String() != "path" {
		t.Errorf("Kind strings = %q, %q", URL, Path)
	}
}
