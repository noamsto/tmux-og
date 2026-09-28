package daemon

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// A remote og-open appends " <nonce>|<url>" records to the session option
// @og_open_url when a controller has registered itself in @og_open_client;
// the og_open subscription carries the whole log here, and the controller
// opens each record it has not seen yet.
const (
	openURLOpt    = "@og_open_url"
	openClientOpt = "@og_open_client"
	openURLMaxLen = 4096
)

// The value is written by the remote, and anything with its tmux socket can
// bypass og-open's own cap, so the controller bounds what a log may cost it.
const (
	openValueMaxLen  = 3 * openURLMaxLen
	openMaxPerReport = 16
	openBurst        = 30
	openRefill       = 2 * time.Second
	openNoticeEvery  = 10 * time.Second
	openOversized    = "!oversized"
)

// openURLFormat is the log bounded remote-side: tmux sends option values raw,
// so the controller's reader would buffer an unbounded value whole before any
// Go-side check. The test is "fits", not "too big", so a tmux that cannot
// evaluate e|<= reads every value as oversized — the feature off, never
// unbounded.
var openURLFormat = "#{?#{e|<=:#{n:" + openURLOpt + "}," + strconv.Itoa(openValueMaxLen) + "},#{" + openURLOpt + "}," + openOversized + "}"

// openerWaitBound is how long a launched opener's exit status still counts.
// xdg-open in generic mode, or a first browser launch, can block for the
// browser's whole lifetime, and a crash hours later is not a failed open.
const openerWaitBound = 5 * time.Second

var openNonceRe = regexp.MustCompile(`^[0-9]+-[0-9]+$`)

type openRecord struct{ nonce, url string }

// parseOpenRecords splits a log value into its records, skipping any token
// that is not "<digits>-<digits>|<url>". The cut is at the FIRST '|', so a
// pipe inside the URL survives.
func parseOpenRecords(v string) []openRecord {
	var recs []openRecord
	for _, tok := range strings.Fields(v) {
		nonce, u, ok := strings.Cut(tok, "|")
		if !ok || !openNonceRe.MatchString(nonce) {
			continue
		}
		recs = append(recs, openRecord{nonce, u})
	}
	return recs
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
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
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
	open    func(string) error
	notify  func(string)
	launch  func(func())
	now     func() time.Time
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
// order and each only after the last succeeded. tmux re-reports the value on
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
	l, ok := one(rt, "display-message -p"+o.target()+" "+tmuxQuote(openURLFormat))
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
	// -F expands against the issuing client, which over the control stream
	// is this one.
	l, ok = one(rt, "set-option -F"+o.target()+" "+openClientOpt+" "+tmuxQuote("#{client_name}"))
	if !ok || l.Kind == controlmode.Error {
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
		if !fresh || !validOpenURL(r.url) {
			continue
		}
		if opened == openMaxPerReport || !o.takeToken() {
			suppressed++
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

// BrowserOpener is the production Config.OpenURL for goos.
func BrowserOpener(goos string) func(string) error {
	name := browserOpenerName(goos)
	return func(u string) error { return runBounded(exec.Command(name, u), openerWaitBound) }
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
