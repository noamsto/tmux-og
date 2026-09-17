package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// Clipboard image paste (#361). A local ctrl+v reaches an agent as the single
// byte 0x16; the agent then reads the system clipboard ITSELF. In a mirror
// pane that read happens on the remote, where the local image does not exist
// — so the daemon (the only component that is both local and holding the ssh
// ControlMaster) intercepts the byte here: read the local clipboard image,
// ship it over the bridge's own socket, and send-keys the remote temp path
// into the pane. Claude Code resolves an existing image path in the prompt
// into an attachment at submit (verified live, see the design spec), so the
// agent receives the image exactly as if the paste had been local.
//
// The TARGETS probe (which decides forward-vs-swallow) is synchronous on
// pumpInput's goroutine; everything after that — extracting the bytes,
// uploading, injecting the path — runs async so a slow link or a wedged
// clipboard owner never freezes the pane's input.

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
// This is a usability heuristic, not a security boundary: @bridge_proc is
// daemon-sanitized but remote-derived, like every other @bridge_* field, and
// trusted the same way. A hostile remote that stamped @bridge_proc=claude on
// a pane it doesn't actually run claude in would need code execution in the
// foreground of the specific pane the user is already mirroring to exfiltrate
// anything — a stronger foothold than the exfil buys.
var pasteAgentProcs = map[string]bool{"claude": true, "pi": true}

// pastePathRe validates the path the remote store script prints before it is
// interpolated anywhere. The script is ours, but the reply crosses ssh and a
// remote shell, so it is treated as untrusted. The directory carries a
// per-invocation random suffix (mktemp -d) rather than a fixed shared path,
// so nothing can be pre-created ahead of a paste.
var pastePathRe = regexp.MustCompile(`^/tmp/og-paste-[A-Za-z0-9]+/img\.(png|jpe?g|gif|webp)$`)

// pasteExtRe gates the extension before it is interpolated into the remote
// mktemp template.
var pasteExtRe = regexp.MustCompile(`^(png|jpe?g|gif|webp)$`)

// bracketedPasteBegin/End are hoisted to package scope so splitPasteDrops's
// per-byte scan does zero allocation: converting a string constant to []byte
// inside the loop (even a short one) copied it on every non-marker byte,
// making the scan quadratic.
var (
	bracketedPasteBegin = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

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
	upload func(ctx context.Context, ext string, data []byte) (string, error)
	// probeClipboard lists the local clipboard's TARGETS and returns how to
	// extract the best image on offer; ok is false when the clipboard holds
	// no image (or no clipboard tool exists), err only when a tool listed a
	// target and then malfunctioned probing it.
	probeClipboard func() (probe clipboardProbe, ok bool, err error)
	// procFor reports the remote pane's foreground command (@bridge_proc).
	procFor func(remotePane string) string
	// notify shows a message on the mirror session's client.
	notify func(msg string)
	// sendCtl injects a command and reports whether it was written, so a
	// dropped injection (e.g. mid-reconnect) is a visible failure rather than
	// a silently discarded ssh keystroke.
	sendCtl func(cmds ...string) bool

	// mu serializes one pane's pastes against its own later input frames.
	// handle locks it for every frame and, when a frame triggers a paste,
	// hands the held lock to the paste goroutine rather than unlocking —
	// legal in Go, since sync.Mutex has no goroutine affinity. That pins
	// every later frame (including a same-burst Enter) behind the pending
	// upload+inject, so a prompt can never submit imageless while the path
	// is still in flight.
	mu sync.Mutex
	// inPaste is the bracketed-paste scan state, carried across frames: a
	// paste spanning more than one 4096-byte pty read must not lose track of
	// being inside ESC[200~...ESC[201~ at the frame boundary.
	inPaste bool
}

// paster builds the handler for one pumpInput, or nil when the Config cannot
// ship files (tests, --test-local): a nil handler forwards input verbatim.
func (c Config) paster() *pasteHandler {
	if c.PasteUpload == nil {
		return nil
	}
	return &pasteHandler{
		upload:         c.PasteUpload,
		probeClipboard: probeClipboardImage,
		procFor:        func(remotePane string) string { return bridgeProc(c, remotePane) },
		notify:         func(msg string) { notifyLocal(c, msg) },
		sendCtl:        c.SendCtl,
	}
}

// handle returns the bytes to forward to the remote pane. A 0x16 outside a
// bracketed paste on an agent pane with an image on the clipboard is
// swallowed; the goroutine it starts sends everything for this pane from
// here on — the rest of this frame's kept bytes, then the upload+injection —
// so ordering against whatever the user types next is preserved. Every other
// case forwards the payload untouched, so text paste, quoted-insert and empty
// clipboards behave exactly as they did before this interception existed.
//
// mu is held for the duration of the call, or — when a paste is triggered —
// handed off to the paste goroutine, so a later frame on this same pane
// cannot race ahead of a pending upload.
func (h *pasteHandler) handle(remotePane string, payload []byte) []byte {
	h.mu.Lock()
	kept, drops := h.splitPasteDrops(payload)
	if drops == 0 {
		h.mu.Unlock()
		return payload
	}
	if !pasteAgentProcs[h.procFor(remotePane)] {
		h.mu.Unlock()
		return payload
	}
	probe, ok, err := h.probeClipboard()
	switch {
	case err != nil:
		h.notify("tmux-og: clipboard image read failed: " + err.Error())
		h.mu.Unlock()
		return kept
	case !ok:
		h.mu.Unlock()
		return payload
	default:
		if drops > 1 {
			h.notify(fmt.Sprintf("tmux-og: ignored %d extra clipboard-paste keystroke(s) in one burst", drops-1))
		}
		go h.paste(remotePane, kept, probe)
		return nil
	}
}

// paste ships one clipboard image to the remote and injects the resulting
// path into the pane's prompt. It owns mu (handed off by handle) for its
// whole run, so nothing typed on this pane after the triggering frame can
// reach the remote ahead of it. Every failure is a visible no-op: the byte
// was already swallowed, so nothing reaching for the remote's (empty)
// clipboard runs in its place and lies about what happened.
func (h *pasteHandler) paste(remotePane string, kept []byte, probe clipboardProbe) {
	defer h.mu.Unlock()
	if len(kept) > 0 {
		h.sendChunks(remotePane, kept)
	}
	// Claude Code's path-inlining regex excludes bmp, and no converter is
	// guaranteed on either host — report rather than ship a dead path. (pi's
	// read tool would take a bmp; the gate set shares one policy.)
	if !pasteExtRe.MatchString(probe.ext) {
		h.notify("tmux-og: clipboard image format not pasteable (." + probe.ext + "; copy as png)")
		return
	}
	data, err := probe.extract()
	if err != nil {
		h.notify("tmux-og: clipboard image read failed: " + err.Error())
		return
	}
	if int64(len(data)) > pasteMaxBytes {
		h.notify(fmt.Sprintf("tmux-og: clipboard image too large (cap %d MiB)", pasteMaxBytes>>20))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pasteTimeout)
	defer cancel()
	path, err := h.upload(ctx, probe.ext, data)
	if err != nil {
		h.notify("tmux-og: image upload to remote failed: " + err.Error())
		return
	}
	if !pastePathRe.MatchString(path) {
		h.notify("tmux-og: remote returned an unexpected paste path")
		return
	}
	// The trailing space keeps whatever the user types next from merging into
	// the path token, which would silently stop the agent recognising it.
	h.sendChunks(remotePane, append([]byte(path), ' '))
}

// sendChunks hex-send-keys payload to remotePane, sidestepping every quoting
// layer between here and the pane. A refused chunk (the bridge reconnecting,
// or the remote pane having closed) is a visible notify, not a silent no-op —
// the rest of payload is abandoned, since the pane's input is already out of
// order at that point.
func (h *pasteHandler) sendChunks(remotePane string, payload []byte) {
	for _, args := range controlmode.SendKeysArgs(remotePane, payload, controlmode.InputChunkBytes) {
		if !h.sendCtl(strings.Join(args, " ")) {
			h.notify("tmux-og: input to the mirror pane was dropped (bridge reconnecting?)")
			return
		}
	}
}

// splitPasteDrops copies payload without its ctrl+v bytes (0x16), counting
// the drops. A 0x16 inside a bracketed-paste bracket (ESC[200~ … ESC[201~)
// is pasted content, not the gesture, and is kept. h.inPaste carries the
// scan state across calls (frames), since a paste can span more than one
// 4096-byte pty read: a lost ESC[201~ pins it true, which forwards every
// later byte on this pane rather than intercepting — the safe direction to
// fail in.
func (h *pasteHandler) splitPasteDrops(payload []byte) (kept []byte, drops int) {
	kept = make([]byte, 0, len(payload))
	for i := 0; i < len(payload); {
		switch {
		case !h.inPaste && bytes.HasPrefix(payload[i:], bracketedPasteBegin):
			h.inPaste = true
			kept = append(kept, bracketedPasteBegin...)
			i += len(bracketedPasteBegin)
		case h.inPaste && bytes.HasPrefix(payload[i:], bracketedPasteEnd):
			h.inPaste = false
			kept = append(kept, bracketedPasteEnd...)
			i += len(bracketedPasteEnd)
		default:
			if payload[i] == 0x16 && !h.inPaste {
				drops++
			} else {
				kept = append(kept, payload[i])
			}
			i++
		}
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
// own preference order (xclip, then wl-paste).
type clipboardTool struct {
	name       string
	listArgs   []string
	extractArg func(target string) []string
}

var clipboardTools = []clipboardTool{
	{"xclip",
		[]string{"-selection", "clipboard", "-t", "TARGETS", "-o"},
		func(t string) []string { return []string{"-selection", "clipboard", "-t", t, "-o"} }},
	{"wl-paste",
		[]string{"-l"},
		func(t string) []string { return []string{"--type", t} }},
}

// probeClipboardImage lists the local clipboard's TARGETS with the same tools
// Claude Code itself uses and picks the tool+target that offers an image. A
// missing tool, a missing display, or a clipboard with no image target is
// ok=false — the caller forwards the keypress and nothing about today's
// behaviour changes. It does not read the image bytes: that happens in
// probe.extract, called only from the async paste goroutine, so a wedged
// clipboard owner costs this synchronous call at most clipTimeout per tool
// rather than blocking on the extract too.
func probeClipboardImage() (probe clipboardProbe, ok bool, err error) {
	for _, tool := range clipboardTools {
		if _, lookErr := exec.LookPath(tool.name); lookErr != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
		out, listErr := exec.CommandContext(ctx, tool.name, tool.listArgs...).Output()
		cancel()
		if listErr != nil {
			continue
		}
		target, ext := imageTarget(strings.Split(string(out), "\n"))
		if target == "" {
			continue
		}
		return clipboardProbe{ext: ext, extract: extractClipboardImage(tool, target)}, true, nil
	}
	return clipboardProbe{}, false, nil
}

// extractClipboardImage builds the bounded, async-only read of one clipboard
// target: a LimitReader one byte past pasteMaxBytes so an oversize clipboard
// is caught without ever buffering it whole (exec.Cmd.Output's unbounded
// bytes.Buffer was the finding-4 gap), and its own clipTimeout so a wedged
// selection owner cannot hang the goroutine indefinitely.
func extractClipboardImage(tool clipboardTool, target string) func() ([]byte, error) {
	return func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, tool.name, tool.extractArg(target)...)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(pipe, pasteMaxBytes+1))
		waitErr := cmd.Wait()
		if readErr != nil {
			return nil, fmt.Errorf("%s extract %s: %w", tool.name, target, readErr)
		}
		if waitErr != nil {
			return nil, fmt.Errorf("%s extract %s: %w", tool.name, target, waitErr)
		}
		return data, nil
	}
}

// bridgeProc reads the mirror pane's @bridge_proc — the REMOTE pane's
// foreground command, stamped by the agent shipper because the local pane
// only ever runs a renderer. One fork per ctrl+v: the gesture is rare, so
// freshness beats caching. clipTimeout bounds the input pump's WAIT for this
// call, not the underlying LocalTmuxOut exec itself (its signature carries no
// context, so a genuinely wedged local tmux server leaks the goroutine and its
// child process rather than being killed) — still strictly better than
// blocking the pump indefinitely, which is what ran here before.
func bridgeProc(cfg Config, remotePane string) string {
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := cfg.LocalTmuxOut("list-panes", "-s", "-t", cfg.LocalSess, "-F", "#{@bridge_pane}|#{@bridge_proc}")
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return ""
		}
		for _, line := range strings.Split(strings.TrimSpace(r.out), "\n") {
			pane, proc, found := strings.Cut(line, "|")
			if found && pane == remotePane {
				return proc
			}
		}
		return ""
	case <-time.After(clipTimeout):
		return ""
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
	out, err := cfg.LocalTmuxOut("list-clients", "-t", cfg.LocalSess, "-F", "#{client_name}")
	if err != nil {
		return false
	}
	client, _, _ := strings.Cut(string(out), "\n")
	if client == "" {
		return false
	}
	esc := strings.NewReplacer("#", "##", "%", "%%").Replace(msg)
	cfg.LocalTmux("display-message", "-c", client, "-d", "5000", "--", esc)
	return true
}
