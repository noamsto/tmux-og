package daemon

// submitHold returns the index of the first byte of payload that must wait
// for the image upload, or len(payload) when nothing must.
//
// Pass-through is an allowlist: printable and UTF-8 text, a few readline
// editing controls, cursor/edit/focus sequences. Anything unknown or able to
// submit (CR, LF, Enter in any CSI-u or modifyOtherKeys encoding, mouse
// reports that can confirm a dialog, a further ctrl+v, lone or incomplete
// escapes) waits, along with everything after it, so the caller holds
// payload[i:] whole and in order.
//
// Passed units are complete and never a bracketed-paste marker, so the
// caller's paste scan state (inPaste/matched) stays valid across them
// without scanning them.
func submitHold(payload []byte) int {
	i := 0
	for i < len(payload) {
		n := passLen(payload[i:])
		if n == 0 {
			return i
		}
		i += n
	}
	return i
}

// passLen is the length of the leading unit of b if it may pass through,
// else 0. b is non-empty.
func passLen(b []byte) int {
	c := b[0]
	switch {
	case c == 0x1b:
		return escPassLen(b)
	case c >= 0x20, // printable, DEL and UTF-8
		c == 0x01, c == 0x02, c == 0x05, c == 0x06, c == 0x08, c == 0x09,
		c == 0x0b, c == 0x15, c == 0x17:
		return 1
	}
	return 0
}

func escPassLen(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	switch b[1] {
	case 'b', 'f', 'd', 0x7f:
		return 2
	case 'O':
		if len(b) < 3 {
			return 0
		}
		switch b[2] {
		case 'A', 'B', 'C', 'D', 'F', 'H':
			return 3
		}
		return 0
	case '[':
		return csiPassLen(b)
	}
	return 0
}

// csiPassLen classifies the CSI sequence at the start of b (ESC [).
func csiPassLen(b []byte) int {
	j := 2
	for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
		j++
	}
	if j >= len(b) || b[j] < 0x40 || b[j] > 0x7e {
		return 0
	}
	switch b[j] {
	case 'A', 'B', 'C', 'D', 'E', 'F', 'H', 'I', 'O', 'Z':
		return j + 1
	case '~':
		switch firstParam(b[2:j]) {
		case 27, 200, 201:
			return 0
		}
		return j + 1
	}
	return 0
}

// firstParam is the leading decimal parameter of a CSI parameter string,
// or -1 when it has none.
func firstParam(params []byte) int {
	n := -1
	for _, c := range params {
		if c < '0' || c > '9' {
			break
		}
		if n < 0 {
			n = 0
		}
		n = n*10 + int(c-'0')
		if n > 0xffff {
			break
		}
	}
	return n
}
