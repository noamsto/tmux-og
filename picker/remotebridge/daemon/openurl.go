package daemon

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// A remote og-open appends " <nonce>|<url>" records to the session option
// @og_open_url when a controller has registered itself in @og_open_client;
// the og_open subscription carries the whole log here, and the controller
// opens each record it has not seen yet.
const (
	openURLOpt    = "@og_open_url"
	openURLFormat = "#{" + openURLOpt + "}"
	openClientOpt = "@og_open_client"
	openURLMaxLen = 4096
)

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
// the subscription. show-options -v rather than display-message -p, whose
// strftime pass would rewrite a %-escape inside a URL.
func (o *urlOpener) connect(rt roundTrip) {
	if o == nil {
		return
	}
	l, ok := one(rt, "show-options -qv"+o.target()+" "+openURLOpt)
	if !ok || l.Kind == controlmode.Error {
		o.fail("seed")
		return
	}
	for _, r := range parseOpenRecords(string(l.Data)) {
		o.seen[r.nonce] = true
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
	o.launch(func() {
		o.notify(fmt.Sprintf("og-open: URL opens on %s will not reach this machine (%s failed; subscriptions need tmux ≥ 3.2)", o.host, step))
	})
}

// handle opens every unseen valid record in one og_open report from session
// sess, then forgets every nonce the report no longer carries — og-open's own
// cap reset is what bounds seen.
func (o *urlOpener) handle(sess, v string) {
	if o == nil || (o.session != "" && sess != o.session) {
		return
	}
	recs := parseOpenRecords(v)
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		fresh := !o.seen[r.nonce] && !seen[r.nonce]
		seen[r.nonce] = true
		if !fresh || !validOpenURL(r.url) {
			continue
		}
		o.launch(func() {
			if err := o.open(r.url); err != nil {
				o.notify(fmt.Sprintf("og-open: could not open %s: %v", r.url, err))
			}
		})
	}
	o.seen = seen
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
