package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// Clipboard image paste (#361). A local ctrl+v reaches an agent as the single
// byte 0x16; the agent then reads the system clipboard ITSELF. In a mirror
// pane that read happens on the remote, where the local image does not exist
// — so the daemon (the only component that is both local and holding the ssh
// ControlMaster) intercepts the byte here: read the local clipboard image,
// pick a fresh remote path, send-keys it into the pane, and ship the image to
// it over the bridge's own socket. Claude Code resolves an existing image path
// in the prompt into an attachment at submit (verified live, see the design
// spec), so the agent receives the image exactly as if the paste had been
// local; whatever could submit the prompt waits until the upload returns.
//
// The TARGETS probe (which decides forward-vs-swallow) is synchronous on
// pumpInput's goroutine; extracting the bytes and uploading them run async so
// a slow link or a wedged clipboard owner never freezes the pane's input, and
// every send to the pane — the path included — stays on the pump goroutine.

const (
	// pasteMaxBytes mirrors the graphics fetcher's gfx-max-bytes default.
	pasteMaxBytes = 8 << 20
	// pasteTimeout bounds the whole upload. It is async, so it can be more
	// generous than the graphics fetcher's 2s without freezing anything, but
	// it stays bounded so a hung socket leaks no goroutine.
	pasteTimeout = 5 * time.Second
	// clipTimeout bounds one clipboard probe/extract: xclip asks the X
	// selection's owner for the data, and a wedged owner would otherwise
	// block the input pump's goroutine. It also bounds the @bridge_proc gate
	// lookup (bridgeProc), the other unbounded fork on that same goroutine.
	clipTimeout = 2 * time.Second
)

// pasteAgentProcs is the gate set: remote foreground commands whose ctrl+v
// means "attach the clipboard image". claude and pi are verified (codex has no
// clipboard read at all; cursor-agent is unmeasured). A pane running anything
// else gets the byte forwarded, preserving readline quoted-insert.
//
// A wrapper script that keeps bash foreground and runs the agent as a child
// reports bash here; the daemon's shipper resolves such a pane's @bridge_proc
// to the agent it finds in the remote process tree (#956), so this set is
// matched against the resolved agent, not the raw foreground command.
//
// This is a usability heuristic, not a security boundary: @bridge_proc is
// daemon-sanitized but remote-derived, like every other @bridge_* field, and
// trusted the same way. A hostile remote that stamped @bridge_proc=claude on
// a pane it doesn't actually run claude in would need code execution in the
// foreground of the specific pane the user is already mirroring to exfiltrate
// anything — a stronger foothold than the exfil buys.
var pasteAgentProcs = map[string]bool{"claude": true, "pi": true}

// pastePathRe validates the remote path newPastePath picks before it is sent
// into the pane or handed to the upload, whose remote store script checks it
// again. The directory name is fresh crypto/rand output rather than a fixed
// shared path, and the store script creates it with mkdir, which fails on an
// existing one, so nothing can be pre-created ahead of a paste.
var pastePathRe = regexp.MustCompile(`^/tmp/og-paste-[A-Za-z0-9]+/img\.(png|jpe?g|gif|webp)$`)

// pasteExtRe gates the extension before it becomes part of the remote path.
var pasteExtRe = regexp.MustCompile(`^(png|jpe?g|gif|webp)$`)

// newPastePath picks the remote path one paste is stored at:
// /tmp/og-paste-<32 hex chars>/img.<ext>. Choosing it locally is what lets
// the path go into the prompt before the upload returns.
func newPastePath(ext string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return "/tmp/og-paste-" + hex.EncodeToString(b[:]) + "/img." + ext
}

// pasteDroppedMsg reports a send to the pane the bridge refused.
const pasteDroppedMsg = "tmux-og: input to the mirror pane was dropped (bridge reconnecting?)"

// bracketedPasteBegin/End are package-level so splitPasteDrops's per-byte
// scan indexes them without allocating.
var (
	bracketedPasteBegin = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

// pasteImageTypeRe gates a clipboard target before it is quoted in a notify:
// the TARGETS listing is whatever the selection owner offers.
var pasteImageTypeRe = regexp.MustCompile(`^image/[A-Za-z0-9.+-]+$`)

// pasteForwardedMsg prefixes every notify for a ctrl+v that reached the
// remote pane untouched although the pane might be an agent.
const pasteForwardedMsg = "tmux-og: ctrl+v forwarded to the remote: "

// clipboardProbe is the result of listing the local clipboard's TARGETS: the
// image type on offer (if any) and how to fetch it. Built synchronously —
// TARGETS decides forward-vs-swallow, so it has to run on the input pump —
// but extract is called only from the async paste goroutine, so a slow read
// never blocks it.
type clipboardProbe struct {
	ext     string
	extract func() ([]byte, error)
}

// pasteHandler intercepts ctrl+v on agent panes. Every side effect is an
// injected field so tests never touch ssh, tmux, or a clipboard. One handler
// is built per pumpInput (see paster), so its fields are effectively
// per-pane state.
type pasteHandler struct {
	// upload stores data at the remote path and returns once it is there or
	// failed to be.
	upload func(ctx context.Context, path string, data []byte) error
	// newPath picks the remote path for an image of type ext.
	newPath func(ext string) string
	// probeClipboard lists the local clipboard's TARGETS and returns how to
	// extract the best image on offer. ok=false with a nil err means a listing
	// succeeded and offered no image (text), so the byte is forwarded silently;
	// a non-nil err means the probe could not tell, or the image is unusable,
	// and the byte is forwarded with a notify.
	probeClipboard func() (probe clipboardProbe, ok bool, err error)
	// procFor reports the remote pane's resolved command (@bridge_proc: the
	// foreground command, or the agent found in the pane's process tree when the
	// foreground is not itself an agent — #956); an error means the lookup could
	// not say, which is not the same as "not an agent".
	procFor func(remotePane string) (string, error)
	// notify shows a message on the mirror session's client.
	notify func(msg string)
	// sendCtl injects a command and reports whether it was written, so a
	// dropped injection (e.g. mid-reconnect) is a visible failure rather than
	// a silently discarded ssh keystroke.
	sendCtl func(cmds ...string) bool
	// clientFor names the local client that pasted (the mirror session's most
	// recently active one), or "" when none is attached. status shows msg on
	// it for ttl. Either nil leaves a paste without progress, and every message
	// falls back to notify.
	clientFor func() string
	status    func(client, msg string, ttl time.Duration) error
	// tick is the progress refresh period; zero means pasteTick.
	tick time.Duration

	// phase is where the in-flight paste stands. Only the pump goroutine
	// touches it, the fields down to holdAll and the scan state below; the
	// paste goroutine hands its results back through done.
	phase pastePhase
	// path is the in-flight paste's remote path, from its ready result.
	path string
	// inserted: the path and its space went into the prompt whole.
	inserted bool
	// passed: a frame passed through to the pane after the path went in, so
	// erasing len(path)+1 bytes could eat the user's own edit.
	passed bool
	// insertRefused: the bridge refused the path send.
	insertRefused bool
	// held is one entry per input frame, so a replayed lone Escape stays a lone
	// Escape instead of fusing with the text after it into an Alt chord.
	held [][]byte
	// firstDrop is the index in splitPasteDrops's kept of its first dropped
	// ctrl+v: bytes before it were typed before the gesture and are never held.
	firstDrop int
	// holdAll holds every byte, not only the submitting suffix, for a paste
	// whose triggering frame ended inside a bracketed paste or a partial marker.
	holdAll bool
	// done carries the paste goroutine's results — a ready, then a final — to
	// the pump goroutine, which owns every send to this pane so input and the
	// path keep one order. It buffers both, so the paste goroutine never
	// waits on the pump.
	done chan pasteResult
	// inPaste is the bracketed-paste scan state, carried across frames: a
	// paste spanning more than one 4096-byte pty read must not lose track of
	// being inside ESC[200~...ESC[201~ at the frame boundary.
	inPaste bool
	// matched is how many bytes of the awaited marker (the end marker while
	// inPaste, else the begin marker) ended the previous payload. A marker can
	// itself straddle two payloads: pumpInput flushes an incomplete escape
	// carry on its own once escCarryGrace passes.
	matched int
}

// paster builds the handler for one pumpInput, or nil when the Config cannot
// ship files (tests, --test-local): a nil handler forwards input verbatim.
func (c Config) paster() *pasteHandler {
	// A routing Config's hooks run through routeWhile, which is main-goroutine
	// only; the handler below is used from its own goroutine, so it needs the
	// original, unwrapped hooks (#808).
	if c.plain != nil {
		c = *c.plain
	}
	if c.PasteUpload == nil {
		return nil
	}
	return &pasteHandler{
		done:           make(chan pasteResult, 2),
		upload:         c.PasteUpload,
		newPath:        newPastePath,
		probeClipboard: probeClipboardImage,
		procFor:        func(remotePane string) (string, error) { return bridgeProc(c, remotePane) },
		notify:         func(msg string) { notifyLocal(c, msg) },
		sendCtl:        c.SendCtl,
		clientFor:      func() string { return activeClient(c) },
		status:         func(client, msg string, ttl time.Duration) error { return showOnClient(c, client, msg, ttl) },
	}
}

// pastePhase is where an in-flight paste stands, as the pump goroutine sees it.
type pastePhase uint8

const (
	// pasteIdle: no paste in flight; input is only scanned for ctrl+v.
	pasteIdle pastePhase = iota
	// pasteReading: the bytes are being extracted and the path is not in the
	// prompt yet, so only plain text passes (splitFree): anything that could
	// move the cursor would move the point the path goes in at.
	pasteReading
	// pasteUploading: the path is in the prompt and the upload runs, so only
	// what could submit it, or is not classified as safe, waits (submitHold).
	pasteUploading
)

// pumping reports whether a paste is in flight.
func (h *pasteHandler) pumping() bool { return h.phase != pasteIdle }

// handle returns the bytes to forward to the remote pane. A 0x16 outside a
// bracketed paste on an agent pane with an image on the clipboard is
// swallowed and starts the paste on its own goroutine. Typing is not held
// back wholesale while it runs: until the path is in the prompt plain text is
// forwarded at once (it lands before the path), and once it is, cursor and
// edit keys pass too (they act on a buffer that already holds the path).
// What is held — in order, with everything after it — is any byte that could
// submit the prompt before the image exists, or move the insertion point
// before the path goes in. Every other case forwards the payload untouched; a
// forward on a pane that might be an agent notifies why (gate lookup failed,
// probe couldn't tell, unsupported image type), except a text clipboard and a
// non-agent proc, which stay silent as they did before this interception
// existed.
//
// handle and settle run on the pane's pump goroutine only, so they need no
// lock and a returned prefix is sent in order with settle's sends.
func (h *pasteHandler) handle(remotePane string, payload []byte) []byte {
	switch h.phase {
	case pasteReading:
		free, rest := h.splitFree(payload)
		h.hold(rest)
		return free
	case pasteUploading:
		if h.holdAll || len(h.held) > 0 {
			h.hold(payload)
			return nil
		}
		// A passed frame skips splitPasteDrops, and the final's replay
		// resumes the scan from the state the swallow left. That is sound:
		// paste markers, lone ESC, incomplete escapes and ^V are always
		// held, and every sequence submitHold passes ends on a byte that
		// leaves matched at 0, so scanning it would change nothing.
		n := submitHold(payload)
		if n > 0 {
			h.passed = true
		}
		h.hold(payload[n:])
		return payload[:n]
	case pasteIdle:
	}
	kept, drops := h.splitPasteDrops(payload)
	if drops == 0 {
		return payload
	}
	proc, err := h.procFor(remotePane)
	if err != nil {
		// Async: notifyLocal forks the local tmux, which a gate timeout
		// suggests is wedged, and this runs on the input pump.
		go h.notify(pasteForwardedMsg + err.Error())
		return payload
	}
	if !pasteAgentProcs[proc] {
		return payload
	}
	probe, ok, err := h.probeClipboard()
	switch {
	case err != nil:
		go h.notify(pasteForwardedMsg + err.Error())
		return payload
	case !ok:
		return payload
	}
	if drops > 1 {
		h.notify(fmt.Sprintf("tmux-og: ignored %d extra clipboard-paste keystroke(s) in one burst", drops-1))
	}
	h.phase = pasteReading
	h.path, h.inserted, h.passed, h.insertRefused = "", false, false, false
	h.holdAll = h.inPaste || h.matched != 0
	go h.paste(probe)
	free, rest := h.splitFree(kept[h.firstDrop:])
	h.held = nil
	h.hold(rest)
	return append(kept[:h.firstDrop:h.firstDrop], free...)
}

func (h *pasteHandler) hold(b []byte) {
	if len(b) > 0 {
		h.held = append(h.held, b)
	}
}

// pasteResult is one message from a paste goroutine. A ready result names
// the remote path once the bytes are read; cancel aborts the upload behind
// it. A final ends the paste: ok reports the upload, and msg is the failure
// for the pump to show, empty when the goroutine already said why it failed.
type pasteResult struct {
	ready  bool
	path   string
	cancel context.CancelFunc
	ok     bool
	msg    string
	out    pasteOut
}

// results is the channel pumpInput selects on; nil for a nil handler, which
// blocks forever.
func (h *pasteHandler) results() <-chan pasteResult {
	if h == nil {
		return nil
	}
	return h.done
}

// settle applies one paste result on the pump goroutine and replays the
// input held so far through deliver (which calls handle, so a held ctrl+v or
// bracketed paste is intercepted like any other).
//
// A ready result inserts the path and moves to the uploading phase, so the
// replay passes what the uploading rule passes. A final ends the paste:
// a failed upload's path is erased when it went in whole and nothing passed
// after it — synchronously, so the erase reaches the pane ahead of a
// replayed Enter — then the outcome is shown, off the pump so held keys never
// wait on a tmux fork, and the held input is replayed.
func (h *pasteHandler) settle(remotePane string, r pasteResult, deliver func([]byte)) {
	held := h.held
	h.held = nil
	if r.ready {
		h.phase = pasteUploading
		h.path = r.path
		// The trailing space keeps whatever the user types next from merging
		// into the path token, which would silently stop the agent
		// recognising it. The path fits one send-keys chunk, so it goes in
		// whole or not at all; a refusal is shown by the final, and the
		// upload it would have named is pointless.
		h.inserted = h.sendChunks(remotePane, append([]byte(r.path), ' '))
		if !h.inserted {
			h.insertRefused = true
			r.cancel()
		}
		h.replay(held, deliver)
		return
	}
	h.phase = pasteIdle
	left := h.inserted && h.passed
	if !r.ok && h.inserted && !h.passed {
		left = !h.sendChunks(remotePane, bytes.Repeat([]byte{0x7f}, len(h.path)+1))
	}
	out := r.out
	switch {
	case h.insertRefused:
		go out.say(pasteDroppedMsg)
	case r.ok:
		if out.client != "" {
			go func() { _ = h.status(out.client, pasteDoneMsg, pasteDoneTTL) }()
		}
	case r.msg != "":
		msg := r.msg
		if left {
			msg += " (path left in the prompt)"
		}
		go out.say(msg)
	}
	h.replay(held, deliver)
}

// replay delivers released input frame by frame through deliver. It stops
// when a frame changes whether a paste is in flight (it started another one)
// or leaves something held (the uploading rule held it), and holds the
// frames after it behind that, keeping their order and boundaries.
func (h *pasteHandler) replay(held [][]byte, deliver func([]byte)) {
	pumping := h.pumping()
	for i, b := range held {
		deliver(b)
		if h.pumping() != pumping || len(h.held) > 0 {
			h.held = append(h.held, held[i+1:]...)
			return
		}
	}
}

// splitFree splits payload at the first byte that is not plain text. Plain
// text is printable ASCII and UTF-8: it can neither submit nor move the
// cursor. Backspace, tab, Enter, escape sequences (arrows, home/end, paste
// markers) and every other control byte start the held suffix, so the path is
// always appended where the text typed so far ends. Once anything is held,
// everything after it is held too, to keep typing in order.
func (h *pasteHandler) splitFree(payload []byte) (free, held []byte) {
	if h.holdAll || len(h.held) > 0 {
		return nil, payload
	}
	for i, c := range payload {
		if c < 0x20 || c == 0x7f {
			return payload[:i], payload[i:]
		}
	}
	return payload, nil
}

// paste reads and uploads one clipboard image. It runs off the pump
// goroutine, so a slow link never freezes the pane's input. A failure before
// the path is picked is said here and ends with a final carrying no message;
// otherwise it posts a ready result with the path, uploads, and posts the
// final, whose message the pump shows. A final always reaches done, so held
// input never waits past the upload's own bounds. Every failure is visible:
// the byte was already swallowed, so nothing reaching for the remote's
// (empty) clipboard runs in its place and lies about what happened.
func (h *pasteHandler) paste(probe clipboardProbe) {
	// Claude Code's path-inlining regex excludes bmp, and no converter is
	// guaranteed on either host — report rather than ship a dead path. (pi's
	// read tool would take a bmp; the gate set shares one policy.)
	if !pasteExtRe.MatchString(probe.ext) {
		out := h.output()
		out.say("tmux-og: clipboard image format not pasteable (." + probe.ext + "; copy as png)")
		h.done <- pasteResult{out: out}
		return
	}
	// The client lookup and the extract are both local forks; run them side
	// by side. The first progress frame needs the client, not the bytes.
	type extracted struct {
		data []byte
		err  error
	}
	got := make(chan extracted, 1)
	go func() {
		data, err := probe.extract()
		got <- extracted{data, err}
	}()
	out := h.output()
	prog := out.startProgress("reading clipboard image")
	x := <-got
	fail := func(msg string) {
		prog.stop()
		out.say(msg)
		h.done <- pasteResult{out: out}
	}
	if x.err != nil {
		fail("tmux-og: clipboard image read failed: " + x.err.Error())
		return
	}
	if int64(len(x.data)) > pasteMaxBytes {
		fail(fmt.Sprintf("tmux-og: clipboard image too large (cap %d MiB)", pasteMaxBytes>>20))
		return
	}
	path := h.newPath(probe.ext)
	if !pastePathRe.MatchString(path) {
		fail("tmux-og: refusing an unexpected paste path")
		return
	}
	prog.set(fmt.Sprintf("uploading image (%s)", formatSize(len(x.data))))
	ctx, cancel := context.WithTimeout(context.Background(), pasteTimeout)
	defer cancel()
	h.done <- pasteResult{ready: true, path: path, cancel: cancel, out: out}
	err := h.upload(ctx, path, x.data)
	prog.stop()
	r := pasteResult{ok: err == nil, out: out}
	if err != nil {
		r.msg = "tmux-og: image upload to remote failed: " + err.Error()
	}
	h.done <- r
}

// sendChunks hex-send-keys payload to remotePane, sidestepping every quoting
// layer between here and the pane. A refused chunk (the bridge reconnecting,
// or the remote pane having closed) abandons the rest of payload, since the
// pane's input is already out of order at that point. It reports whether
// every chunk was written; the caller shows a refusal.
func (h *pasteHandler) sendChunks(remotePane string, payload []byte) bool {
	for _, args := range controlmode.SendKeysArgs(remotePane, payload, controlmode.InputChunkBytes) {
		if !h.sendCtl(strings.Join(args, " ")) {
			return false
		}
	}
	return true
}

// splitPasteDrops copies payload without its ctrl+v bytes (0x16), counting
// the drops. A 0x16 inside a bracketed paste (ESC[200~ … ESC[201~) is pasted
// content, not the gesture, and is kept. h.inPaste and h.matched carry the
// scan across calls, so a paste or a marker split over several payloads is
// tracked exactly. Bytes are never held back: a partial marker is forwarded
// as it arrives and only its progress is remembered.
func (h *pasteHandler) splitPasteDrops(payload []byte) (kept []byte, drops int) {
	kept = make([]byte, 0, len(payload))
	h.firstDrop = -1
	for _, c := range payload {
		marker := bracketedPasteBegin
		if h.inPaste {
			marker = bracketedPasteEnd
		}
		switch c {
		case marker[h.matched]:
			h.matched++
			if h.matched == len(marker) {
				h.inPaste = !h.inPaste
				h.matched = 0
			}
		case 0x1b:
			// ESC occurs only at index 0 of either marker, so a mismatching ESC
			// restarts the match there.
			h.matched = 1
		default:
			h.matched = 0
		}
		if c == 0x16 && !h.inPaste {
			if drops == 0 {
				h.firstDrop = len(kept)
			}
			drops++
			continue
		}
		kept = append(kept, c)
	}
	return kept, drops
}

// imageTarget picks the best image MIME target offered by a clipboard and
// maps it to the file extension the remote temp file will carry. Preference
// order matches what an agent can inline (png first); bmp is last because it
// is reported unsupported rather than shipped.
func imageTarget(targets []string) (target, ext string) {
	pref := []struct{ mime, ext string }{
		{"image/png", "png"},
		{"image/jpeg", "jpg"},
		{"image/jpg", "jpg"},
		{"image/webp", "webp"},
		{"image/gif", "gif"},
		{"image/bmp", "bmp"},
	}
	offered := map[string]bool{}
	for _, t := range targets {
		offered[strings.TrimSpace(t)] = true
	}
	for _, p := range pref {
		if offered[p.mime] {
			return p.mime, p.ext
		}
	}
	return "", ""
}

// clipboardTool is one clipboard backend probeClipboardImage tries, in CC's
// own preference order (xclip, wl-paste, then macOS's osascript). pick maps the
// tool's raw listing to an image target; decode, when set, turns the extract's
// stdout into image bytes (osascript prints a hex literal, not raw bytes) and
// readLimit bounds that stdout.
type clipboardTool struct {
	name       string
	listArgs   []string
	extractArg func(target string) []string
	pick       func(listing string) (target, ext string)
	decode     func(out []byte) ([]byte, error)
	readLimit  int64
}

func pickMIMETarget(listing string) (target, ext string) {
	return imageTarget(strings.Split(listing, "\n"))
}

// osascriptImageClasses are the image types `clipboard info` lists, as it
// prints them: only PNG as a raw class code, the rest by terminology name.
// Extraction coerces every one to PNG, so ext is always png.
var osascriptImageClasses = []string{"«class PNGf»", "TIFF picture", "JPEG picture", "GIF picture"}

func pickOsascriptTarget(listing string) (target, ext string) {
	for _, class := range osascriptImageClasses {
		if strings.Contains(listing, class) {
			return "«class PNGf»", "png"
		}
	}
	return "", ""
}

const (
	osascriptDataPrefix = "«data PNGf"
	osascriptDataSuffix = "»"
	// osascriptReadLimit bounds the hex literal: two chars per byte plus the
	// wrapper and a trailing newline, one byte past so oversize is detectable.
	osascriptReadLimit = 2*pasteMaxBytes + 64 + 1
)

// decodeOsascriptPNG unwraps `«data PNGf<hex>»`, the form osascript prints for
// a clipboard coerced to PNG.
func decodeOsascriptPNG(out []byte) ([]byte, error) {
	if int64(len(out)) >= osascriptReadLimit {
		return nil, fmt.Errorf("image exceeds the %d MiB cap", pasteMaxBytes>>20)
	}
	body, ok := strings.CutPrefix(strings.TrimSpace(string(out)), osascriptDataPrefix)
	if !ok {
		return nil, errors.New("unexpected osascript output")
	}
	body, ok = strings.CutSuffix(body, osascriptDataSuffix)
	if !ok {
		return nil, errors.New("unexpected osascript output")
	}
	return hex.DecodeString(body)
}

var clipboardTools = []clipboardTool{
	{name: "xclip",
		listArgs:   []string{"-selection", "clipboard", "-t", "TARGETS", "-o"},
		extractArg: func(t string) []string { return []string{"-selection", "clipboard", "-t", t, "-o"} },
		pick:       pickMIMETarget, readLimit: pasteMaxBytes + 1},
	{name: "wl-paste",
		listArgs:   []string{"-l"},
		extractArg: func(t string) []string { return []string{"--type", t} },
		pick:       pickMIMETarget, readLimit: pasteMaxBytes + 1},
	{name: "osascript",
		listArgs:   []string{"-e", "clipboard info"},
		extractArg: func(t string) []string { return []string{"-e", "the clipboard as " + t} },
		pick:       pickOsascriptTarget, decode: decodeOsascriptPNG, readLimit: osascriptReadLimit},
}

// probeClipboardImage lists the local clipboard's TARGETS with the same tools
// Claude Code itself uses and picks the tool+target that offers an image. The
// result is ok=true for a pasteable image; ok=false with a nil err when a
// listing succeeded and offered none (text), which the caller forwards
// silently; and an err when no listing could say (no tool, or every listing
// failed) or the only image on offer is a type that cannot be pasted. It does
// not read the image bytes: that happens in probe.extract, called only from
// the async paste goroutine, so a wedged clipboard owner costs this
// synchronous call at most clipTimeout per tool rather than blocking on the
// extract too.
func probeClipboardImage() (probe clipboardProbe, ok bool, err error) {
	var failures []string
	var listed bool
	var unsupported string
	for _, tool := range clipboardTools {
		if _, lookErr := exec.LookPath(tool.name); lookErr != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
		out, listErr := exec.CommandContext(ctx, tool.name, tool.listArgs...).Output() //nolint:gosec // tool.name is from the fixed clipboardTools table; target comes from the clipboard's own target list, passed as argv
		timedOut := ctx.Err() != nil
		cancel()
		if listErr != nil {
			failures = append(failures, listFailure(tool.name, listErr, timedOut))
			continue
		}
		listed = true
		targets := strings.Split(string(out), "\n")
		if target, ext := tool.pick(string(out)); target != "" {
			return clipboardProbe{ext: ext, extract: extractClipboardImage(tool, target)}, true, nil
		}
		if unsupported == "" {
			unsupported = unsupportedImageTarget(targets)
		}
	}
	switch {
	case unsupported != "":
		return clipboardProbe{}, false, fmt.Errorf("clipboard image type %s is not pasteable (copy as png)", unsupported)
	case listed:
		return clipboardProbe{}, false, nil
	case len(failures) > 0:
		return clipboardProbe{}, false, errors.New(strings.Join(failures, "; "))
	default:
		return clipboardProbe{}, false, errors.New("no clipboard tool (xclip, wl-paste or osascript) on PATH")
	}
}

// listFailure describes one failed TARGETS listing for the notify: the tool's
// own first stderr line when it printed one, since that is the actionable part
// ("Nothing is copied", "Can't open display").
func listFailure(name string, err error, timedOut bool) string {
	if timedOut {
		return fmt.Sprintf("%s timed out after %s", name, clipTimeout)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return name + ": " + err.Error()
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(exitErr.Stderr)), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return name + ": " + err.Error()
	}
	return name + ": " + line
}

// unsupportedImageTarget returns the first image/* target a clipboard offers,
// or "" when none passes pasteImageTypeRe. imageTarget has already found no
// pasteable one by the time this is asked.
func unsupportedImageTarget(targets []string) string {
	for _, t := range targets {
		if t = strings.TrimSpace(t); strings.HasPrefix(t, "image/") && pasteImageTypeRe.MatchString(t) {
			return t
		}
	}
	return ""
}

// extractClipboardImage builds the bounded, async-only read of one clipboard
// target: a LimitReader one byte past the tool's cap so an oversize clipboard
// is caught without ever buffering it whole (exec.Cmd.Output's unbounded
// bytes.Buffer was the finding-4 gap), and its own clipTimeout so a wedged
// selection owner cannot hang the goroutine indefinitely.
func extractClipboardImage(tool clipboardTool, target string) func() ([]byte, error) {
	return func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, tool.name, tool.extractArg(target)...) //nolint:gosec // tool.name is from the fixed clipboardTools table; target was chosen by imageTarget from a fixed set
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(pipe, tool.readLimit))
		waitErr := cmd.Wait()
		if readErr != nil {
			return nil, fmt.Errorf("%s extract %s: %w", tool.name, target, readErr)
		}
		if waitErr != nil {
			return nil, fmt.Errorf("%s extract %s: %w", tool.name, target, waitErr)
		}
		if tool.decode != nil {
			if data, err = tool.decode(data); err != nil {
				return nil, fmt.Errorf("%s extract %s: %w", tool.name, target, err)
			}
		}
		return data, nil
	}
}

// bridgeProc reads the mirror pane's @bridge_proc — the REMOTE pane's agent
// command, resolved from its process tree when a wrapper script keeps a shell
// in the foreground (#956), else the foreground command itself. The remote
// stamps it via the agent shipper because the local pane only ever runs a
// renderer. One fork per ctrl+v: the gesture is rare, so
// freshness beats caching. clipTimeout bounds the input pump's WAIT for this
// call, not the underlying LocalTmuxOut exec itself (its signature carries no
// context, so a genuinely wedged local tmux server leaks the goroutine and its
// child process rather than being killed) — still strictly better than
// blocking the pump indefinitely, which is what ran here before.
// Every case that cannot yield a proc is an error, so the caller can tell "not
// an agent" apart from "could not look".
func bridgeProc(cfg Config, remotePane string) (string, error) {
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := cfg.LocalTmuxOut("list-panes", "-s", "-t", cfg.target(), "-F", "#{@bridge_pane}|#{@bridge_proc}|#{@bridge_sock}")
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("mirror pane lookup failed: %w", r.err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(r.out), "\n") {
			// Remote pane ids collide across daemons sharing a host session,
			// so only this daemon's windows answer. The sock goes last: a
			// socket path may hold '|'.
			f := strings.SplitN(line, "|", 3)
			if len(f) != 3 || f[0] != remotePane || f[2] != cfg.SockPath {
				continue
			}
			proc := f[1]
			if proc == "" {
				return "", fmt.Errorf("mirror pane %s has no @bridge_proc yet", remotePane)
			}
			return proc, nil
		}
		return "", fmt.Errorf("mirror pane %s not found", remotePane)
	case <-time.After(clipTimeout):
		return "", fmt.Errorf("mirror pane lookup timed out after %s", clipTimeout)
	}
}

// notifyLocal shows msg on the client viewing the mirror session and reports
// whether one was found to show it on. A detached session has no client to
// show it on, and the paste's async context has no better channel — the
// message is best-effort by construction. msg is escaped and passed after --
// per this repo's display-message convention (scripts/og-notify.sh): the
// argument is format-expanded and strftime-run, and a leading "-" would
// otherwise be read as a flag.
func notifyLocal(cfg Config, msg string) bool {
	if cfg.LocalTmuxOut == nil || cfg.LocalTmux == nil {
		return false
	}
	out, err := cfg.LocalTmuxOut("list-clients", "-t", cfg.target(), "-F", "#{client_name}")
	if err != nil {
		return false
	}
	client, _, _ := strings.Cut(string(out), "\n")
	if client == "" {
		return false
	}
	_ = showOnClient(cfg, client, msg, 5*time.Second)
	return true
}

// showOnClient displays msg on one named client for ttl. The message is
// escaped and passed after -- per this repo's display-message convention.
func showOnClient(cfg Config, client, msg string, ttl time.Duration) error {
	esc := strings.NewReplacer("#", "##", "%", "%%").Replace(msg)
	return cfg.LocalTmux("display-message", "-c", client, "-d", strconv.FormatInt(ttl.Milliseconds(), 10), "--", esc)
}
