package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/generator/config"
	"github.com/noamsto/tmux-og/generator/paths"
)

// The four flags are hand-transcribed bool->string maps in two dialects, and
// both arguments default to false — an inverted map is byte-identical wherever
// only the default is rendered, so pin both values of each.
func TestFlagStringsBothWays(t *testing.T) {
	on := Build(&config.Config{
		AINaming: config.AINaming{Enable: true},
		Resume:   config.Resume{Claude: true, Carousel: true},
		Dwindle:  config.Dwindle{Enable: true},
	}, &paths.Paths{})
	off := Build(&config.Config{}, &paths.Paths{})

	for _, tt := range []struct {
		name     string
		got      string
		want     string
		opposite string
	}{
		{"@ai_naming on", on.AINamingFlag, "1", off.AINamingFlag},
		{"@ai_naming off", off.AINamingFlag, "0", on.AINamingFlag},
		{"@resume_claude on", on.ResumeClaudeFlag, "on", off.ResumeClaudeFlag},
		{"@resume_claude off", off.ResumeClaudeFlag, "off", on.ResumeClaudeFlag},
		{"@resume_carousel on", on.ResumeCarouselFlag, "on", off.ResumeCarouselFlag},
		{"@resume_carousel off", off.ResumeCarouselFlag, "off", on.ResumeCarouselFlag},
		{"@dwindle on", on.DwindleFlag, "on", off.DwindleFlag},
		{"@dwindle off", off.DwindleFlag, "off", on.DwindleFlag},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
		if tt.got == tt.opposite {
			t.Errorf("%s renders the same string both ways", tt.name)
		}
	}
}

func tickPaths() *paths.Paths {
	return &paths.Paths{Scripts: map[string]string{
		"tmux-pr-enrich":    "/store/pr/bin/tmux-pr-enrich",
		"tmux-issue-stamp":  "/store/is/bin/tmux-issue-stamp",
		"tmux-agent-usage":  "/store/au/bin/tmux-agent-usage",
		"tmux-update-icons": "/store/ui/bin/tmux-update-icons",
	}, Bin: map[string]string{
		"tmux-session-resources": "/store/sr/bin/tmux-session-resources",
	}}
}

func TestTickHookIfShellJoinAndEscaping(t *testing.T) {
	got := tickHookIfShell(&config.Config{
		Enrich:     config.Enrich{Enable: true},
		AgentUsage: config.AgentUsage{Enable: true},
	}, tickPaths())

	want := `if-shell "tmux list-commands set-hook | grep -q -- -B" ` +
		`"set-hook -g -u -B '@og-pr-tick' \; set -gu '@og-pr-tick' \; ` +
		`set-hook -g -u -B '@og-backfill-tick' \; set -gu '@og-backfill-tick' \; ` +
		`set-hook -g -u -B '@og-usage-tick' \; set -gu '@og-usage-tick' \; ` +
		`set-hook -g -u -B '@og-sweep-tick' \; set -gu '@og-sweep-tick' \; ` +
		`set-hook -g -u -B '@og-res-tick' \; set -gu '@og-res-tick' \; ` +
		`set-hook -g -B '@og-pr-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \"/store/pr/bin/tmux-pr-enrich --tick\"' \; ` +
		`set-hook -g -B '@og-backfill-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \"/store/is/bin/tmux-issue-stamp --backfill\"' \; ` +
		`set-hook -g -B '@og-usage-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \"/store/au/bin/tmux-agent-usage --tick\"' \; ` +
		`set-hook -g -B '@og-sweep-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \"OG_TICK_SWEEP=1 /store/ui/bin/tmux-update-icons\"' \; ` +
		`set-hook -g -B '@og-res-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \"/store/sr/bin/tmux-session-resources --tick\"'" ` +
		`"display-message 'tmux-og: tmux predates 3.8 -B session monitors -- PR/backfill/usage polling and the agent sweep only run while a real client has this session attached, and remote session resources are not stamped at all'"`
	if got != want {
		t.Fatalf("tickHookIfShell =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Error("tickHookIfShell must be one line")
	}
}

// Every clear is emitted whatever is off, so a rebuild with a feature disabled
// cannot leave the previous generation's monitor firing at a dead store path.
func TestTickHookIfShellClearsSurviveFeaturesOff(t *testing.T) {
	got := tickHookIfShell(&config.Config{}, tickPaths())
	for _, n := range tickHookNames {
		for _, clear := range []string{"set-hook -g -u -B '" + n + "'", "set -gu '" + n + "'"} {
			if !strings.Contains(got, clear) {
				t.Errorf("missing clear %q", clear)
			}
		}
	}
	for _, absent := range []string{"tmux-pr-enrich", "tmux-issue-stamp", "tmux-agent-usage"} {
		if strings.Contains(got, absent) {
			t.Errorf("%s armed with its feature off", absent)
		}
	}
	if !strings.Contains(got, "OG_TICK_SWEEP=1 /store/ui/bin/tmux-update-icons") {
		t.Error("the sweep hook is unconditional")
	}
	if !strings.Contains(got, "/store/sr/bin/tmux-session-resources --tick") {
		t.Error("the session-resource hook is unconditional")
	}
}

func TestEnrichIconsDoubledOnlyDoublesOverrides(t *testing.T) {
	cfg := &config.Config{Enrich: config.Enrich{Icons: map[string]string{"linear": "L#"}}}
	got := enrichIconsDoubled(cfg)
	if got["linear"] != "L##" {
		t.Errorf("override = %q, want %q", got["linear"], "L##")
	}
	if got["github"] != enrichIconDefaults["github"] {
		t.Errorf("default github = %q, want the raw default", got["github"])
	}
	if raw := enrichIconsRaw(cfg); raw["linear"] != "L#" {
		t.Errorf("raw dialect = %q, want %q", raw["linear"], "L#")
	}
}

func TestAgentUsageArgsFallbacks(t *testing.T) {
	cfg := &config.Config{
		AgentUsage:   config.AgentUsage{Enable: true, MonthlyThreshold: 50},
		ProcessIcons: map[string]string{"claude": "C"},
	}
	got := agentUsageArgs(cfg)
	want := " --icon-usage-claude 'C' --icon-usage-codex '\U0001F916' --icon-usage-cursor '\U0001F9CA'" +
		" --icon-usage-pi '\U0001F967' --agent-usage-monthly-threshold '50'"
	if got != want {
		t.Fatalf("agentUsageArgs = %q, want %q", got, want)
	}
	if off := agentUsageArgs(&config.Config{}); off != "" {
		t.Fatalf("agentUsageArgs with the segment off = %q, want empty", off)
	}
}

func TestBridgeOptsShape(t *testing.T) {
	got := bridgeOpts()
	if len(got) != len(bridgeOptNames) {
		t.Fatalf("%d entries, want %d", len(got), len(bridgeOptNames))
	}
	if got["pr_check_state"] != "#{?#{@bridge_win},#{@bridge_pr_check_state},#{@pr_check_state}}" {
		t.Fatalf("pr_check_state = %q", got["pr_check_state"])
	}
}

// The tag is a pure tmux format, so the only honest test is tmux evaluating it:
// a substring check passes with the comparison inverted.
func TestCrewRepoTagEvaluates(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH: check generator-go-lint's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux not on PATH")
	}
	// A short socket dir: macOS sun_path is 104 bytes and t.TempDir() embeds the
	// long test name.
	dir, err := os.MkdirTemp("/tmp", "og-1165-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// A scratch server must not touch the live one or the shared status/data dirs,
	// and -f /dev/null keeps a wrapped tmux from loading the full og config.
	env := append(os.Environ(), "TMUX_TMPDIR="+dir, "TMUX=",
		"CLAUDE_STATUS_DIR="+filepath.Join(dir, "status"), "XDG_DATA_HOME="+filepath.Join(dir, "data"))
	t.Cleanup(func() {
		kill := exec.Command(tmuxBin, "-L", "crewrepo", "kill-server") //nolint:gosec // tmux path from LookPath
		kill.Env = env
		_ = kill.Run()
	})
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(tmuxBin, append([]string{"-f", "/dev/null", "-L", "crewrepo"}, args...)...) //nolint:gosec // tmux path from LookPath, args are test literals
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sessDir := filepath.Join(dir, "tmux-og")
	if err := os.Mkdir(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("new-session", "-d", "-s", "probe", "-c", sessDir)
	eval := func() string { return run("display-message", "-p", "-t", "probe:", crewRepoTag) }

	if got := eval(); got != "" {
		t.Errorf("@crew_repo unset: %q, want empty", got)
	}
	run("set-option", "-w", "-t", "probe:", "@crew_repo", "tmux-og")
	if got := eval(); got != "" {
		t.Errorf("@crew_repo equal to the session repo: %q, want empty", got)
	}
	run("set-option", "-w", "-t", "probe:", "@crew_repo", "dispatcher")
	if got := eval(); !strings.Contains(got, "dispatcher ") {
		t.Errorf("@crew_repo different: %q, want the repo name", got)
	}
	run("set-option", "-w", "-t", "probe:", "@bridge_placed", "1")
	if got := eval(); got != "" {
		t.Errorf("placed mirror: %q, want empty (host badge already shows origin)", got)
	}
}

func TestCrewRepoTagInStatusFormat(t *testing.T) {
	d := Build(&config.Config{}, &paths.Paths{})
	out, err := Execute(writeTemplate(t, "x {{.CrewRepoTag}} y\n"), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), crewRepoTag) {
		t.Fatalf("CrewRepoTag not rendered: %s", out)
	}
}
