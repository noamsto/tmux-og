package initcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
	"github.com/noamsto/tmux-og/generator/render"
)

// expectedLive overrides the two fields Render writes live at og init's own
// default rather than config.Defaults()'s zero value, so TestRenderIsValidConfig
// and Render itself can never independently drift on what those two values are.
func expectedLive(c *config.Config) {
	c.Tmux.Prefix = defaultPrefix
	c.Remote.AuthPersistSeconds = defaultAuthPersistSeconds
}

func TestRenderIsValidConfig(t *testing.T) {
	text, err := Render()
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(Render() output): %v\n---\n%s", err, text)
	}

	expected, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	expectedLive(expected)

	if !reflect.DeepEqual(loaded, expected) {
		t.Fatalf("loaded config =\n%+v\nwant\n%+v", loaded, expected)
	}
}

// leafPaths reflects over t's toml tags, recursing into struct-typed fields
// and treating every other type (string, bool, int, []string, *string,
// map[string]string) as a leaf. This mirrors, independently, the shape of
// config.Config that Render must document field-for-field.
func leafPaths(t reflect.Type, prefix string) []string {
	var paths []string
	for f := range t.Fields() {
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		if f.Type.Kind() == reflect.Struct {
			paths = append(paths, leafPaths(f.Type, path)...)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// decomment strips the leading "# " from every line that starts with it
// (after leading whitespace), turning a fully-commented "# key = value" line
// into a live "key = value" one. A plain "#" with no trailing space, and
// blank lines, are left untouched.
func decomment(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		lines[i] = indent + trimmed[len("# "):]
	}
	return strings.Join(lines, "\n")
}

func TestRenderCoversEveryField(t *testing.T) {
	paths := leafPaths(reflect.TypeFor[config.Config](), "")
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("leafPaths found no fields — reflection is broken")
	}

	for _, p := range paths {
		if _, ok := fieldDocs[p]; !ok {
			t.Errorf("fieldDocs has no entry for %q", p)
		}
	}

	text, err := Render()
	if err != nil {
		t.Fatal(err)
	}

	stripped := decomment(text)
	var c config.Config
	md, err := toml.Decode(stripped, &c)
	if err != nil {
		t.Fatalf("decode decommented Render() output: %v\n---\n%s", err, stripped)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		t.Errorf("decommented Render() output has undecoded key(s): %v", undecoded)
	}
	for _, p := range paths {
		if !md.IsDefined(strings.Split(p, ".")...) {
			t.Errorf("path %q is not defined in decommented Render() output", p)
		}
	}
}

// TestGenerateAcceptsInitOutput proves render.Build accepts a config decoded
// from Render()'s output. It stops short of executing config/tmux.conf.tmpl
// itself, which lives outside this Go module's source tree per
// generator/default.nix's `src = lib.cleanSource ./.` and so isn't reachable
// from a test in this package.
func TestGenerateAcceptsInitOutput(t *testing.T) {
	text, err := Render()
	if err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	p, err := paths.FromPrefix(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	render.Build(cfg, p)
}
