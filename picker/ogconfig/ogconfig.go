// Package ogconfig reads the user settings the Nix module writes as JSON.
// Keeping them out of the Go source keeps the binaries identical across
// configs, so they come from the binary cache instead of building per user.
package ogconfig

import (
	"encoding/json"
	"fmt"
	"os"
)

// Env names the JSON file; the module's wrappers set it.
const Env = "TMUX_OG_CONFIG"

type Tip struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type Config struct {
	ProcessIcons   map[string]string `json:"processIcons"`
	FallbackIcon   string            `json:"fallbackIcon"`
	MaxIconsPicker int               `json:"maxIconsPicker"`
	SplashTips     []Tip             `json:"splashTips"`
	SplashPrefix   string            `json:"splashPrefix"`
	SplashTimeout  int               `json:"splashTimeout"`
}

// Load returns the config named by Env, or ok=false when Env is unset and the
// caller keeps its built-in defaults.
func Load() (cfg Config, ok bool, err error) {
	path := os.Getenv(Env)
	if path == "" {
		return Config{}, false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the module's store file, set by its wrapper
	if err != nil {
		return Config{}, false, fmt.Errorf("%s: %w", Env, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("%s %s: %w", Env, path, err)
	}
	return cfg, true, nil
}
