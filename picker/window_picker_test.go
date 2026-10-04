package main

import (
	"strings"
	"testing"
)

func currentListModel(items []listItem) tuiModel {
	m := tuiModel{windowMode: true, ready: true, theme: "dark", width: 120, height: 30, sessionItems: items}
	m = m.recombine().withFilter()
	m.cursor = m.firstSelectable(0)
	return m
}

func winRow(sess string, idx int, scratch bool) listItem {
	t := sess + ":" + string(rune('0'+idx))
	return listItem{target: t, display: t, plain: t, searchText: t, session: sess, isScratch: scratch}
}

func TestWindowScratchFilter(t *testing.T) {
	items := []listItem{
		{target: "main", plain: "main", isHeader: true, session: "main"},
		winRow("main", 1, false),
		{target: "scratch-x", plain: "scratch-x", isHeader: true, session: "scratch-x"},
		winRow("scratch-x", 1, true),
	}
	m := currentListModel(items)
	for _, it := range m.visible {
		if it.session == "scratch-x" {
			t.Fatalf("scratch window visible by default: %+v", m.visible)
		}
	}
	m = m.toggleScratchOnly()
	var got []string
	for _, it := range m.visible {
		got = append(got, it.target)
	}
	if len(got) != 2 || got[1] != "scratch-x:1" {
		t.Fatalf("^s visible = %v, want scratch-x header + window only", got)
	}
}

func TestWindowFooterNoScopeHint(t *testing.T) {
	opts := scopeOpts()
	wm := tuiModel{windowMode: true, width: 200, tmuxOpts: opts}
	if got := stripANSI(wm.renderHints()); strings.Contains(got, "⇥:scope") {
		t.Errorf("window footer = %q, want no ⇥:scope", got)
	}
	sm := tuiModel{width: 200, tmuxOpts: opts}
	if got := stripANSI(sm.renderHints()); !strings.Contains(got, "⇥:scope") {
		t.Errorf("session footer = %q, want ⇥:scope", got)
	}
}

func TestWindowCursorFollowsTarget(t *testing.T) {
	a, b, c := winRow("main", 1, false), winRow("main", 2, false), winRow("main", 3, false)
	m := currentListModel([]listItem{a, b, c})
	m.cursor = 1 // main:2
	want := m.visible[m.cursor].target

	next, _ := m.Update(refreshMsg{items: []listItem{c, b, a}})
	gm := next.(tuiModel)
	if gm.currentTarget() != want {
		t.Errorf("after reorder cursor on %q, want %q", gm.currentTarget(), want)
	}

	gm.cursor = len(gm.visible) - 1
	want = gm.currentTarget()
	next, _ = gm.handleKey(wallKey("ctrl+g"))
	gm = next.(tuiModel)
	if gm.currentTarget() != want {
		t.Errorf("^g moved cursor to %q, want %q", gm.currentTarget(), want)
	}
	next, _ = gm.Update(refreshMsg{items: []listItem{b, c, a}})
	if gm = next.(tuiModel); gm.currentTarget() != want {
		t.Errorf("regrouped refresh moved cursor to %q, want %q", gm.currentTarget(), want)
	}

	// A vanished target lands on its neighbour, not the top.
	gm.cursor = 1
	next, _ = gm.Update(refreshMsg{items: []listItem{a, c}})
	if gm = next.(tuiModel); gm.cursor != 1 {
		t.Errorf("cursor after target vanished = %d, want 1", gm.cursor)
	}
}

func TestMirrorWindowKillConfirms(t *testing.T) {
	mirror := winRow("main", 1, false)
	mirror.bridgePane, mirror.bridgeSock = "%7", "/tmp/b.sock"
	var killed []string
	orig := bridgeKillWindow
	bridgeKillWindow = func(_ map[string]string, _, pane string) error {
		killed = append(killed, pane)
		return nil
	}
	t.Cleanup(func() { bridgeKillWindow = orig })

	for _, cancel := range []string{"n", "esc"} {
		m := currentListModel([]listItem{mirror})
		next, _ := m.handleKey(wallKey("ctrl+x"))
		m = next.(tuiModel)
		if len(m.killConfirm) != 1 || len(killed) != 0 {
			t.Fatalf("^x: confirm=%d kills=%v, want staged and no kill", len(m.killConfirm), killed)
		}
		if got := stripANSI(m.renderHints()); !strings.Contains(got, "kill main:1 on the remote?") || !strings.Contains(got, "(y/N)") {
			t.Errorf("prompt = %q, want the mirror kill prompt", got)
		}
		wall := m
		wall.mode = modeWall
		if got := stripANSI(wall.renderWallHints()); !strings.Contains(got, "(y/N)") {
			t.Errorf("wall prompt = %q, want (y/N)", got)
		}
		next, _ = m.handleKey(wallKey(cancel))
		if m = next.(tuiModel); len(m.killConfirm) != 0 || len(killed) != 0 {
			t.Fatalf("%s: confirm=%d kills=%v, want cancelled", cancel, len(m.killConfirm), killed)
		}
	}

	m := currentListModel([]listItem{mirror})
	next, _ := m.handleKey(wallKey("ctrl+x"))
	next, _ = next.(tuiModel).handleKey(wallKey("y"))
	if len(killed) != 1 || killed[0] != "%7" {
		t.Errorf("kills after y = %v, want [%%7]", killed)
	}
	_ = next
}
