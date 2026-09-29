package controlmode

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// ClientCommandFlag is the third field of %begin/%end/%error for a block that a
// command THIS control client sent produced: tmux writes
// !!(state->flags & CMDQ_STATE_CONTROL) there (cmdq_fire_command). A hook on the
// remote runs its commands in our own command queue and so emits blocks flagged
// 0 — with tmux-og on the far side, one per after-new-window hook follows every
// new-window. Matching replies without this flag takes a hook's empty block as
// our reply and desynchronises every later round-trip (#276).
const ClientCommandFlag = 1

// Unescape decodes tmux control-mode %output data: bytes below 0x20 and the
// backslash are written as three-digit octal (\NNN); all else is literal.
// Operates on bytes — a UTF-8 rune may be split across two %output lines.
func Unescape(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+3 < len(data) {
			// try three octal digits
			d0, d1, d2 := data[i+1], data[i+2], data[i+3]
			if isOctal(d0) && isOctal(d1) && isOctal(d2) {
				out = append(out, (d0-'0')<<6|(d1-'0')<<3|(d2-'0'))
				i += 3
				continue
			}
		}
		out = append(out, data[i])
	}
	return out
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }

type Kind int

const (
	Other Kind = iota
	Output
	Begin
	End
	Error
	WindowClose
	Exit
	LayoutChange
	WindowAdd
	WindowRenamed
	SessionWindowChanged
	SessionChanged
	WindowPaneChanged
	Pause
	Continue
	SubscriptionChanged
)

type Line struct {
	Kind Kind
	Pane string
	Args []string
	Data []byte
	// Flags is the guard flags field, set on Begin/End/Error only; see
	// ClientCommandFlag.
	Flags int
}

// ParseLine is the string-based entry point, kept for the readBlock retained-
// line path (which already owns a string via sc.Text()) and for callers
// outside the hot %output loop. It's a thin wrapper over parseLine.
func ParseLine(raw string) Line {
	return parseLine([]byte(raw))
}

// extSep is the "%extended-output" age/payload separator, pre-allocated so
// cutExtSep never allocates the literal per call.
var extSep = []byte(" : ")

// cutSpace splits b at the first space, mirroring strings.Cut(string(b), " ")
// without ever converting b to a string.
func cutSpace(b []byte) (before, after []byte, found bool) {
	before, after, ok := bytes.Cut(b, []byte{' '})
	if !ok {
		return b, nil, false
	}
	return before, after, true
}

// cutExtSep splits b at the first " : ", mirroring strings.Cut(string(b), " : ").
func cutExtSep(b []byte) (before, after []byte, found bool) {
	before, after, ok := bytes.Cut(b, extSep)
	if !ok {
		return b, nil, false
	}
	return before, after, true
}

// fieldsToStrings copies each field to an owned string; used for the
// low-frequency verbs whose Args are never retained as []byte.
func fieldsToStrings(fields [][]byte) []string {
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = string(f)
	}
	return out
}

// parseLine is the zero-copy hot path: it never converts the whole line to a
// string. Every retained field is either a small string(...) conversion (verb,
// pane id, Fields), Unescape's freshly allocated output, or an explicit
// bytes.Clone — raw itself (and any slice of it) must never be retained
// beyond this call, since callers may pass a scanner buffer valid only until
// the next Scan().
func parseLine(raw []byte) Line {
	if len(raw) == 0 || raw[0] != '%' {
		return Line{Kind: Other}
	}
	verbB, rest, _ := cutSpace(raw)
	switch string(verbB) {
	case "%output":
		pane, data, _ := cutSpace(rest)
		return Line{Kind: Output, Pane: string(pane), Data: Unescape(data)}
	case "%extended-output":
		// Flow-control form of %output, emitted once pause-after is armed
		// (refresh-client -f pause-after=N): "%extended-output %pane <age-ms> :
		// <escaped-data>". Same payload escaping as %output; drop the pane's
		// pause age and treat it as ordinary output, or live output is lost.
		pane, r, _ := cutSpace(rest)
		_, data, _ := cutExtSep(r)
		return Line{Kind: Output, Pane: string(pane), Data: Unescape(data)}
	case "%begin":
		f := fieldsToStrings(bytes.Fields(rest))
		return Line{Kind: Begin, Args: f, Flags: guardFlags(f)}
	case "%end":
		f := fieldsToStrings(bytes.Fields(rest))
		return Line{Kind: End, Args: f, Flags: guardFlags(f)}
	case "%error":
		f := fieldsToStrings(bytes.Fields(rest))
		return Line{Kind: Error, Args: f, Flags: guardFlags(f)}
	case "%window-close":
		return Line{Kind: WindowClose, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%exit":
		return Line{Kind: Exit, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%layout-change":
		return Line{Kind: LayoutChange, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%window-add":
		return Line{Kind: WindowAdd, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%window-renamed":
		// name may contain spaces: id is the first token, the rest is the
		// whole name (kept in Data, not Fields-split). Data must be an owned
		// copy — it must not alias the scanner's reused buffer.
		id, name, _ := cutSpace(rest)
		return Line{Kind: WindowRenamed, Args: []string{string(id)}, Data: bytes.Clone(name)}
	case "%session-changed":
		// Emitted at attach and on every switch-client that moves this client.
		// Same shape as %window-renamed: id is the first token, the rest is the
		// whole session name, which may contain spaces.
		id, name, _ := cutSpace(rest)
		return Line{Kind: SessionChanged, Args: []string{string(id)}, Data: bytes.Clone(name)}
	case "%session-window-changed":
		return Line{Kind: SessionWindowChanged, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%window-pane-changed":
		return Line{Kind: WindowPaneChanged, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%pause":
		return Line{Kind: Pause, Args: fieldsToStrings(bytes.Fields(rest))}
	case "%subscription-changed":
		// "%subscription-changed <name> <session> <window> <index> <pane> : <value>",
		// where the ids are '-' for whatever the subscription's scope leaves
		// unresolved. Everything after the FIRST " : " is the format's value and
		// may hold ':' of its own (an issue title does), so cut once. tmux writes
		// the separator unconditionally, so an emptied value arrives as a trailing
		// space rather than as a line cutExtSep cannot split — which is what makes
		// "the option was unset" reportable at all.
		head, value, ok := cutExtSep(rest)
		if !ok {
			return Line{Kind: Other}
		}
		return Line{
			Kind: SubscriptionChanged,
			Args: fieldsToStrings(bytes.Fields(head)),
			Data: bytes.Clone(value),
		}
	case "%continue":
		return Line{Kind: Continue, Args: fieldsToStrings(bytes.Fields(rest))}
	default:
		return Line{Kind: Other}
	}
}

// guardFlags reads the flags field of a %begin/%end/%error guard line.
func guardFlags(fields []string) int {
	if len(fields) < 3 {
		return 0
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0
	}
	return n
}

type Reader struct {
	sc *bufio.Scanner
	// pending holds the lines a completed block yielded, in stream order, until
	// Next has handed them out one at a time.
	pending []Line
}

func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &Reader{sc: sc}
}

func (rd *Reader) Next() (Line, bool) {
	for {
		if len(rd.pending) > 0 {
			l := rd.pending[0]
			rd.pending = rd.pending[1:]
			return l, true
		}
		if !rd.sc.Scan() {
			return Line{}, false
		}
		l := parseLine(rd.sc.Bytes())
		if l.Kind != Begin {
			return l, true
		}
		rd.pending = rd.readBlock(l)
	}
}

// readBlock consumes a guarded block and returns, in stream order, each
// notification tmux emitted inside it followed by the block's own terminal line
// (Kind End or Error, Data = the command output alone). tmux emits the
// notifications a command causes inside that command's block, so they have to
// be lifted out — left in the body they read as that command's output.
//
// A body line is only taken for a notification when it parses as a known verb,
// so pane content that merely starts with '%' stays body.
func (rd *Reader) readBlock(begin Line) []Line {
	id := ""
	if len(begin.Args) > 0 {
		id = begin.Args[0]
	}
	var (
		out  []Line
		body []string
	)
	end := func(kind Kind, flags int) []Line {
		return append(out, Line{Kind: kind, Args: []string{id}, Flags: flags, Data: []byte(strings.Join(body, "\n"))})
	}
	for rd.sc.Scan() {
		raw := rd.sc.Text()
		t := ParseLine(raw)
		switch t.Kind {
		case End, Error:
			return end(t.Kind, t.Flags)
		case Other, Begin:
			body = append(body, raw)
		default:
			out = append(out, t)
		}
	}
	return end(End, begin.Flags)
}
