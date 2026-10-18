package daemon

import (
	"fmt"
	"strconv"
)

// RelayEnvVar is the environment variable the daemon publishes the local
// terminal's relayable-graphics capability under. It is a binding cross-repo
// contract — noamsto/aeye reads it to decide whether a program running behind
// the bridge may hand the terminal a sixel image directly instead of going
// through the kitty/carousel path.
//
// Grammar: a comma-separated set of protocol tokens the *local* terminal can
// paint. Exactly one token is defined today, "sixel". Absent, empty, or an
// unrecognised token all mean no relayable capability; an unrecognised token
// is never permission to relay it.
const RelayEnvVar = "OG_RELAY_GRAPHICS"

// RelayEnvCmd returns the control-mode command that publishes value (a
// graphics.Relay.String()) into the bridged remote session's environment
// table, where it is visible to, and inherited by, every pane the session
// gains from this point on.
//
// This rides a control-mode set-environment against the remote SESSION,
// deliberately not the ssh command's own env prefix (sshControlArgs): a
// variable on that prefix reaches a remote pane only because the remote's own
// tmux update-environment allowlist names it, so a newly introduced variable
// there is silently inert against any remote whose config predates it.
// set-environment against the session carries no such allowlist and is,
// verified, inherited by panes split afterwards.
func RelayEnvCmd(session, value string) string {
	return fmt.Sprintf("set-environment -t %s %s %s", tmuxQuote(session), RelayEnvVar, tmuxQuote(value))
}

// RelayEnvUnsetCmd is RelayEnvCmd's teardown twin: it removes the variable
// from the remote session's environment table rather than leaving a stale
// value for whoever attaches to that session next.
func RelayEnvUnsetCmd(session string) string {
	return fmt.Sprintf("set-environment -u -t %s %s", tmuxQuote(session), RelayEnvVar)
}

// CellEnvVar is the environment variable the daemon publishes the local
// terminal's cell size under. It is a binding cross-repo contract —
// noamsto/aeye (cellsize.go) reads it before any other source to cut image
// crops to the shape of the preview box, because a remote tmux has no pixel
// size for a control-mode client and answers CSI 16 t with a default.
//
// Grammar: "<w>x<h>", each a positive integer count of pixels per cell, at
// most 512, e.g. "15x28". Absent means unknown. The value is the LOCAL
// terminal's cell size, never the remote's.
const CellEnvVar = "AEYE_CELL_PX"

// maxCellPx is the largest per-axis cell size CellEnvVar's grammar allows.
const maxCellPx = 512

// CellPx builds a CellEnvVar value from a client's reported cell width and
// height, or "" when either is not a canonical positive decimal in
// 1..maxCellPx. Empty, zero, signed, padded or oversize reports all collapse
// to "": an unknown size is never published as a guess.
func CellPx(w, h string) string {
	if !validCellAxis(w) || !validCellAxis(h) {
		return ""
	}
	return w + "x" + h
}

func validCellAxis(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s)
	return err == nil && n <= maxCellPx
}

// CellEnvCmd returns the control-mode command that publishes cell (a CellPx
// value) into the bridged remote session's environment table, for the same
// reason RelayEnvCmd does.
func CellEnvCmd(session, cell string) string {
	return fmt.Sprintf("set-environment -t %s %s %s", tmuxQuote(session), CellEnvVar, tmuxQuote(cell))
}

// CellEnvUnsetCmd removes the variable from the remote session's environment
// table: the teardown twin of CellEnvCmd, and the publish for an unknown size.
func CellEnvUnsetCmd(session string) string {
	return fmt.Sprintf("set-environment -u -t %s %s", tmuxQuote(session), CellEnvVar)
}

// CellEnvSyncCmd makes the remote session's variable match cell: set for a
// size, unset for "" so a value left by an earlier bridge never outlives it.
func CellEnvSyncCmd(session, cell string) string {
	if cell == "" {
		return CellEnvUnsetCmd(session)
	}
	return CellEnvCmd(session, cell)
}
