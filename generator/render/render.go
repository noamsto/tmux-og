// Package render executes config/tmux.conf.tmpl against the decoded config and
// resolved paths.
package render

import (
	"bytes"
	"fmt"
	"os"
	"text/template"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
)

// Data is every name the template may reference. Derived values arrive one
// group at a time as the template grows; the raw config and paths are always
// reachable, so a lift only adds what needs computing rather than proxying.
type Data struct {
	Config *config.Config
	Paths  *paths.Paths

	// CopyCommand is what tmux pipes a copy-mode selection into. Chosen from
	// the config's platform key, never runtime.GOOS: under cross-compilation
	// the generator runs on the build machine, not the one tmux will run on.
	CopyCommand string

	// DefaultShellConfig and TerminalConfig are interpolated mid-line and so
	// carry their own trailing indentation; see their builders in base.go.
	DefaultShellConfig string
	TerminalConfig     string
	PluginConfigs      string
	PluginRunShells    string
	FocusFollowsMouse  string

	BridgeGate string
	BridgeCtl  string

	// CarouselBind and PrdashBind are empty when their package is not wired
	// in, and carry no trailing newline; see carouselBind for the contract.
	CarouselBind   string
	PrdashBind     string
	LazygitBind    string
	BtopBind       string
	K9sBind        string
	YaziBind       string
	EnrichCardBind string

	Icons pickerIcons

	AINamingFlag       string
	ResumeClaudeFlag   string
	ResumeCarouselFlag string

	// EnrichIconsDoubled is the tmux-format dialect; the card's raw one lives
	// inside EnrichCardBind. Keeping both reachable at once is the point.
	EnrichIconsDoubled map[string]string
	BridgeOpt          map[string]string

	// AgentUsageArgs is spliced mid-line into status-format[0] and carries its
	// own leading space; TickHookIfShell is one line, no trailing newline.
	AgentUsageArgs  string
	TickHookIfShell string

	// CarouselHooks and PersistBlock are whole-block values: body at column 0
	// with one trailing newline, or "" when their package is not wired in.
	// PersistBlock alone opens with a blank line of its own (I7).
	CarouselHooks string
	PersistBlock  string

	// MenuBinds is the %if-gated menu block, no trailing newline.
	MenuBinds string

	// DragBinds is the %if-gated mirror float border drag block, no trailing
	// newline; see dragBinds' doc comment for why it exists.
	DragBinds string
}

// Build derives the template data.
func Build(cfg *config.Config, p *paths.Paths) Data {
	copyCommand := "wl-copy"
	if cfg.Platform == "darwin" {
		copyCommand = "pbcopy"
	}
	return Data{
		Config:             cfg,
		Paths:              p,
		CopyCommand:        copyCommand,
		DefaultShellConfig: defaultShellConfig(cfg.Tmux.DefaultShell),
		TerminalConfig:     terminalConfig(cfg.Tmux.TerminalTerm, cfg.Tmux.SixelTerminals),
		PluginConfigs:      pluginConfigs,
		PluginRunShells:    pluginRunShells(p),
		FocusFollowsMouse:  onOff(cfg.Tmux.FocusFollowsMouse),
		BridgeGate:         bridgeGate,
		BridgeCtl:          bridgeCtl(p),
		CarouselBind:       carouselBind(p),
		PrdashBind:         prdashBind(p),
		LazygitBind:        lazygitBind(p),
		BtopBind:           btopBind(p),
		K9sBind:            k9sBind(p),
		YaziBind:           yaziBind(p),
		EnrichCardBind:     enrichCardBind(cfg, p),
		Icons:              icons,
		AINamingFlag:       oneZero(cfg.AINaming.Enable),
		ResumeClaudeFlag:   onOff(cfg.Resume.Claude),
		ResumeCarouselFlag: onOff(cfg.Resume.Carousel),
		EnrichIconsDoubled: enrichIconsDoubled(cfg),
		BridgeOpt:          bridgeOpts(),
		AgentUsageArgs:     agentUsageArgs(cfg),
		TickHookIfShell:    tickHookIfShell(cfg, p),
		CarouselHooks:      carouselHooks(p),
		PersistBlock:       persistBlock(p),
		MenuBinds:          menuBinds(p),
		DragBinds:          dragBinds(p),
	}
}

// Execute renders templatePath. missingkey=error covers map lookups; a struct
// field the template names but Data lacks is already fatal without it.
func Execute(templatePath string, d Data) ([]byte, error) {
	src, err := os.ReadFile(templatePath) //nolint:gosec // template path is a CLI flag from the trusted caller
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	tmpl, err := template.New("tmux.conf").Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return buf.Bytes(), nil
}
