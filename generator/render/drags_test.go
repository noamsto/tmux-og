package render

import (
	"strings"
	"testing"
)

func TestDragBindsGatedOnStockVersion(t *testing.T) {
	lines := strings.Split(dragBinds(keysPaths()), "\n")
	if lines[0] != `%if "#{==:#{version},next-3.9}"` {
		t.Fatalf("first line = %q", lines[0])
	}
	if lines[len(lines)-1] != "%endif" {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}
}

func TestDragStartBinds(t *testing.T) {
	p := keysPaths()
	lines := strings.Split(dragBinds(p), "\n")
	floatGate := "#{&&:" + bridgeGate + ",#{pane_floating_flag}}"

	var rootLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, "bind-key -N '"+dragStartNote+"' -T root ") {
			rootLines = append(rootLines, l)
		}
	}
	if len(rootLines) != len(dragStock) {
		t.Fatalf("%d root binds, want %d (one per stockdrags.txt line)", len(rootLines), len(dragStock))
	}

	wideGateCount := 0
	for i, s := range dragStock {
		head, word := trailingQuoted(t, rootLines[i])
		if got, want := unquote(t, word), s.cmd; got != want {
			t.Fatalf("%s %s: stock branch =\n%q\nwant\n%q", s.table, s.key, got, want)
		}
		gate := floatGate
		if s.cmd == "resize-pane -M" {
			gate = bridgeGate
			wideGateCount++
		}
		want := "bind-key -N '" + dragStartNote + "' -T " + s.table + " " + s.key + " if-shell -F -t = '" + gate + "' { " +
			"set -F @og_bridge_drag '#{pane_id}' ; " + s.cmd + " ; switch-client -T " + dragBindTable + " } "
		if head != want {
			t.Fatalf("%s %s: mirror branch =\n%q\nwant\n%q", s.table, s.key, head, want)
		}
	}
	if wideGateCount != 1 {
		t.Fatalf("%d stock binds took the wide gate, want exactly 1 (resize-pane -M)", wideGateCount)
	}
}

func TestDragEndTableComplete(t *testing.T) {
	p := keysPaths()
	lines := strings.Split(dragBinds(p), "\n")
	prefix := "bind-key -N '" + dragEndNote + "' -T " + dragBindTable + " "

	var endLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			endLines = append(endLines, l)
		}
	}
	if want := len(dragLocations) * len(dragModifiers); len(endLines) != want {
		t.Fatalf("%d og-bridge-drag binds, want %d", len(endLines), want)
	}

	wantKeys := map[string]bool{}
	for _, loc := range dragLocations {
		for _, mod := range dragModifiers {
			wantKeys[mod+"MouseDragEnd1"+loc] = true
		}
	}
	wantBody := `run-shell "` + bridgeCtl(p) + ` drag #{q:@og_bridge_drag}"`
	gotKeys := map[string]bool{}
	for _, l := range endLines {
		rest := strings.TrimPrefix(l, prefix)
		key, body, ok := strings.Cut(rest, " ")
		if !ok {
			t.Fatalf("malformed og-bridge-drag bind: %q", l)
		}
		if body != wantBody {
			t.Fatalf("%s: body = %q, want %q", key, body, wantBody)
		}
		gotKeys[key] = true
	}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("%d distinct keys, want %d", len(gotKeys), len(wantKeys))
	}
	for k := range wantKeys {
		if !gotKeys[k] {
			t.Fatalf("missing key %q", k)
		}
	}
}
