package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// completeToml is what the Nix serializer emits: every required key, no optionals.
func completeToml() string {
	var b strings.Builder
	b.WriteString("bash = \"/store/bash/bin/bash\"\n\n[scripts]\n")
	for _, n := range RequiredScripts {
		fmt.Fprintf(&b, "%q = \"/store/%s/bin/%s\"\n", n, n, n)
	}
	b.WriteString("\n[bin]\n")
	for _, n := range RequiredBin {
		fmt.Fprintf(&b, "%q = \"/store/go-tools/bin/%s\"\n", n, n)
	}
	b.WriteString("\n[plugins]\n")
	for n, entry := range pluginEntries {
		fmt.Fprintf(&b, "%q = \"/store/%s/share/tmux-plugins/%s/%s\"\n", n, n, n, entry)
	}
	return b.String()
}

func writeToml(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "paths.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadComplete(t *testing.T) {
	p, err := Load(writeToml(t, completeToml()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Bash != "/store/bash/bin/bash" {
		t.Errorf("bash = %q", p.Bash)
	}
	if len(p.Scripts) != 26 {
		t.Errorf("scripts = %d keys, want 26", len(p.Scripts))
	}
	if got := p.Plugins["catppuccin"]; !strings.HasSuffix(got, "/catppuccin.tmux") {
		t.Errorf("plugins.catppuccin = %q", got)
	}
	for name, target := range optionalsOf(p) {
		if target != nil {
			t.Errorf("%s = %q, want absent", name, *target)
		}
	}
}

func TestLoadOptionalsPresent(t *testing.T) {
	// Top-level keys must precede the first table header, or TOML nests them.
	body := `persist_wire_script = "/store/wire/tmux-remux-wire"
carousel_toggle = "/store/aeye/bin/tmux-claude-images"
carousel_aeye = "/store/aeye/bin/aeye"
prdash = "/store/prdash/bin/prdash"
` + completeToml()
	p, err := Load(writeToml(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range optionalsOf(p) {
		if target == nil {
			t.Errorf("%s absent, want present", name)
		}
	}
}

func TestLoadMissingRequired(t *testing.T) {
	body := completeToml()
	tests := []struct {
		name string
		drop string
		want string
	}{
		{"script", "\"tmux-pr-enrich\" = \"/store/tmux-pr-enrich/bin/tmux-pr-enrich\"\n", "scripts.tmux-pr-enrich"},
		{"binary", "\"tmux-statusline\" = \"/store/go-tools/bin/tmux-statusline\"\n", "bin.tmux-statusline"},
		{"bash", "bash = \"/store/bash/bin/bash\"\n", "bash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trimmed := strings.Replace(body, tt.drop, "", 1)
			if trimmed == body {
				t.Fatalf("fixture did not contain %q", tt.drop)
			}
			_, err := Load(writeToml(t, trimmed))
			if err == nil {
				t.Fatalf("want an error naming %s, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), "missing required key(s): "+tt.want) {
				t.Fatalf("error = %q, want it to name %s", err, tt.want)
			}
		})
	}
}

func TestLoadMissingPluginNamesAllAtOnce(t *testing.T) {
	_, err := Load(writeToml(t, "bash = \"/b\"\n"))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"scripts.tmux-update-icons", "bin.tmux-splash", "plugins.fingers"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %s: %s", want, msg)
		}
	}
}

// An empty value is as unusable as an absent key, so it fails the same way.
func TestEmptyValueCountsAsMissing(t *testing.T) {
	body := strings.Replace(completeToml(), "\"/store/bash/bin/bash\"", "\"\"", 1)
	_, err := Load(writeToml(t, body))
	if err == nil || !strings.Contains(err.Error(), "bash") {
		t.Fatalf("error = %v, want it to name bash", err)
	}
}

func TestFromPrefix(t *testing.T) {
	dir := t.TempDir()
	p, err := FromPrefix(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "bin", "bash"); p.Bash != want {
		t.Errorf("bash = %q, want %q", p.Bash, want)
	}
	if want := filepath.Join(dir, "bin", "tmux-reflow-windows"); p.Scripts["tmux-reflow-windows"] != want {
		t.Errorf("scripts.tmux-reflow-windows = %q, want %q", p.Scripts["tmux-reflow-windows"], want)
	}
	if want := filepath.Join(dir, "bin", "tmux-statusline"); p.Bin["tmux-statusline"] != want {
		t.Errorf("bin.tmux-statusline = %q, want %q", p.Bin["tmux-statusline"], want)
	}
	want := filepath.Join(dir, "share", "tmux-og", "plugins", "tmux-fzf", "main.tmux")
	if p.Plugins["tmux-fzf"] != want {
		t.Errorf("plugins.tmux-fzf = %q, want %q", p.Plugins["tmux-fzf"], want)
	}
	for name, target := range optionalsOf(p) {
		if target != nil {
			t.Errorf("%s = %q, want absent under an empty prefix", name, *target)
		}
	}
}

func TestFromPrefixOptionalsProbeBin(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"prdash", "aeye"} {
		if err := os.WriteFile(filepath.Join(bin, n), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := FromPrefix(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Prdash == nil || *p.Prdash != filepath.Join(bin, "prdash") {
		t.Errorf("prdash = %v, want the probed path", p.Prdash)
	}
	if p.CarouselAeye == nil {
		t.Error("carousel_aeye absent, want the probed path")
	}
	if p.CarouselToggle != nil {
		t.Errorf("carousel_toggle = %q, want absent", *p.CarouselToggle)
	}
	if p.PersistWireScript != nil {
		t.Errorf("persist_wire_script = %q, want absent", *p.PersistWireScript)
	}
}

func optionalsOf(p *Paths) map[string]*string {
	return map[string]*string{
		"persist_wire_script": p.PersistWireScript,
		"carousel_toggle":     p.CarouselToggle,
		"carousel_aeye":       p.CarouselAeye,
		"prdash":              p.Prdash,
	}
}

// A probe error that is not "absent" must surface: silently treating it as a
// disabled feature drops the feature the path names.
func TestFromPrefixOptionalProbeError(t *testing.T) {
	dir := t.TempDir()
	// bin as a regular file makes every probe under it fail ENOTDIR, which is
	// not os.IsNotExist and does not depend on the test user's privileges.
	if err := os.WriteFile(filepath.Join(dir, "bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := FromPrefix(dir)
	if err == nil {
		t.Fatalf("FromPrefix = %v, want a probe error", p)
	}
	var perr *fs.PathError
	if !errors.As(err, &perr) {
		t.Errorf("err = %v, want it to wrap a *fs.PathError", err)
	}
}
