package main

import "github.com/noamsto/tmux-og/picker/ogconfig"

// Built-in defaults for a binary run without $TMUX_OG_CONFIG; the module's
// wrapper always sets it from programs.tmux-og.splash.*.

var splashTips = []tip{
	{Key: "prefix + s", Label: "Sessions"},
	{Key: "prefix + w", Label: "Windows"},
	{Key: "prefix + a", Label: "Claude windows"},
	{Key: "prefix + i", Label: "Issues / PRs"},
	{Key: "prefix + g", Label: "LazyGit"},
	{Key: "prefix + b", Label: "btop"},
	{Key: "prefix + R", Label: "Restore snapshot"},
	{Key: "prefix + u", Label: "Undo close"},
}

var splashPrefix = "`"

var splashTimeoutSec = 10

func applyConfig() error {
	cfg, ok, err := ogconfig.Load()
	if !ok {
		return err
	}
	splashTips = splashTips[:0]
	for _, t := range cfg.SplashTips {
		splashTips = append(splashTips, tip(t))
	}
	splashPrefix, splashTimeoutSec = cfg.SplashPrefix, cfg.SplashTimeout
	return nil
}
