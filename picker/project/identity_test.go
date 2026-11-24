package project

import (
	"regexp"
	"strings"
	"testing"
)

var (
	nameCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	keyCharset  = regexp.MustCompile(`^[A-Za-z0-9._/@+:-]+$`)
)

func TestNormalizeKey(t *testing.T) {
	const want = "github.com/noamsto/tmux-og"
	tests := []struct {
		name string
		raw  string
		key  string
		ok   bool
	}{
		{"scp", "git@github.com:noamsto/tmux-og.git", want, true},
		{"scp without user", "github.com:noamsto/tmux-og.git", want, true},
		{"scp without .git", "git@github.com:noamsto/tmux-og", want, true},
		{"scp leading slash path", "git@github.com:/noamsto/tmux-og.git", want, true},
		{"https", "https://github.com/noamsto/tmux-og", want, true},
		{"https .git", "https://github.com/noamsto/tmux-og.git", want, true},
		{"http", "http://github.com/noamsto/tmux-og", want, true},
		{"trailing slash", "https://github.com/noamsto/tmux-og/", want, true},
		{"trailing slash after .git", "https://github.com/noamsto/tmux-og.git/", want, true},
		{"ssh with port and slash", "ssh://git@GitHub.com:22/noamsto/tmux-og.git/", want, true},
		{"ssh port no user", "ssh://github.com:2222/noamsto/tmux-og", want, true},
		{"git scheme", "git://github.com/noamsto/tmux-og.git", want, true},
		{"git+ssh scheme", "git+ssh://git@github.com/noamsto/tmux-og.git", want, true},
		{"ssh+git scheme", "ssh+git://git@github.com/noamsto/tmux-og.git", want, true},
		{"upper-case scheme", "HTTPS://github.com/noamsto/tmux-og", want, true},
		{"userinfo with password dropped", "https://user:tok@host.example/noamsto/tmux-og.git", "host.example/noamsto/tmux-og", true},
		{"last @ of authority wins", "https://us@er:tok@github.com/noamsto/tmux-og.git", want, true},
		{"mixed-case host", "https://GITHUB.com/noamsto/tmux-og", want, true},
		{"key form", "github.com/noamsto/tmux-og", want, true},
		{"key form upper-case host", "GitHub.com/noamsto/tmux-og", want, true},
		{"owner and repo case kept", "git@github.com:NoamSto/Tmux-OG.git", "github.com/NoamSto/Tmux-OG", true},
		{"key form dotless host, two segments", "gh-work/Org/Repo", "gh-work/Org/Repo", true},
		{"key form dotless host, nested", "gh-work/g/s/r", "gh-work/g/s/r", true},
		{"ssh host alias without dot", "git@gh-work:Org/Repo.git", "gh-work/Org/Repo", true},
		{"ssh host alias with underscore", "git@gh_work:Org/Repo.git", "gh_work/Org/Repo", true},
		{"key form underscore host", "gh_work/Org/Repo", "gh_work/Org/Repo", true},
		{"nested group path", "https://gitlab.com/group/sub/repo.git", "gitlab.com/group/sub/repo", true},
		{"dot in repo name", "https://github.com/o/r.js.git", "github.com/o/r.js", true},
		{"repo named like .gitx", "https://github.com/o/r.gitx", "github.com/o/r.gitx", true},
		{"plus and at in path", "https://github.com/o+x/r@v", "github.com/o+x/r@v", true},

		{"empty", "", "", false},
		{"too long", "https://github.com/o/" + strings.Repeat("a", 512), "", false},
		{"absolute local path", "/srv/x.git", "", false},
		{"relative parent path", "../x", "", false},
		{"relative path", "foo/bar", "", false},
		{"relative path trailing slash", "foo/bar/", "", false},
		{"relative path .git", "foo/bar.git", "", false},
		{"dot path", ".", "", false},
		{"file scheme", "file:///x", "", false},
		{"unknown scheme", "ftp://github.com/o/r", "", false},
		{"no path after authority", "https://github.com", "", false},
		{"empty path after authority", "https://github.com/", "", false},
		{"non-digit port", "ssh://git@github.com:abc/o/r", "", false},
		{"empty port", "ssh://git@github.com:/o/r", "", false},
		{"empty host", "https:///o/r", "", false},
		{"host starts with dot", "https://.github.com/o/r", "", false},
		{"host starts with dash", "https://-github.com/o/r", "", false},
		{"scp empty host", "git@:o/r", "", false},
		{"scp empty path", "git@github.com:", "", false},
		{"only .git", "git@github.com:.git", "", false},
		{"empty segment", "https://github.com/o//r", "", false},
		{"dot segment", "https://github.com/o/./r", "", false},
		{"dotdot segment", "https://github.com/o/../r", "", false},
		{"percent rejected in final key", "https://github.com/o/r%20x", "", false},
		{"tilde rejected in final key", "https://github.com/~o/r", "", false},
		{"percent in host", "https://git%68ub.com/o/r", "", false},
		{"ipv6 brackets", "ssh://[::1]/o/r", "", false},
		{"tmux format #(", "https://github.com/o/#(touch x)", "", false},
		{"tmux format #{", "https://github.com/o/#{pane_id}", "", false},
		{"hash", "https://github.com/o/r#x", "", false},
		{"single quote", "https://github.com/o/r'x", "", false},
		{"double quote", "https://github.com/o/r\"x", "", false},
		{"newline", "https://github.com/o/r\nx", "", false},
		{"trailing newline", "https://github.com/o/r\n", "", false},
		{"ESC", "https://github.com/o/r\x1bx", "", false},
		{"NUL", "https://github.com/o/r\x00", "", false},
		{"pipe", "https://github.com/o/r|x", "", false},
		{"semicolon", "https://github.com/o/r;x", "", false},
		{"command substitution", "https://github.com/o/$(id)", "", false},
		{"dollar", "https://github.com/o/$HOME", "", false},
		{"backtick", "https://github.com/o/`id`", "", false},
		{"backslash", "https://github.com/o/r\\x", "", false},
		{"space", "https://github.com/o/r x", "", false},
		{"tab", "https://github.com/o/r\tx", "", false},
		{"non-ASCII", "https://github.com/o/ré", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := NormalizeKey(tt.raw)
			if key != tt.key || ok != tt.ok {
				t.Fatalf("NormalizeKey(%q) = (%q, %v), want (%q, %v)", tt.raw, key, ok, tt.key, tt.ok)
			}
			if ok && !keyCharset.MatchString(key) {
				t.Fatalf("NormalizeKey(%q) = %q, outside the key charset", tt.raw, key)
			}
		})
	}
}

func TestNormalizeKeyIdempotent(t *testing.T) {
	for _, raw := range []string{
		"git@github.com:noamsto/tmux-og.git",
		"https://gitlab.com/group/sub/repo",
		"git@gh-work:Org/Repo.git",
		"gh-work/Org/Repo",
		"git@gh_work:Org/Repo.git",
	} {
		key, ok := NormalizeKey(raw)
		if !ok {
			t.Fatalf("NormalizeKey(%q) not ok", raw)
		}
		again, ok := NormalizeKey(key)
		if !ok || again != key {
			t.Fatalf("NormalizeKey(%q) = (%q, %v), want (%q, true)", key, again, ok, key)
		}
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct{ key, want string }{
		{"github.com/noamsto/tmux-og", "tmux-og"},
		{"gitlab.com/group/sub/repo", "repo"},
		{"host/repo", "repo"},
		{"host", "host"},
	}
	for _, tt := range tests {
		if got := RepoName(tt.key); got != tt.want {
			t.Errorf("RepoName(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestLeafName(t *testing.T) {
	a := "github.com/noamsto/tmux-og"
	b := "github.com/fork/tmux-og"
	first, second := LeafName(a), LeafName(a)
	if first != second {
		t.Fatal("LeafName is not deterministic")
	}
	if first == LeafName(b) {
		t.Fatalf("keys %q and %q share the leaf name %q", a, b, first)
	}
	for _, key := range []string{a, b, "gitlab.com/g/r.js", "gh-work/Org/Repo", "h/we!rd"} {
		leaf := LeafName(key)
		if !nameCharset.MatchString(leaf) {
			t.Errorf("LeafName(%q) = %q, outside [A-Za-z0-9_-]", key, leaf)
		}
		if !strings.HasPrefix(leaf, "og-") {
			t.Errorf("LeafName(%q) = %q, want og- prefix", key, leaf)
		}
	}
	if got := LeafName("a/b/c"); !strings.HasPrefix(got, "og-c-") || len(got) != len("og-c-")+8 {
		t.Errorf("LeafName(a/b/c) = %q, want og-c-<8 hex>", got)
	}
}

func TestSlug(t *testing.T) {
	key := "github.com/noamsto/tmux-og"
	none := func(string) bool { return false }

	if got := Slug(key, none); got != "tmux-og" {
		t.Errorf("free slug = %q, want tmux-og", got)
	}
	if got := Slug(key, func(s string) bool { return s == "tmux-og" }); got != "noamsto-tmux-og" {
		t.Errorf("collision slug = %q, want noamsto-tmux-og", got)
	}
	taken := map[string]bool{"tmux-og": true, "noamsto-tmux-og": true}
	got := Slug(key, func(s string) bool { return taken[s] })
	if !strings.HasPrefix(got, "noamsto-tmux-og-") || len(got) != len("noamsto-tmux-og-")+4 {
		t.Errorf("double collision slug = %q, want noamsto-tmux-og-<4 hex>", got)
	}
	if got := Slug(key, func(string) bool { return true }); !nameCharset.MatchString(got) {
		t.Errorf("all-taken slug = %q", got)
	}
	if got := Slug("github.com/o.x/r.js", none); got != "r_js" {
		t.Errorf("sanitized slug = %q, want r_js", got)
	}
	if got := Slug("host", func(s string) bool { return s == "host" }); got != "host-host" {
		t.Errorf("one-segment key collision slug = %q, want host-host", got)
	}
	for _, k := range []string{key, "gh-work/Org/Repo", "gitlab.com/g/s/r.js", "h/o:x/r+y"} {
		if s := Slug(k, none); !nameCharset.MatchString(s) {
			t.Errorf("Slug(%q) = %q, outside [A-Za-z0-9_-]", k, s)
		}
	}
}

func TestSafePath(t *testing.T) {
	tests := []struct {
		p  string
		ok bool
	}{
		{"/home/noams/git/tmux-og", true},
		{"/home/noams/git/.worktrees/o/r/feat-x", true},
		{"/srv/a+b/c@d:e_f-g.h", true},
		{"/a", true},
		{strings.Repeat("/a", 512), true},

		{"", false},
		{"/", false},
		{"relative/path", false},
		{"~/x", false},
		{"/a/../b", false},
		{"/..", false},
		{"/a/..", false},
		{"/a/..b", true},
		{strings.Repeat("/a", 513), false},
		{"/a b", false},
		{"/a'b", false},
		{"/a\"b", false},
		{"/a\\b", false},
		{"/a#b", false},
		{"/a$b", false},
		{"/a`b`", false},
		{"/a\nb", false},
		{"/a\tb", false},
		{"/a\x1bb", false},
		{"/a\x00b", false},
		{"/a|b", false},
		{"/a;b", false},
		{"/a(b)", false},
		{"/#(touch x)", false},
		{"/$(id)", false},
		{"/aé", false},
	}
	for _, tt := range tests {
		if got := SafePath(tt.p); got != tt.ok {
			t.Errorf("SafePath(%q) = %v, want %v", tt.p, got, tt.ok)
		}
	}
}

func TestSafeBranch(t *testing.T) {
	tests := []struct {
		b  string
		ok bool
	}{
		{"main", true},
		{"feat/1238-multi-host", true},
		{"release/v1.2.3", true},
		{"a_b-c.d", true},
		{"a.b", true},
		{strings.Repeat("a", 255), true},

		{"", false},
		{strings.Repeat("a", 256), false},
		{"HEAD", false},
		{"head", true},
		{"-x", false},
		{"/x", false},
		{"x/", false},
		{"x.", false},
		{"a..b", false},
		{"a//b", false},
		{".hidden", false},
		{"a/.hidden", false},
		{"a.lock", false},
		{"a/b.lock", false},
		{"a.lock/b", false},
		{"a.locked", true},
		{"a b", false},
		{"a~b", false},
		{"a^b", false},
		{"a:b", false},
		{"a?b", false},
		{"a*b", false},
		{"a[b", false},
		{"a\\b", false},
		{"a@{b", false},
		{"a'b", false},
		{"a\"b", false},
		{"a#b", false},
		{"a$b", false},
		{"`id`", false},
		{"a\nb", false},
		{"a\tb", false},
		{"a\x1bb", false},
		{"a\x00b", false},
		{"a|b", false},
		{"a;b", false},
		{"#(touch x)", false},
		{"#{pane_id}", false},
		{"$(id)", false},
		{"--upload-pack=x", false},
		{"aé", false},
	}
	for _, tt := range tests {
		if got := SafeBranch(tt.b); got != tt.ok {
			t.Errorf("SafeBranch(%q) = %v, want %v", tt.b, got, tt.ok)
		}
	}
}
