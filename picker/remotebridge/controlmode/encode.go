package controlmode

import "fmt"

// InputChunkBytes is the per-command input chunk size callers should pass to
// SendKeysArgs. Each byte becomes 2 hex chars plus a separator, so 2048 bytes
// yields a ~6KB command line — comfortably under tmux's command-line limits
// while cutting the number of send-keys round trips vs. the old 500-byte
// chunking by ~4x.
const InputChunkBytes = 2048

func SendKeysArgs(pane string, b []byte, maxHexPerCmd int) [][]string {
	if len(b) == 0 {
		return nil
	}
	if maxHexPerCmd < 1 {
		maxHexPerCmd = 1
	}
	var cmds [][]string
	for i := 0; i < len(b); i += maxHexPerCmd {
		end := min(i+maxHexPerCmd, len(b))
		cmd := []string{"send-keys", "-H", "-t", pane}
		for _, by := range b[i:end] {
			cmd = append(cmd, fmt.Sprintf("%02x", by))
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}
