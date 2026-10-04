package daemon

// ctlSafe wraps a format so its expansion carries no control byte. tmux writes
// option values raw into control-mode output, in reply bodies and in
// %subscription-changed values alike, so a "\n" in a remote-set value becomes
// extra stream lines the reader would take for framing.
//
// Each control byte of the expansion becomes one space: og_open's records are
// whitespace-split and JSON reads a space as whitespace, and the length stays
// one byte per byte, so openURLFormat's #{n:} bound still holds inside the
// wrapper. The pattern spells its colons #{l::} because a bare ":" ends the
// modifier, and "#:" is unescaped inside the regex only on next-3.9 (3.2a,
// 3.3a and 3.7c pass it through literally); "\n" in a tmux regex is the letter
// n. Wrapping the whole format covers every field and leaves tmux's own row
// separators, which sit outside the expansion, alone.
//
// Invalid UTF-8 in the expansion makes it empty on a UTF-8 remote: it fails
// closed and reads as unset, which for agentUsageFormat blanks the whole usage
// segment. This is the remote half; the reader in controlmode is the half that
// holds when a socket holder replaces a subscription format or the remote's
// regex differs.
func ctlSafe(format string) string {
	return "#{s/[[#{l::}cntrl#{l::}]]/ /:" + format + "}"
}
