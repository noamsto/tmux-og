package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// maxRemoteDirCandidates bounds the dirs kept per host before the
// session-name collision and dedupe pass trims them to maxZoxideSuggestions.
const maxRemoteDirCandidates = 100

// remoteZoxideScript lists the host's zoxide dirs, one per line. The `-d`
// filter drops dead dirs and, since a newline inside a path splits it into
// fragments on this line-delimited wire, almost always the fragments too.
// printf %s, not echo "$l": dash's echo expands backslashes. A host without
// zoxide yields empty stdout and exit 0 (head's), the normal no-section case.
// No quote or backslash: the remote login shell may be fish and shellQuote
// only escapes quotes.
const remoteZoxideScript = `z=$(command -v zoxide 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/zoxide); ` +
	`"$z" query -l 2>/dev/null | while IFS= read -r l; do [ -d "$l" ] && printf %s "$l" && echo; done | head -n 300`

var remoteZoxideBody = "sh -c " + shellQuote(remoteZoxideScript)

// remoteZoxideArgv is sshProbe's ssh invocation for remoteZoxideBody. Never a
// non-batch ssh: a prompt would land in the picker's pty.
func remoteZoxideArgv(host string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-T",
		host,
		"--",
		remoteZoxideBody,
	}
}

// fetchRemoteZoxideFn is seamed for tests.
var fetchRemoteZoxideFn = sshRemoteZoxide

// sshRemoteZoxide returns host's raw zoxide entries; the error is an ssh
// failure, timeout or non-zero exit, never an absent zoxide.
func sshRemoteZoxide(host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", remoteZoxideArgv(host)...) //nolint:gosec // G204: fixed binary, argv passed without a shell
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return splitRemoteZoxideOutput(stdout.String()), nil
}

// splitRemoteZoxideOutput splits the script's stdout into entries. A path with
// a newline arrives as separate fragments; filterRemoteDirs drops them.
func splitRemoteZoxideOutput(out string) []string {
	var entries []string
	for line := range strings.SplitSeq(out, "\n") {
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}

// remoteDir is one remote zoxide dir, already vetted. path is the raw value the
// remote receives; name and shown are sanitized for local rendering.
type remoteDir struct {
	path  string
	name  string // derived session name
	shown string // path as displayed and searched
}

// filterRemoteDirs keeps the entries the transport and og-remote-open can
// carry: absolute, no newline/CR/NUL (validEmitField) and no backslash
// (og-remote-open's shell_quotable rejects it). The name comes from the raw
// path before sanitizing, so the sanitizer's length cut never shortens it
// from the middle.
func filterRemoteDirs(entries []string) []remoteDir {
	var dirs []remoteDir
	for _, raw := range entries {
		if len(dirs) == maxRemoteDirCandidates {
			break
		}
		if !strings.HasPrefix(raw, "/") || !validEmitField(raw) || strings.Contains(raw, `\`) {
			continue
		}
		name := sanitizeStatusText(sessionNameFromPath(raw))
		shown := sanitizeStatusText(raw)
		if name == "" || shown == "" {
			continue
		}
		dirs = append(dirs, remoteDir{path: raw, name: name, shown: shown})
	}
	return dirs
}

// remoteZoxideEntry is one host's cached dirs. Only a successful fetch sets
// done, so a host that needed auth is fetched again on the next Tab entry.
type remoteZoxideEntry struct {
	dirs     []remoteDir
	fetching bool
	done     bool
}

// remoteZoxideMsg carries one host's fetch result. The handler vets entries
// with filterRemoteDirs when err is nil.
type remoteZoxideMsg struct {
	host    string
	entries []string
	err     error
}

func remoteZoxideCmd(host string) tea.Cmd {
	return func() tea.Msg {
		entries, err := fetchRemoteZoxideFn(host)
		return remoteZoxideMsg{host: host, entries: entries, err: err}
	}
}

// remoteZoxideRows is host's "── New session ──" section for a single-host
// scope, or nil. The hide rule is name-based: the probe returns session
// names, not their working dirs.
func (m tuiModel) remoteZoxideRows(host string) []listItem {
	dirs := m.remoteZoxide[host].dirs
	if len(dirs) == 0 {
		return nil
	}
	block, taken := m.remoteHostBlock(host)
	if len(block) == 0 {
		return nil
	}
	hostRow := block[0]
	if hostRow.remoteInert || hostRow.remoteNeedsAuth || hostRow.remoteTailscaleCheck || hostRow.remoteUnreachable {
		return nil
	}
	for name := range hostRow.remoteHostSessionIDs {
		taken[name] = true
	}
	for _, bm := range m.mirrors {
		if bm.host == host {
			taken[bm.sess] = true
		}
	}

	exclude := parseExcludePatterns(envOrMap("PICKER_ZOXIDE_EXCLUDE", m.tmuxOpts, "@picker_zoxide_exclude", ""))
	cBlue := ansiFg(envOrMap("THM_BLUE", m.tmuxOpts, "@thm_blue", "#89b4fa"))
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", m.tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	iDir := envOrMap("PICKER_ICON_DIR", m.tmuxOpts, "@icon_dir", iconDir)
	reset := "\033[0m"

	var rows []listItem
	for _, d := range dirs {
		if len(rows) == maxZoxideSuggestions {
			break
		}
		if taken[d.name] || isExcluded(d.path, exclude) {
			continue
		}
		taken[d.name] = true
		rows = append(rows, listItem{
			target:        "remotedir:" + host + ":" + d.path,
			remoteDirHost: host,
			remoteDirPath: d.path,
			remoteDirName: d.name,
			display:       fmt.Sprintf("%s %s  %s", cBlue+iDir+reset, d.name, cDim+d.shown+reset),
			plain:         fmt.Sprintf("%s %s  %s", iDir, d.name, d.shown),
			searchText:    d.name + " " + d.shown,
		})
	}
	if len(rows) == 0 {
		return nil
	}

	rule := "── New session " + strings.Repeat("─", 220)
	divider := listItem{
		display:        cDim + rule + reset,
		plain:          rule,
		isHeader:       true,
		isZoxideHeader: true,
		headerLabel:    "New session",
		headerIcon:     iDir,
	}
	return append([]listItem{divider}, rows...)
}

// wantsRemoteZoxide reports whether the current single-host scope still needs
// its host's dirs fetched: not cached, not in flight, and the host's block
// absent or a pending placeholder rather than flagged as unusable.
func (m tuiModel) wantsRemoteZoxide() bool {
	if m.scope.kind != scopeHost || m.windowMode || m.emitPath != "" {
		return false
	}
	if e := m.remoteZoxide[m.scope.host]; e.done || e.fetching {
		return false
	}
	block, _ := m.remoteHostBlock(m.scope.host)
	if len(block) == 0 {
		return true
	}
	h := block[0]
	return !h.remoteInert && !h.remoteNeedsAuth && !h.remoteTailscaleCheck && !h.remoteUnreachable
}
