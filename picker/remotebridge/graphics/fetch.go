package graphics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// remoteFetch runs on the remote host. It prints "<mtime> <size>" first, then
// the bytes — but only when the caller's cached key differs and the file is
// within the cap. One round trip covers stat, cache validation and transfer,
// and a cache hit transfers nothing but the header.
//
// $1 path, $2 the caller's cached "<mtime> <size>" (empty if none), $3 max bytes.
const remoteFetch = `p=$1; k=$2; m=$3
s=$(stat -c '%Y %s' -- "$p" 2>/dev/null || stat -f '%m %z' -- "$p") || exit 1
printf '%s\n' "$s"
[ "$s" = "$k" ] && exit 0
sz=${s#* }
[ "$sz" -gt "$m" ] && exit 3
exec cat -- "$p"`

// cachePrune is the total-bytes ceiling for the fetch cache, and how often
// (in fetches) it is enforced.
const (
	cacheCap      = 256 << 20
	pruneInterval = 64
)

// SSHFetcher localises a remote path by copying it into a local cache over the
// daemon's ssh ControlMaster socket, so no fetch pays a new handshake and image
// bytes never share the control stream with live terminal output.
type SSHFetcher struct {
	Host string
	// CtlSock is read fresh inside fetch, once per ssh call — never cached at
	// construction — because a Proxy (and the *SSHFetcher it holds) can be
	// built before a transport replacement, and it must never keep dialling
	// through a ControlPath a later replacement has already closed (R9). A
	// nil func, like the old empty-string field, means "no -S": every caller
	// that never wires a real transport (the --test-local/-ssh "" branch)
	// leaves this nil rather than supplying func() string { return "" }.
	CtlSock  func() string
	CacheDir string
	MaxBytes int64
	// Run executes ssh; injected so tests never touch the network. It takes the
	// context so production can use exec.CommandContext — cancellation has to
	// kill the ssh process, not merely stop waiting on it.
	Run func(ctx context.Context, args ...string) ([]byte, error)

	// mu guards the maps and the counter — never held across Run, or one slow
	// fetch would serialize every concurrent one behind it (#556).
	mu       sync.Mutex
	keys     map[string]string // remote path -> last seen "<mtime> <size>"
	locals   map[string]string // "<path>\x00<key>" -> local file
	inflight map[string]*fetchCall
	fetches  int
}

// fetchCall dedups concurrent fetches of the same remote path: the first
// caller runs the ssh round-trip, the rest wait on done for its outcome.
type fetchCall struct {
	done  chan struct{}
	local string
	err   error
}

// NewSSHFetcher builds the production fetcher. ctlSock is an accessor rather
// than a value so a proxy wired up before a transport replacement keeps
// tracking the live ControlPath instead of a snapshot taken at construction.
func NewSSHFetcher(host string, ctlSock func() string, cacheDir string, maxBytes int64) *SSHFetcher {
	f := &SSHFetcher{Host: host, CtlSock: ctlSock, CacheDir: cacheDir, MaxBytes: maxBytes}
	f.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "ssh", args...).Output() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	}
	_ = os.MkdirAll(cacheDir, 0o700)
	f.prune()
	return f
}

func (f *SSHFetcher) Localize(ctx context.Context, remote string) (string, error) {
	f.mu.Lock()
	if f.keys == nil {
		f.keys, f.locals, f.inflight = map[string]string{}, map[string]string{}, map[string]*fetchCall{}
	}
	if c, ok := f.inflight[remote]; ok {
		f.mu.Unlock()
		return waitFetch(ctx, c)
	}
	c := &fetchCall{done: make(chan struct{})}
	f.inflight[remote] = c
	key := f.keys[remote]
	f.mu.Unlock()

	// The fetch outlives the caller's deadline on purpose (#558): a store
	// dropped to a timed-out fetch self-heals on the sender's next repaint,
	// and that repaint hits a warm cache only if this transfer ran to
	// completion — otherwise every retry on a slow link pays the full stream
	// timeout and the image never resolves. The detached context is still
	// bounded, so a dead link's ssh cannot live forever.
	go func() { //nolint:gosec // fetch deliberately outlives the caller (#558) and is bounded by bgFetchTimeout
		fetchCtx, cancel := context.WithTimeout(context.Background(), bgFetchTimeout)
		defer cancel()
		local, err := f.fetch(fetchCtx, remote, key)
		f.mu.Lock()
		delete(f.inflight, remote)
		c.local, c.err = local, err
		close(c.done)
		f.mu.Unlock()
	}()
	return waitFetch(ctx, c)
}

// waitFetch parks the caller on the in-flight fetch until it completes or the
// caller's own deadline passes — in which case the fetch runs on in the
// background and only the caller gives up.
func waitFetch(ctx context.Context, c *fetchCall) (string, error) {
	select {
	case <-c.done:
		return c.local, c.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// bgFetchTimeout bounds a fetch that outlived its caller's deadline. D4's
// budget governs how long the pane's stream may be held; once the caller has
// given up, the transfer's only remaining job is warming the cache, and the
// bound exists so a hung ssh still dies.
const bgFetchTimeout = 60 * time.Second

// maxConcurrentFetches bounds one batch's parallel ssh channels over the
// ControlMaster — enough to collapse a carousel's re-transmit storm into one
// round-trip's latency, not so many that the remote forks a shell storm.
const maxConcurrentFetches = 8

// LocalizeBatch fetches every path concurrently under the batch's shared
// deadline. locals and errs are indexed parallel to remotes.
func (f *SSHFetcher) LocalizeBatch(ctx context.Context, remotes []string) ([]string, []error) {
	locals := make([]string, len(remotes))
	errs := make([]error, len(remotes))
	sem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup
	for i, remote := range remotes {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			locals[i], errs[i] = f.Localize(ctx, remote)
		})
	}
	wg.Wait()
	return locals, errs
}

// fetch runs one ssh round-trip for remote, never holding f.mu. key is the
// caller's cached "<mtime> <size>" (empty when never fetched); the maps are
// re-locked for the bookkeeping after the transfer.
func (f *SSHFetcher) fetch(ctx context.Context, remote, key string) (string, error) {
	if err := os.MkdirAll(f.CacheDir, 0o700); err != nil {
		return "", fmt.Errorf("fetch %s: %w", remote, err)
	}

	// remote and key are remote-derived; a backslash defeats POSIX quoting under
	// a fish login shell (cf. shell_quotable in scripts/lib-remote.sh).
	if strings.ContainsRune(remote, '\\') || strings.ContainsRune(key, '\\') {
		return "", fmt.Errorf("fetch %s: backslash in path or key", remote)
	}

	args := []string{}
	sock := ""
	if f.CtlSock != nil {
		sock = f.CtlSock()
	}
	if sock != "" {
		args = append(args, "-S", sock)
	}
	args = append(args, "-T", f.Host, "--", "sh", "-c", shQuote(remoteFetch), "_",
		shQuote(remote), shQuote(key), strconv.FormatInt(f.MaxBytes, 10))

	out, err := f.Run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", remote, err)
	}
	hdrLine, body, ok := bytes.Cut(out, []byte{'\n'})
	if !ok {
		return "", fmt.Errorf("fetch %s: no header in reply", remote)
	}
	hdr := strings.TrimSpace(string(hdrLine))
	if len(strings.Fields(hdr)) != 2 {
		return "", fmt.Errorf("fetch %s: bad header %q", remote, hdr)
	}

	ck := remote + "\x00" + hdr
	if len(body) == 0 {
		// The remote skipped the transfer because our cached key matched.
		f.mu.Lock()
		defer f.mu.Unlock()
		if local, ok := f.locals[ck]; ok {
			if _, statErr := os.Stat(local); statErr == nil {
				f.keys[remote] = hdr
				return local, nil
			}
		}
		// We claimed a copy we no longer have (daemon restart, pruned entry).
		// Forget the key, or every later call would keep asking for the same
		// no-op transfer and this path would never recover.
		delete(f.keys, remote)
		return "", fmt.Errorf("fetch %s: cached copy is gone, refetching next time", remote)
	}

	sum := sha256.Sum256([]byte(ck))
	local := filepath.Join(f.CacheDir, hex.EncodeToString(sum[:])[:32]+".bin")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		return "", fmt.Errorf("fetch %s: %w", remote, err)
	}
	f.mu.Lock()
	f.keys[remote] = hdr
	f.locals[ck] = local
	f.fetches++
	prune := f.fetches%pruneInterval == 0
	f.mu.Unlock()
	if prune {
		f.prune()
	}
	return local, nil
}

// prune drops the oldest cache entries until the directory is under cacheCap.
func (f *SSHFetcher) prune() {
	ents, err := os.ReadDir(f.CacheDir)
	if err != nil {
		return
	}
	type ent struct {
		path string
		size int64
		mod  int64
	}
	var all []ent
	var total int64
	for _, e := range ents {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		all = append(all, ent{filepath.Join(f.CacheDir, e.Name()), info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod < all[j].mod })
	for _, e := range all {
		if total <= cacheCap {
			return
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}

// shQuote single-quotes s for the remote login shell: ssh space-joins the
// post-host argv into one string the remote shell re-parses, so every argument
// has to survive that second parse intact.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
