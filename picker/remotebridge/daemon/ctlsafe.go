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
// Invalid UTF-8 in the expansion measured two ways (empty on one remote, the
// 0xff byte kept with control bytes turned to spaces on another); either way no
// raw newline reaches the stream. An empty expansion makes that row
// unparsable, so it is skipped and the previous values persist, not unset.
// This is the remote half; the reader in controlmode bounds memory and keeps a
// newline inside a reply body from opening or closing a block, but cannot hold
// a raw newline in a top-level %subscription-changed value, so a socket holder
// replacing a format, or a remote whose regex does not match, still forges
// top-level lines.
func ctlSafe(format string) string {
	return "#{s/[[#{l::}cntrl#{l::}]]/ /:" + format + "}"
}
