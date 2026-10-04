package controlmode

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// ClientCommandFlag is the third field of %begin/%end/%error for a block that a
// command THIS control client sent produced: tmux writes
// !!(state->flags & CMDQ_STATE_CONTROL) there (cmdq_fire_command). A hook on the
// remote runs its commands in our own command queue and so emits blocks flagged
// 0 — with tmux-og on the far side, one per after-new-window hook follows every
// new-window. Matching replies without this flag takes a hook's empty block as
// our reply and desynchronises every later round-trip (#276).
const ClientCommandFlag = 1

const (
	// MaxLine is the longest line the Reader keeps, newline excluded: at least
	// 20× the largest genuine line (a ~32 KiB %output, a ~50 KiB capture row).
	MaxLine = 1 << 20
	// MaxBody is the most reply body the Reader retains for one block: enough
	// for an extreme 1000×300 dense-truecolor capture-pane -e (~15 MB).
	MaxBody = 16 << 20
)

// ErrReplyTooLarge is the Err of a reply whose body exceeded MaxBody or held a
// line longer than MaxLine; the Reader read it through and kept none of it.
var ErrReplyTooLarge = errors.New("controlmode: reply body exceeds MaxBody")

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
	// Err is set on a reply that failed in the reader itself; see
	// ErrReplyTooLarge.
	Err error
}

// ParseLine is the string-based entry point for callers outside the Reader's
// hot %output loop. It's a thin wrapper over parseLine.
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
// beyond this call, since the Reader passes its read buffer, valid only until
// the next read.
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
		// copy — it must not alias the Reader's reused buffer.
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

// validGuard reports whether a %begin's fields are the shape tmux writes: time,
// command number and flags, all unsigned decimal, flags 0 or 1.
func validGuard(fields []string) bool {
	if len(fields) != 3 || (fields[2] != "0" && fields[2] != "1") {
		return false
	}
	for _, f := range fields {
		if f == "" {
			return false
		}
		for i := range len(f) {
			if f[i] < '0' || f[i] > '9' {
				return false
			}
		}
	}
	return true
}

// Reader yields a control-mode stream one Line at a time, folding each guarded
// %begin…%end/%error block into a single terminal Line (Kind End or Error, Args
// = the %begin's time, Data = the command output alone).
//
// Framing follows only what tmux itself can write (#860):
//   - tmux writes %begin and its %end/%error synchronously with the same three
//     fields (cmdq_fire_command, cmdq_guard), so only a guard repeating the
//     %begin's fields closes a block, blocks never nest, and a guard outside a
//     block — or a malformed %begin — is Other.
//   - %subscription-changed is written only from a timer, which cannot fire
//     inside a block, so in a block it is body.
//   - %exit is printed by the exiting client, so it is always the last line: in
//     a block it is genuine only when end-of-stream follows it, else body.
//   - Lines past MaxLine and bodies past MaxBody are read through, never kept:
//     an overlong top-level line is Other, an oversized block fails as Error
//     with ErrReplyTooLarge, and the stream goes on.
//
// tmux next-3.8 builds between d29aa121 and 6db5175e write the notifications
// a command causes inside that command's block (#276). SetLiftInBlock(true)
// returns each such line the moment it is read, ahead of the terminal line. It
// is off by default: in any other tmux a body line that parses as a verb is
// pane content (a capture-pane row, a hook's display-message), and acting on it
// would let a pane drive the mirror. %output, %extended-output, %pause and
// %continue stay body even when it is on: no tmux writes a genuine %output in a
// block, and an in-block %pause/%continue only answers this client's own
// refresh-client -A, whose reply the daemon acts on instead. A body line is only
// taken for a notification when it parses as a known verb
// (docs/agents/bridge-daemon.md).
type Reader struct {
	br *bufio.Reader
	// long accumulates a line too long for br's buffer; reused across lines.
	long  []byte
	ended bool

	// open block state: the %begin's fields and flags, the retained body, and
	// whether the body overflowed (and so is no longer retained).
	open     bool
	begin    [3]string
	flags    int
	body     []byte
	bodyLine bool
	overflow bool
	// heldExit is an owned copy of an in-block %exit awaiting the next read.
	heldExit []byte

	// liftInBlock returns in-block notifications other than %output, %pause and
	// %continue; its zero value keeps them as body. Set from another goroutine,
	// see SetLiftInBlock.
	liftInBlock atomic.Bool
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// SetLiftInBlock chooses whether an in-block notification is returned (true) or
// kept in the reply body (false, the default). It is safe to call concurrently
// with Next and takes effect from the next line read.
func (rd *Reader) SetLiftInBlock(lift bool) { rd.liftInBlock.Store(lift) }

// LiftsInBlock reports whether a remote tmux reporting #{version} == version
// may write notifications inside a command's block. d29aa121 made notifications
// synchronous events, written in-block; 6db5175e (merged 9228f97d, 2026-08-03)
// defers them out of guard blocks again. Every build in between reports
// next-3.8, so only next-3.8 lifts. A string that is neither a release (X.Y, an
// optional letter, an optional -rc or -rcN) nor next-X.Y cannot be ruled out
// and lifts too.
func LiftsInBlock(version string) bool {
	rest, next := strings.CutPrefix(version, "next-")
	major, rest, ok := cutDigits(rest)
	if !ok || !strings.HasPrefix(rest, ".") {
		return true
	}
	minor, rest, ok := cutDigits(rest[1:])
	if !ok {
		return true
	}
	if len(rest) > 0 && rest[0] >= 'a' && rest[0] <= 'z' {
		rest = rest[1:]
	}
	if tail, found := strings.CutPrefix(rest, "-rc"); found {
		_, rest, _ = cutDigits(tail)
	}
	if rest != "" {
		return true
	}
	return next && major == "3" && minor == "8"
}

// cutDigits splits s after its leading run of ASCII digits; ok is false when
// there is none.
func cutDigits(s string) (digits, rest string, ok bool) {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[:n], s[n:], n > 0
}

// Next returns the next line, or false once the stream has ended. A Line never
// aliases reader state: callers may keep it across later calls.
func (rd *Reader) Next() (Line, bool) {
	for {
		raw, tooLong, ok := rd.readLine()
		if !ok {
			return rd.finish()
		}
		if !rd.open {
			if l, ok := rd.topLevel(raw, tooLong); ok {
				return l, true
			}
			continue
		}
		if l, ok := rd.inBlock(raw, tooLong); ok {
			return l, true
		}
	}
}

func (rd *Reader) topLevel(raw []byte, tooLong bool) (Line, bool) {
	if tooLong {
		return Line{Kind: Other}, true
	}
	l := parseLine(raw)
	switch l.Kind {
	case Begin:
		if !validGuard(l.Args) {
			return Line{Kind: Other}, true
		}
		rd.open, rd.begin, rd.flags = true, [3]string(l.Args), l.Flags
		return Line{}, false
	case End, Error:
		return Line{Kind: Other}, true
	default:
		return l, true
	}
}

func (rd *Reader) inBlock(raw []byte, tooLong bool) (Line, bool) {
	if rd.heldExit != nil {
		rd.appendBody(rd.heldExit)
		rd.heldExit = nil
	}
	if tooLong {
		rd.setOverflow()
		return Line{}, false
	}
	l := parseLine(raw)
	switch l.Kind {
	case End, Error:
		if slices.Equal(l.Args, rd.begin[:]) {
			return rd.closeBlock(l.Kind), true
		}
		rd.appendBody(raw)
	case Exit:
		rd.heldExit = bytes.Clone(raw)
	case Begin, SubscriptionChanged, Output, Pause, Continue, Other:
		rd.appendBody(raw)
	default:
		if rd.liftInBlock.Load() {
			return l, true
		}
		rd.appendBody(raw)
	}
	return Line{}, false
}

// finish drains an open block at end-of-stream: a held %exit was genuine, and
// the block resolves with a synthesized End carrying its partial body.
func (rd *Reader) finish() (Line, bool) {
	if !rd.open {
		return Line{}, false
	}
	if rd.heldExit != nil {
		l := parseLine(rd.heldExit)
		rd.heldExit = nil
		return l, true
	}
	return rd.closeBlock(End), true
}

func (rd *Reader) appendBody(raw []byte) {
	if rd.overflow {
		return
	}
	sep := 0
	if rd.bodyLine {
		sep = 1
	}
	n := len(rd.body) + sep + len(raw)
	if n > MaxBody {
		rd.setOverflow()
		return
	}
	if n > cap(rd.body) {
		// Doubled by make+copy, not a bytes.Buffer: its append-make growth
		// allocates every step twice under -race, breaking the memory bound.
		grown := make([]byte, len(rd.body), min(max(n, 2*cap(rd.body)), MaxBody))
		copy(grown, rd.body)
		rd.body = grown
	}
	if rd.bodyLine {
		rd.body = append(rd.body, '\n')
	}
	rd.body = append(rd.body, raw...)
	rd.bodyLine = true
}

func (rd *Reader) setOverflow() {
	rd.overflow = true
	rd.body = nil
}

// closeBlock ends the open block with its terminal line, handing the body's
// bytes to the caller.
func (rd *Reader) closeBlock(kind Kind) Line {
	l := Line{Kind: kind, Args: []string{rd.begin[0]}, Flags: rd.flags, Data: rd.body}
	if rd.overflow {
		l.Kind, l.Data, l.Err = Error, []byte(ErrReplyTooLarge.Error()), ErrReplyTooLarge
	}
	rd.open, rd.begin, rd.flags = false, [3]string{}, 0
	rd.body, rd.bodyLine, rd.overflow, rd.heldExit = nil, false, false, nil
	return l
}

// readLine returns the next line without its "\n" or one trailing "\r", as
// bufio.ScanLines does; a final unterminated line is still a line. A line
// longer than MaxLine is consumed to its newline and reported as tooLong with
// no data. The slice is valid only until the next call. ok is false once the
// underlying reader has returned any error.
func (rd *Reader) readLine() (line []byte, tooLong, ok bool) {
	if rd.ended {
		return nil, false, false
	}
	rd.long = rd.long[:0]
	for {
		chunk, err := rd.br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if !tooLong && len(rd.long)+len(chunk) <= MaxLine {
				rd.long = append(rd.long, chunk...)
			} else {
				tooLong = true
				rd.long = rd.long[:0]
			}
			continue
		}
		if err != nil {
			rd.ended = true
			if !tooLong && len(rd.long)+len(chunk) == 0 {
				return nil, false, false
			}
		} else {
			chunk = chunk[:len(chunk)-1]
		}
		if tooLong || len(rd.long)+len(chunk) > MaxLine {
			return nil, true, true
		}
		if len(rd.long) > 0 {
			rd.long = append(rd.long, chunk...)
			chunk = rd.long
		}
		return bytes.TrimSuffix(chunk, []byte{'\r'}), false, true
	}
}
