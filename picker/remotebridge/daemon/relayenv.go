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

// relayOwnedCond is the format that is true when no controller owns the
// session's size record or the record names token. The size-owner record is
// the arbiter between two controllers' daemons (#979, #1035), so an env write
// from the one that lost it must not land.
func relayOwnedCond(token string) string {
	return "#{?#{" + bridgeOwnerOption + "},#{m:" + token + "|*,#{" + bridgeOwnerOption + "}},1}"
}

// relayEnv builds the env writes for one remote session. The condition on a
// guarded write is evaluated by the remote tmux when the command runs, so
// racing claims cannot interleave a stale write. An empty token means size
// ownership is off (session pinning off) and writes stay unconditional.
type relayEnv struct{ target, token string }

func (e relayEnv) guard(cmd string) string {
	if e.token == "" {
		return cmd
	}
	return "if-shell -F -t " + tmuxQuote(e.target) + " " + tmuxQuote(relayOwnedCond(e.token)) + " " + tmuxQuote(cmd)
}

func (e relayEnv) relay(v string) string { return e.guard(RelayEnvCmd(e.target, v)) }

func (e relayEnv) cell(c string) string { return e.guard(CellEnvSyncCmd(e.target, c)) }

// publish returns the writes that make the remote session's env match view.
func (e relayEnv) publish(view *Viewing) []string {
	return e.publishValues(view.Relay.Load().String(), view.Cell())
}

// publishValues is publish for values the caller already snapshotted, so a
// batch and the check that it is still current read the same view.
func (e relayEnv) publishValues(relay, cell string) []string {
	return []string{e.relay(relay), e.cell(cell)}
}

// relayEnvTeardownCmd unsets both variables only while the session still has
// exactly attached clients. attached is the session_attached the caller just
// read, so a client attaching between that read and this write makes it not
// land.
func relayEnvTeardownCmd(sessID string, attached int) string {
	return "if-shell -F -t " + tmuxQuote(sessID) + " " + tmuxQuote("#{==:#{session_attached},"+strconv.Itoa(attached)+"}") + " " + tmuxQuote(RelayEnvUnsetCmd(sessID)+" ; "+CellEnvUnsetCmd(sessID))
}

// relayEnvRelinquishCmd is a departing owner's write while other controllers
// stay attached: relay empty and cell unknown are the values that are always
// safe for whichever controller the user is on, so the survivors fall back to
// the kitty/carousel path and an unknown crop size until a claim or release
// republishes. It lands only while the record names token, never while it is
// unset, so it cannot clear values a survivor already published.
func relayEnvRelinquishCmd(sessID, token string) string {
	return "if-shell -F -t " + tmuxQuote(sessID) + " " + tmuxQuote("#{m:"+token+"|*,#{"+bridgeOwnerOption+"}}") + " " + tmuxQuote(RelayEnvCmd(sessID, "")+" ; "+CellEnvUnsetCmd(sessID))
}
