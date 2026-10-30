package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// A remote og-open appends " <nonce>[@<client>]|<url>" records to the session
// option @og_open_url when a controller has registered itself in
// @og_open_clients (or the legacy @og_open_client); the og_open subscription
// carries the whole log here, and the controller opens each record it has not
// seen yet. A record tagged for another client is that client's to open.
const (
	openURLOpt     = "@og_open_url"
	openClientOpt  = "@og_open_client"
	openClientsOpt = "@og_open_clients"
	openURLMaxLen  = 4096
)

// The value is written by the remote, and anything with its tmux socket can
// bypass og-open's own cap, so the controller bounds what a log may cost it.
const (
	openValueMaxLen   = 3 * openURLMaxLen
	openClientsMaxLen = 1024
	openMaxPrunes     = 16
	openMaxPerReport  = 16
	openBurst         = 30
	openRefill        = 2 * time.Second
	openNoticeEvery   = 10 * time.Second
	openOversized     = "!oversized"
)

// openURLFormat is the log bounded remote-side: tmux sends option values raw,
// so the size cap has to run where the value lives. The test is "fits",
// not "too big", so a tmux that cannot evaluate e|<= reads every value as
// oversized — the feature off, never unbounded.
var openURLFormat = "#{?#{e|<=:#{n:" + openURLOpt + "}," + strconv.Itoa(openValueMaxLen) + "},#{" + openURLOpt + "}," + openOversized + "}"

// openerWaitBound is how long a launched opener's exit status still counts.
// xdg-open in generic mode, or a first browser launch, can block for the
// browser's whole lifetime, and a crash hours later is not a failed open.
const openerWaitBound = 5 * time.Second

var (
	openNonceRe = regexp.MustCompile(`^[0-9]+-[0-9]+$`)
	openTagRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type openRecord struct{ nonce, tag, url string }

// parseOpenRecords splits a log value into its records, skipping any token
// that is not "<digits>-<digits>[@<tag>]|<url>". The cut is at the FIRST '|',
// then the nonce part at the first '@', so a pipe or '@' inside the URL
// survives.
func parseOpenRecords(v string) []openRecord {
	var recs []openRecord
	for tok := range strings.FieldsSeq(v) {
		head, u, ok := strings.Cut(tok, "|")
		if !ok {
			continue
		}
		nonce, tag, tagged := strings.Cut(head, "@")
		if !openNonceRe.MatchString(nonce) || (tagged && !openTagRe.MatchString(tag)) {
			continue
		}
		recs = append(recs, openRecord{nonce, tag, u})
	}
	return recs
}

// openClientsFormat reads the registry bounded remote-side like openURLFormat:
// the option is remote-written, and every stale name in it costs a prune
// command. A live registry is about 14 bytes per controller, so openClientsMaxLen
// leaves room for dozens. On a tmux without e|<= every register reads
// oversized and resets, degrading to one registrant like the legacy slot.
var openClientsFormat = "#{client_name}|#{?#{e|<=:#{n:" + openClientsOpt + "}," + strconv.Itoa(openClientsMaxLen) + "},#{" + openClientsOpt + "}," + openOversized + "}"

// openRegisterReadCmds read this control client's name with the session's
// registry, and every attached client; list-clients is server-wide, as a
// registered name can outlive its client on any session.
func openRegisterReadCmds(target string) []string {
	return []string{
		"display-message -p" + target + " " + tmuxQuote(ctlSafe(openClientsFormat)),
		"list-clients -F " + tmuxQuote(ctlSafe("#{client_name}")),
	}
}

// parseOpenRegister splits the two read replies into this client's name, the
// registry's entries and the attached clients' names. oversized is the remote
// reporting a registry past openClientsMaxLen, with listed nil. ok is false
// when the name could not be a tag.
func parseOpenRegister(selfAndList, clients string) (self string, listed, attached []string, oversized, ok bool) {
	self, rest, _ := strings.Cut(selfAndList, "|")
	if !openTagRe.MatchString(self) {
		return "", nil, nil, false, false
	}
	for line := range strings.SplitSeq(clients, "\n") {
		if line != "" {
			attached = append(attached, line)
		}
	}
	if rest == openOversized {
		return self, nil, attached, true, true
	}
	return self, strings.Fields(rest), attached, false, true
}

// openRegisterWriteCmds prune the registry of names no attached client holds,
// list self unless it already is, and point @og_open_client at self. A name
// outside the tag charset is never pruned: it goes into a substitution
// pattern, and cannot be a tag anyway. The registry is remote-written and the
// controller bounds what it costs: at most openMaxPrunes prunes per register
// (the rest wait for the next one; a live registry never has that many stale
// names), and an oversized one is replaced by self alone. The reset drops other
// live registrants until their next register, accepted because only a hostile
// or broken remote grows the list past the bound.
func openRegisterWriteCmds(target, self string, listed, attached []string, oversized bool) []string {
	if oversized {
		return []string{
			"set-option -F" + target + " " + openClientsOpt + " " + tmuxQuote(" #{client_name}"),
			"set-option -F" + target + " " + openClientOpt + " " + tmuxQuote("#{client_name}"),
		}
	}
	var cmds []string
	pruned := map[string]bool{}
	selfListed := false
	for _, name := range listed {
		switch {
		case name == self:
			selfListed = true
		case !openTagRe.MatchString(name) || pruned[name] || slices.Contains(attached, name) || len(pruned) >= openMaxPrunes:
		default:
			pruned[name] = true
			cmds = append(cmds, "set-option -F"+target+" "+openClientsOpt+" "+tmuxQuote("#{s/ "+name+"( |$)/ /:"+openClientsOpt+"}"))
		}
	}
	if !selfListed {
		cmds = append(cmds, "set-option -a -F"+target+" "+openClientsOpt+" "+tmuxQuote(" #{client_name}"))
	}
	return append(cmds, "set-option -F"+target+" "+openClientOpt+" "+tmuxQuote("#{client_name}"))
}

// validOpenURL is the controller-side boundary: the URL is remote-derived and
// becomes a local opener's argv, so it is re-checked here whatever og-open
// checked. The literal scheme prefix is what keeps it from ever reading as an
// opener flag; non-ASCII bytes pass (IRIs), since they can form neither a flag
// nor a separator.
func validOpenURL(u string) bool {
	if len(u) == 0 || len(u) > openURLMaxLen {
		return false
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] <= 0x20 || u[i] == 0x7f {
			return false
		}
	}
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// urlOpener opens on this machine the URLs a mirrored session's og-open
// logged. Built once per mirror run, so seen survives the reconnects that
// re-run connect.
type urlOpener struct {
	host string
	// session is the pinned remote session id. The subscription follows the
	// control client's CURRENT session, so during a session-pin excursion
	// (#396) it reports another session's log, whose records another bridge
	// owns. Empty accepts every report, as resShipper does.
	session string
	seen    map[string]bool
	// self is this control client's name, read at registration: the tag of the
	// records it opens. Empty until a register succeeds, so no tagged record
	// opens under a stale name.
	self string
	open func(string) error
	// claim reports whether this opener is the first on the machine to take a
	// record; nil takes every record.
	claim  func(host, session, nonce string) bool
	notify func(string)
	launch func(func())
	now    func() time.Time
	// spent is the token bucket's drain since refilled, so the zero value is
	// a full bucket.
	spent    int
	refilled time.Time
	// Every notice forks local tmux, and the remote decides how often one is
	// due, so at most one is in flight and one is sent per openNoticeEvery.
	// Locked because open errors notice from launched goroutines.
	noticeMu   sync.Mutex
	noticeBusy bool
	noticeNext time.Time
}

// newURLOpener returns nil when open is nil: the feature is off, and a nil
// *urlOpener's methods issue nothing.
func newURLOpener(cfg Config, session string, open func(string) error) *urlOpener {
	if open == nil {
		return nil
	}
	// notify runs on a launched goroutine, and a routing Config's hooks are
	// main-goroutine only (#808) — paster's rule.
	if cfg.plain != nil {
		cfg = *cfg.plain
	}
	return &urlOpener{
		host:    cfg.RemoteHost,
		session: session,
		seen:    map[string]bool{},
		open:    open,
		claim:   newOpenClaimer(openClaimDir(), time.Now),
		notify:  func(m string) { notifyLocal(cfg, m) },
		launch:  func(f func()) { go f() },
		now:     time.Now,
	}
}

func (o *urlOpener) target() string {
	if o.session == "" {
		return ""
	}
	return " -t " + tmuxQuote(o.session)
}

// connect seeds seen, subscribes, then registers this control client, in that
// order and each only after the last succeeded. Registering reads the
// registry, then prunes names no attached client holds, lists this client and
// points the legacy @og_open_client slot at it. tmux re-reports the value on
// subscribe, so seeding first is what keeps an attach or reconnect from
// replaying old records while still opening one appended in between; and
// registering last means og-open never sees a registered client that lacks
// the subscription. The seed reads the subscription's own bounded format;
// display-message's strftime pass touches only that template, never the
// substituted value, so a %-escape in a URL survives.
//
// A seed that fits replaces seen rather than merging into it: a nonce the log
// no longer carries cannot be replayed from it, and merging would let forced
// reconnects grow seen without bound.
func (o *urlOpener) connect(rt roundTrip) {
	if o == nil {
		return
	}
	l, ok := one(rt, "display-message -p"+o.target()+" "+tmuxQuote(ctlSafe(openURLFormat)))
	if !ok || l.Kind == controlmode.Error {
		o.fail("seed")
		return
	}
	if recs, fits := boundedOpenRecords(string(l.Data)); fits {
		o.seen = make(map[string]bool, len(recs))
		for _, r := range recs {
			o.seen[r.nonce] = true
		}
	}
	if !sendSubscription(rt, openSubName, "", openURLFormat) {
		o.fail("subscribe")
		return
	}
	o.register(rt)
}

// register's -F writes expand against the issuing client, which over the
// control stream is this one. A failed read writes nothing, so a stale registry
// is never edited blind.
func (o *urlOpener) register(rt roundTrip) {
	o.self = ""
	next := rt(openRegisterReadCmds(o.target())...)
	reg, okReg := next()
	cl, okCl := next()
	if !okReg || !okCl || reg.Kind == controlmode.Error || cl.Kind == controlmode.Error {
		o.fail("register")
		return
	}
	self, listed, attached, oversized, ok := parseOpenRegister(string(reg.Data), string(cl.Data))
	if !ok {
		o.fail("register")
		return
	}
	o.self = self
	writes := openRegisterWriteCmds(o.target(), self, listed, attached, oversized)
	next = rt(writes...)
	failed := false
	for range writes {
		l, ok := next()
		failed = failed || !ok || l.Kind == controlmode.Error
	}
	if failed {
		o.fail("register")
	}
}

func (o *urlOpener) fail(step string) {
	o.notice(fmt.Sprintf("og-open: URL opens on %s will not reach this machine (%s failed; subscriptions need tmux ≥ 3.2)", o.host, step))
}

func (o *urlOpener) notice(msg string) {
	now := o.now()
	o.noticeMu.Lock()
	if o.noticeBusy || now.Before(o.noticeNext) {
		o.noticeMu.Unlock()
		return
	}
	o.noticeBusy, o.noticeNext = true, now.Add(openNoticeEvery)
	o.noticeMu.Unlock()
	o.launch(func() {
		o.notify(msg)
		o.noticeMu.Lock()
		o.noticeBusy = false
		o.noticeMu.Unlock()
	})
}

// boundedOpenRecords parses a log value, or reports that it does not fit.
func boundedOpenRecords(v string) ([]openRecord, bool) {
	if v == openOversized || len(v) > openValueMaxLen {
		return nil, false
	}
	return parseOpenRecords(v), true
}

// handle opens every unseen valid record in one og_open report from session
// sess, within the rate limits, then forgets every nonce the report no longer
// carries. An oversized value is dropped whole and seen kept, so the small
// value og-open's reset leaves behind still opens.
func (o *urlOpener) handle(sess, v string) {
	if o == nil || (o.session != "" && sess != o.session) {
		return
	}
	recs, fits := boundedOpenRecords(v)
	if !fits {
		o.notice("og-open: ignored an oversized URL log from " + o.host)
		return
	}
	seen := make(map[string]bool, len(recs))
	opened, suppressed := 0, 0
	for _, r := range recs {
		fresh := !o.seen[r.nonce] && !seen[r.nonce]
		seen[r.nonce] = true
		if !fresh || (r.tag != "" && r.tag != o.self) || !validOpenURL(r.url) {
			continue
		}
		if opened == openMaxPerReport || !o.takeToken() {
			suppressed++
			continue
		}
		if o.claim != nil && !o.claim(o.host, o.session, r.nonce) {
			o.spent--
			continue
		}
		opened++
		o.launch(func() {
			if err := o.open(r.url); err != nil {
				o.notice(fmt.Sprintf("og-open: could not open %s: %v", r.url, err))
			}
		})
	}
	o.seen = seen
	if suppressed > 0 {
		o.notice(fmt.Sprintf("og-open: suppressed %d URL opens from %s (rate limit)", suppressed, o.host))
	}
}

// takeToken spends one token of a bucket holding openBurst that refills one
// per openRefill.
func (o *urlOpener) takeToken() bool {
	now := o.now()
	if o.spent > 0 {
		if n := int(now.Sub(o.refilled) / openRefill); n >= o.spent {
			o.spent = 0
		} else if n > 0 {
			o.spent -= n
			o.refilled = o.refilled.Add(time.Duration(n) * openRefill)
		}
	}
	if o.spent == openBurst {
		return false
	}
	if o.spent == 0 {
		o.refilled = now
	}
	o.spent++
	return true
}

const (
	openClaimTTL      = 24 * time.Hour
	openClaimSweepGap = time.Hour
)

// openClaimDir is the per-user directory of claim markers, shared by every
// daemon of this user on this machine.
func openClaimDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("tmux-og-open-%d", os.Getuid()))
}

// newOpenClaimer returns a claim func over dir. Every daemon mirroring one
// remote session receives the same og_open report, so a record is opened by
// whichever daemon creates its marker first (O_EXCL); another controller
// machine has its own dir and still opens. An unusable dir fails open: a
// duplicate tab beats a dropped URL. Markers older than openClaimTTL are swept
// at most once per openClaimSweepGap, by mtime.
func newOpenClaimer(dir string, now func() time.Time) func(host, session, nonce string) bool {
	var mu sync.Mutex
	var swept time.Time
	return func(host, session, nonce string) bool {
		if !openClaimDirOK(dir) {
			return true
		}
		mu.Lock()
		if now().Sub(swept) >= openClaimSweepGap {
			swept = now()
			sweepOpenClaims(dir, now().Add(-openClaimTTL))
		}
		mu.Unlock()
		sum := sha256.Sum256([]byte(host + "\x00" + session + "\x00" + nonce))
		f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:16])), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // dir is the owner-checked claim dir; the name is a hex digest
		if err != nil {
			return !errors.Is(err, os.ErrExist)
		}
		_ = f.Close()
		return true
	}
}

// openClaimDirOK creates dir 0700 if missing and accepts it only when it is a
// real directory owned by this user and closed to others: the temp dir is
// shared, and a squatter could otherwise pre-claim records to suppress opens.
func openClaimDirOK(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func sweepOpenClaims(dir string, before time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// BrowserOpener is the production Config.OpenURL for goos.
func BrowserOpener(goos string) func(string) error {
	name := browserOpenerName(goos)
	return func(u string) error { return runBounded(exec.Command(name, u), openerWaitBound) } //nolint:gosec // name comes from browserOpenerName (fixed per-OS binaries); the URL is a single argv element, no shell
}

func browserOpenerName(goos string) string {
	if goos == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func envWithout(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// runBounded starts cmd and reports its exit only if it comes within bound;
// past that it returns nil and a goroutine keeps reaping the child.
//
// BROWSER is dropped from the child's env because this machine's own tmux
// exports BROWSER=og-open: a generic-mode xdg-open would bounce the URL back
// through og-open, and on a controller that is itself mirrored, a second hop.
// Output goes to the daemon's log, never a pipe a lingering browser would
// hold open.
func runBounded(cmd *exec.Cmd, bound time.Duration) error {
	cmd.Env = envWithout(os.Environ(), "BROWSER")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(bound):
		return nil
	}
}
