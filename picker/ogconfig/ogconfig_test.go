package ogconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadUnset(t *testing.T) {
	t.Setenv(Env, "")
	if _, ok, err := Load(); ok || err != nil {
		t.Fatalf("ok=%v err=%v, want defaults kept", ok, err)
	}
}

func TestLoadReadsModuleJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	json := `{"processIcons":{"claude":"C"},"fallbackIcon":"?","maxIconsPicker":3,
"splashTips":[{"key":"prefix + s","label":"Sessions"}],"splashPrefix":"a","splashTimeout":0}`
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(Env, path)
	cfg, ok, err := Load()
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := Config{
		ProcessIcons:   map[string]string{"claude": "C"},
		FallbackIcon:   "?",
		MaxIconsPicker: 3,
		SplashTips:     []Tip{{Key: "prefix + s", Label: "Sessions"}},
		SplashPrefix:   "a",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadFailsLoudly(t *testing.T) {
	t.Setenv(Env, filepath.Join(t.TempDir(), "missing.json"))
	if _, ok, err := Load(); ok || err == nil {
		t.Fatalf("ok=%v err=%v, want an error for a missing file", ok, err)
	}
}
