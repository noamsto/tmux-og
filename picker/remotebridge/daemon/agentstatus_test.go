package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseAgentStatus(t *testing.T) {
	body := strings.Join([]string{
		"%1|claude|processing 1700000000 |",                       // trailing empty fields trimmed away
		"%2|nvim|||||||",                                          // mirrored pane, no agent
		"%3|claude|waiting 1700000042 1||ENG-7||||fix | y",        // unseen, issues, task holding a '|'
		"%4|fish|garbage||",                                       // unparsable stamp reads as no agent
		"%5|claude||||plan-critic|working|colour111|grill it",     // a decorated role pane
		"%6|claude||||#(id)|WORKING|#[fg=red]|",                   // markup and an uppercase state drop
		"%7|claude||||plan-critic-with-a-very-long-name||red|",    // over its cap, so dropped whole
		"%8|pi|processing 1700000200 |idle 1700000050 bg=2|ENG-7", // screen-scraped state alongside a hook stamp
	}, "\n")

	got := parseAgentStatus(body)
	if len(got) != 8 {
		t.Fatalf("got %d rows, want 8 (every mirrored pane): %+v", len(got), got)
	}
	if got[0].pane != "%1" || got[0].proc != "claude" || got[0].state != "processing" || got[0].ts != 1700000000 || got[0].unseen {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[1].proc != "nvim" || got[1].state != "" {
		t.Errorf("agent-free pane must survive with its command: %+v", got[1])
	}
	w := got[2]
	if w.pane != "%3" || w.state != "waiting" || !w.unseen || w.issues != "ENG-7" || w.task != "fix | y" {
		t.Errorf("row 2 = %+v", w)
	}
	if got[3].proc != "fish" || got[3].state != "" {
		t.Errorf("unparsable stamp = %+v, want no state", got[3])
	}
	if r := got[4]; r.crewRole != "plan-critic" || r.crewState != "working" || r.crewColor != "colour111" || r.task != "grill it" {
		t.Errorf("decorated role pane = %+v", r)
	}
	// The border format these three reach is RENDERED by the local tmux, so a
	// value carrying '#(...)' would run that command on this host.
	if r := got[5]; r.crewRole != "" || r.crewState != "" || r.crewColor != "" {
		t.Errorf("unshaped crew values must drop, got %+v", r)
	}
	if r := got[6]; r.crewRole != "" || r.crewColor != "red" {
		t.Errorf("an over-cap role drops whole and takes nothing else with it: %+v", r)
	}
	s := got[7]
	if s.pane != "%8" || s.state != "processing" || s.screenState != "idle" || s.screenTS != 1700000050 || s.screenFlags != "bg=2" || s.issues != "ENG-7" {
		t.Errorf("row 7 (screen-scraped) = %+v", s)
	}
}

// A role pane the dispatcher decorated carries its badge across as @bridge_*,
// which is what the local pane-border-format draws from — the local pane runs a
// renderer and knows neither its role nor its state.
func TestAgentShipperStampsCrewDecorations(t *testing.T) {
	a := &agentShipper{dir: t.TempDir(), sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)

	// No agent state: a parked role pane still has to draw its border.
	rows := []paneStatus{{pane: "%1", proc: "claude", crewRole: "reviewer", crewState: "idle", crewColor: "colour114"}}
	a.apply(cfg, rows)
	want := []string{
		"set-option", "-p", "-t", "%7", "@bridge_crew_role", "reviewer", ";",
		"set-option", "-p", "-t", "%7", "@bridge_crew_state", "idle", ";",
		"set-option", "-p", "-t", "%7", "@bridge_crew_role_color", "colour114",
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
		t.Fatalf("crew stamp = %v, want one sequence %v", calls, want)
	}

	a.apply(cfg, rows)
	if len(calls) != 2 {
		t.Errorf("unchanged decorations re-stamped: %v", calls)
	}

	// Only the field that moved is re-sent.
	rows[0].crewState = "working"
	a.apply(cfg, rows)
	want = []string{"set-option", "-p", "-t", "%7", "@bridge_crew_state", "working"}
	if len(calls) != 3 || !reflect.DeepEqual(calls[2], want) {
		t.Errorf("state change = %v, want %v", calls[2:], want)
	}

	// The remote drops the decoration: the option is unset, not left stale.
	rows[0].crewRole, rows[0].crewState, rows[0].crewColor = "", "", ""
	a.apply(cfg, rows)
	want = []string{
		"set-option", "-p", "-t", "%7", "-u", "@bridge_crew_role", ";",
		"set-option", "-p", "-t", "%7", "-u", "@bridge_crew_state", ";",
		"set-option", "-p", "-t", "%7", "-u", "@bridge_crew_role_color",
	}
	if len(calls) != 4 || !reflect.DeepEqual(calls[3], want) {
		t.Errorf("undecorate = %v, want %v", calls[3:], want)
	}
}

// mirrorCfg is a Config wired to two mirror panes, capturing the local tmux
// commands the shipper issues.
func mirrorCfg(calls *[][]string) Config {
	return Config{
		LocalPanes: func() map[string]string { return map[string]string{"%1": "%7", "%2": "%8"} },
		LocalTmux: func(args ...string) error {
			*calls = append(*calls, args)
			return nil
		},
	}
}

func TestAgentShipperApply(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", skew: 10, written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)

	a.apply(cfg, []paneStatus{
		{pane: "%1", proc: "claude", state: "waiting", ts: 1700000000, unseen: true, task: "ship it", issues: "ENG-7"},
		{pane: "%9", state: "done", ts: 1700000000}, // no local mirror — skipped
	})

	body, err := os.ReadFile(filepath.Join(dir, "panes", "7"))
	if err != nil {
		t.Fatalf("pane file: %v", err)
	}
	want := "state=waiting\ntimestamp=1700000010\nsession=lab-mono\nunseen=1\n"
	if string(body) != want {
		t.Errorf("pane file =\n%q\nwant\n%q", body, want)
	}
	if strings.Contains(string(body), "transcript=") {
		t.Error("transcript path names a file on the remote's disk; it must not be written")
	}
	for sub, want := range map[string]string{"tasks": "ship it\n", "issues": "ENG-7\n"} {
		got, err := os.ReadFile(filepath.Join(dir, sub, "7"))
		if err != nil || string(got) != want {
			t.Errorf("%s/7 = %q (%v), want %q", sub, got, err, want)
		}
	}

	// The agent exits: its pane stops reporting, so the files go with it.
	a.apply(cfg, nil)
	for _, sub := range []string{"panes", "tasks", "issues"} {
		if _, err := os.Stat(filepath.Join(dir, sub, "7")); !os.IsNotExist(err) {
			t.Errorf("%s/7 outlived the agent", sub)
		}
	}
}

// A panes/ file this shipper writes must carry the LOCAL tmux server's own
// PID as server=, so a second server's boot-time prune can tell it belongs to
// a still-live server rather than deleting it (#676).
func TestAgentShipperStampsServerPID(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)
	cfg.LocalTmuxOut = func(args ...string) (string, error) {
		return "4242", nil
	}

	a.apply(cfg, []paneStatus{
		{pane: "%1", proc: "claude", state: "waiting", ts: 1700000000},
	})

	body, err := os.ReadFile(filepath.Join(dir, "panes", "7"))
	if err != nil {
		t.Fatalf("pane file: %v", err)
	}
	if !strings.Contains(string(body), "server=4242\n") {
		t.Errorf("pane file = %q, want it to contain server=4242", body)
	}
}

// The local pane runs a renderer, so its own command tells the icons nothing.
// @bridge_proc carries the remote's — stamped for every mirrored pane, agent or
// not, and only when it changes (else it is a fork per pane per second).
func TestAgentShipperStampsRemoteCommand(t *testing.T) {
	a := &agentShipper{dir: t.TempDir(), sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)

	rows := []paneStatus{{pane: "%1", proc: "nvim"}, {pane: "%2", proc: "claude", state: "processing", ts: 1700000000}}
	a.apply(cfg, rows)
	want := [][]string{
		{"set-option", "-p", "-t", "%7", "@bridge_proc", "nvim"},
		{"set-option", "-p", "-t", "%8", "@bridge_proc", "claude"},
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls, want) {
		t.Fatalf("stamps = %v, want %v", calls, want)
	}

	a.apply(cfg, rows)
	if len(calls) != 2 {
		t.Errorf("unchanged commands re-stamped: %v", calls)
	}

	rows[0].proc = "fish"
	a.apply(cfg, rows)
	if len(calls) != 3 || calls[2][5] != "fish" {
		t.Errorf("a changed command should re-stamp: %v", calls)
	}
}

// An agent-free pane is still mirrored — it gets the icon stamp, but no state
// file, and a file left from a previous agent goes away.
func TestAgentShipperDropsFilesWhenAgentLeavesPane(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)

	a.apply(cfg, []paneStatus{{pane: "%1", proc: "claude", state: "done", ts: 1700000000}})
	a.apply(cfg, []paneStatus{{pane: "%1", proc: "fish"}})

	if _, err := os.Stat(filepath.Join(dir, "panes", "7")); !os.IsNotExist(err) {
		t.Error("state file outlived the agent that left the pane")
	}
	last := calls[len(calls)-1]
	if last[5] != "fish" {
		t.Errorf("last stamp = %v, want the pane's new command", last)
	}
}

// A screen-scraped agent (pi, codex, cursor — #635) has no hook, so its state
// rides @agent_screen alone. It must land in screen/<id>, independent of
// panes/, tasks/, issues/ — none of which a screen-only pane ever gets.
func TestAgentShipperWritesScreenFile(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", skew: 10, written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)

	a.apply(cfg, []paneStatus{
		{pane: "%1", proc: "pi", screenState: "processing", screenTS: 1700000000, screenFlags: "bg=2"},
	})

	body, err := os.ReadFile(filepath.Join(dir, "screen", "7"))
	if err != nil {
		t.Fatalf("screen file: %v", err)
	}
	want := "state=processing\ntimestamp=1700000010\nbg=2\n"
	if string(body) != want {
		t.Errorf("screen file =\n%q\nwant\n%q", body, want)
	}
	for _, sub := range []string{"panes", "tasks", "issues"} {
		if _, err := os.Stat(filepath.Join(dir, sub, "7")); !os.IsNotExist(err) {
			t.Errorf("a screen-only pane must not get a %s file", sub)
		}
	}

	// The screen-scraper's verdict clears (agent exited back to a shell):
	// the file goes, independently of the hook-driven panes/ file it never had.
	a.apply(cfg, []paneStatus{{pane: "%1", proc: "fish"}})
	if _, err := os.Stat(filepath.Join(dir, "screen", "7")); !os.IsNotExist(err) {
		t.Error("screen file outlived the scraper's cleared verdict")
	}
}

// An unchanged screen row must not be rewritten, the same rule panes/ already
// gets — and it must be independent of the hook state, since a Claude pane
// stamps only @claude_status and a screen-only pane stamps only
// @agent_screen.
func TestAgentShipperLeavesUnchangedScreenRowAlone(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)
	row := paneStatus{pane: "%1", proc: "pi", screenState: "idle", screenTS: 1700000000}
	a.apply(cfg, []paneStatus{row})

	path := filepath.Join(dir, "screen", "7")
	seen := "state=idle\ntimestamp=1700000000\n"
	if got, err := os.ReadFile(path); err != nil || string(got) != seen {
		t.Fatalf("seed write = %q (%v), want %q", got, err, seen)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.apply(cfg, []paneStatus{row})
	if got, _ := os.ReadFile(path); string(got) != "tampered" {
		t.Errorf("unchanged screen row was rewritten:\n%q", got)
	}

	row.screenTS = 1700000100
	a.apply(cfg, []paneStatus{row})
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "timestamp=1700000100") {
		t.Errorf("new remote screen state should overwrite: %q", got)
	}
}

// Teardown (a dying bridge) must drop the screen file exactly as it drops
// panes/tasks/issues.
func TestAgentShipperClearDropsScreenFile(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)
	a.apply(cfg, []paneStatus{{pane: "%1", proc: "pi", screenState: "processing", screenTS: 1700000000}})

	a.clear()
	if _, err := os.Stat(filepath.Join(dir, "screen", "7")); !os.IsNotExist(err) {
		t.Error("clear left the screen file behind")
	}
}

// Looking at a mirror window runs the local mark-seen hook, which strips
// `unseen` from the file. The remote's stamp still says 1 until its agent writes
// again, so an unchanged row must not be written back over that.
func TestAgentShipperLeavesUnchangedRowsAlone(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)
	row := paneStatus{pane: "%1", state: "waiting", ts: 1700000000, unseen: true}
	a.apply(cfg, []paneStatus{row})

	path := filepath.Join(dir, "panes", "7")
	seen := "state=waiting\ntimestamp=1700000000\nsession=lab-mono\n"
	if err := os.WriteFile(path, []byte(seen), 0o644); err != nil {
		t.Fatal(err)
	}
	a.apply(cfg, []paneStatus{row})
	if got, _ := os.ReadFile(path); string(got) != seen {
		t.Errorf("unchanged row was rewritten:\n%q", got)
	}

	// A fresh remote write (new timestamp) does land, unseen and all.
	row.ts = 1700000100
	a.apply(cfg, []paneStatus{row})
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "unseen=1") {
		t.Errorf("new remote state should overwrite: %q", got)
	}
}

// A bridge that dies must not leave a mirror pane's state behind: the shell-side
// prune collects by server-start mtime and would keep it until a tmux restart.
func TestAgentShipperClear(t *testing.T) {
	dir := t.TempDir()
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	var calls [][]string
	cfg := mirrorCfg(&calls)
	a.apply(cfg, []paneStatus{{pane: "%1", state: "processing", ts: 1700000000}})

	a.clear()
	if _, err := os.Stat(filepath.Join(dir, "panes", "7")); !os.IsNotExist(err) {
		t.Error("clear left the pane file behind")
	}
	if len(a.written) != 0 {
		t.Errorf("written = %v, want empty", a.written)
	}
}

func TestAgentShipperNoLocalPanes(t *testing.T) {
	a := &agentShipper{dir: t.TempDir(), written: map[string]paneStatus{}}
	a.apply(Config{}, []paneStatus{{pane: "%1", state: "done", ts: 1}})
	if len(a.written) != 0 {
		t.Errorf("no LocalPanes seam should write nothing, got %v", a.written)
	}
}

// The hazard the split between stamp and apply exists for: a
// %subscription-changed carries ONE pane, and reaping against it would read
// every other pane as "stopped reporting" and delete its files — an agent's
// state vanishing from the status bar because a different pane changed.
func TestAgentShipperQueuedFlushDoesNotReapOtherPanes(t *testing.T) {
	dir := t.TempDir()
	a := newAgentShipper("lab-mono", 0)
	a.dir = dir
	var calls [][]string
	cfg := mirrorCfg(&calls)

	// Both panes reporting, established by a full read.
	a.apply(cfg, []paneStatus{
		{pane: "%1", proc: "claude", state: "waiting", ts: 1700000000},
		{pane: "%2", proc: "claude", state: "processing", ts: 1700000000},
	})
	for _, id := range []string{"7", "8"} {
		if _, err := os.Stat(filepath.Join(dir, "panes", id)); err != nil {
			t.Fatalf("panes/%s not written: %v", id, err)
		}
	}

	// One pane's stamp moves. Subscribed and freshly polled, so flush does no
	// read of its own and applies the queued row alone.
	a.subscribed = true
	a.lastPoll, a.lastGen = time.Now(), uint64(0)
	a.queue("%2|claude|done 1700000100 |")

	var issued []string
	a.flush(cfg, replyRT(&issued, body("")), 0, true)

	if len(issued) != 0 {
		t.Errorf("issued %v, want no round-trip on the notification path", issued)
	}
	if _, err := os.Stat(filepath.Join(dir, "panes", "7")); err != nil {
		t.Errorf("the pane nobody reported on lost its state: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "panes", "8"))
	if err != nil || !strings.Contains(string(got), "state=done") {
		t.Errorf("panes/8 = %q (%v), want the queued state", got, err)
	}

	// The backstop read is the only caller holding the whole set, so it is the
	// one that may decide a pane has stopped reporting.
	a.apply(cfg, []paneStatus{{pane: "%2", proc: "claude", state: "done", ts: 1700000100}})
	if _, err := os.Stat(filepath.Join(dir, "panes", "7")); !os.IsNotExist(err) {
		t.Error("a full read that omits a pane must reap it")
	}
}
