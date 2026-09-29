package graphics

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetcherWritesBytesToCacheAndReturnsLocalPath(t *testing.T) {
	dir := t.TempDir()
	var gotArgs []string
	f := &SSHFetcher{
		Host: "g6", CtlSock: func() string { return "/run/x.sock" }, CacheDir: dir, MaxBytes: 1 << 20,
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			gotArgs = args
			return []byte("1700000000 5\nHELLO"), nil
		},
	}
	local, err := f.Localize(context.Background(), "/tmp/a.png")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(local)
	if err != nil || string(b) != "HELLO" {
		t.Fatalf("cached content = %q err=%v", b, err)
	}
	if filepath.Dir(local) != dir {
		t.Fatalf("wrote outside the cache dir: %s", local)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "-S /run/x.sock") || !strings.Contains(joined, "g6") {
		t.Fatalf("did not use the ControlMaster socket: %v", gotArgs)
	}
}

// CtlSock is read fresh inside fetch on every call, never snapshotted at
// construction: a Proxy (and the *SSHFetcher it holds) can be built before a
// transport replacement completes, so a cached value could keep dialling
// through a ControlPath a later replacement already closed (R9). An accessor
// that goes back to returning "" must omit -S exactly like the old
// empty-string field did.
func TestFetcherReadsCtlSockAtCallTimeAndFallsBackWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	sock := "/run/first.sock"
	var gotArgs []string
	f := &SSHFetcher{
		Host: "g6", CacheDir: dir, MaxBytes: 1 << 20,
		CtlSock: func() string { return sock },
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			gotArgs = args
			return []byte("1700000000 5\nHELLO"), nil
		},
	}
	if _, err := f.Localize(context.Background(), "/tmp/a.png"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "-S /run/first.sock") {
		t.Fatalf("first fetch did not use the initial socket: %v", gotArgs)
	}

	sock = "/run/second.sock"
	if _, err := f.Localize(context.Background(), "/tmp/b.png"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "-S /run/second.sock") {
		t.Fatalf("second fetch did not follow the replaced socket: %v", gotArgs)
	}

	sock = ""
	if _, err := f.Localize(context.Background(), "/tmp/c.png"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(gotArgs, " "), "-S ") {
		t.Fatalf("an accessor returning \"\" must omit -S, got: %v", gotArgs)
	}
}

// A nil CtlSock (every caller with no ssh transport at all) must behave
// exactly like the old empty-string field: no -S added.
func TestFetcherNilCtlSockOmitsDashS(t *testing.T) {
	dir := t.TempDir()
	var gotArgs []string
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("1700000000 5\nHELLO"), nil
	}}
	if _, err := f.Localize(context.Background(), "/tmp/a.png"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(gotArgs, " "), "-S ") {
		t.Fatalf("nil CtlSock must behave like the old empty string: %v", gotArgs)
	}
}

// Run's context is the fetch's kill switch: NewSSHFetcher's production Run
// wraps exec.CommandContext, and only the exec itself dying on cancel (not a
// goroutine-plus-select wrapper around it) keeps a hung ssh from running
// forever (spec D4). Since #558 that context is the fetch's own detached,
// bounded one — not the caller's, whose deadline must not kill a transfer
// that is warming the cache for the next repaint.
func TestFetcherThreadsABoundedContextToRun(t *testing.T) {
	dir := t.TempDir()
	var gotCtx context.Context
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		gotCtx = ctx
		return []byte("1700000000 5\nHELLO"), nil
	}}
	if _, err := f.Localize(context.Background(), "/tmp/a.png"); err != nil {
		t.Fatal(err)
	}
	if gotCtx == nil {
		t.Fatal("Run was never called")
	}
	deadline, ok := gotCtx.Deadline()
	if !ok {
		t.Fatal("Run's context has no deadline — a hung ssh would run forever")
	}
	if d := time.Until(deadline); d <= 0 || d > bgFetchTimeout {
		t.Fatalf("Run's deadline is %v out, want within bgFetchTimeout", d)
	}
}

// A fetch that outlives the caller's deadline completes in the background and
// warms the cache: the next call (the sender's self-heal repaint) gets the
// header-only hit instead of paying a second full transfer that would time
// out again (#558).
func TestFetcherTimedOutFetchWarmsTheCache(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var runs atomic.Int32
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		runs.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte("1700000000 5\nHELLO"), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.Localize(ctx, "/tmp/a.png"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call err = %v, want the caller's deadline", err)
	}
	// The caller gave up; the fetch is still parked in Run. Let it finish.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		done := len(f.inflight) == 0
		f.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	local, err := f.Localize(context.Background(), "/tmp/a.png")
	if err != nil {
		t.Fatalf("second call after background completion: %v", err)
	}
	if b, _ := os.ReadFile(local); string(b) != "HELLO" {
		t.Fatalf("cached content = %q", b)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("Run calls = %d, want 2 (transfer, then header-only validation)", n)
	}
}

func TestFetcherSecondCallIsAHitAndTransfersNothing(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	f := &SSHFetcher{
		Host: "g6", CacheDir: dir, MaxBytes: 1 << 20,
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			calls++
			if calls == 1 {
				return []byte("1700000000 5\nHELLO"), nil
			}
			// Same mtime+size: the remote script prints the key and exits
			// without cat-ing.
			return []byte("1700000000 5\n"), nil
		},
	}
	first, err := f.Localize(context.Background(), "/tmp/a.png")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Localize(context.Background(), "/tmp/a.png")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("cache miss on an unchanged file: %s vs %s", first, second)
	}
}

func TestFetcherTreatsAChangedMtimeAsANewFile(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	f := &SSHFetcher{
		Host: "g6", CacheDir: dir, MaxBytes: 1 << 20,
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			calls++
			if calls == 1 {
				return []byte("1700000000 1\nA"), nil
			}
			return []byte("1700000009 1\nB"), nil
		},
	}
	first, _ := f.Localize(context.Background(), "/tmp/scratch.raw")
	second, _ := f.Localize(context.Background(), "/tmp/scratch.raw")
	if first == second {
		t.Fatal("a rewritten scratch frame must not reuse the old cache entry")
	}
	b, _ := os.ReadFile(second)
	if string(b) != "B" {
		t.Fatalf("second content = %q", b)
	}
}

func TestFetcherRejectsOversizeAndBadReplies(t *testing.T) {
	dir := t.TempDir()
	// Over the cap the remote script exits 3 without cat-ing, which surfaces as
	// a non-zero ssh exit.
	over := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 4, Run: func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("exit status 3")
	}}
	if _, err := over.Localize(context.Background(), "/tmp/big.raw"); err == nil {
		t.Fatal("oversize fetch must error so the store is dropped")
	}
	bad := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(context.Context, ...string) ([]byte, error) {
		return []byte("garbage"), nil
	}}
	if _, err := bad.Localize(context.Background(), "/tmp/a.png"); err == nil {
		t.Fatal("unparsable reply must error")
	}
}

// The #556 regression net: Localize must not hold the fetcher lock across Run,
// or LocalizeBatch's goroutines would serialize behind the slowest fetch and
// the batch would cost N round-trips again.
func TestFetcherLocalizeRunsConcurrently(t *testing.T) {
	dir := t.TempDir()
	started := make(chan string, 2)
	release := make(chan struct{})
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		// The remote path is the third-to-last argument (then key, max-bytes).
		started <- args[len(args)-3]
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte("1700000000 5\nHELLO"), nil
	}}
	var wg sync.WaitGroup
	locals := make([]string, 2)
	errs := make([]error, 2)
	for i, p := range []string{"/tmp/a.png", "/tmp/b.png"} {
		wg.Go(func() {
			locals[i], errs[i] = f.Localize(context.Background(), p)
		})
	}
	// Both fetches must be in flight before either completes.
	seen := map[string]bool{<-started: true, <-started: true}
	close(release)
	wg.Wait()
	if !seen["'/tmp/a.png'"] || !seen["'/tmp/b.png'"] {
		t.Fatalf("fetches serialized or lost: %v", seen)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
	}
	if locals[0] == locals[1] {
		t.Fatalf("distinct paths cached to the same file: %s", locals[0])
	}
}

// Two concurrent fetches of the SAME path share one ssh round-trip: the
// second waits on the first's outcome rather than double-transferring.
func TestFetcherDedupsConcurrentFetchesOfOnePath(t *testing.T) {
	dir := t.TempDir()
	var runs atomic.Int32
	release := make(chan struct{})
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		runs.Add(1)
		<-release
		return []byte("1700000000 5\nHELLO"), nil
	}}
	var wg sync.WaitGroup
	locals := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			locals[i], errs[i] = f.Localize(context.Background(), "/tmp/a.png")
		})
	}
	// Let the first fetch reach Run and the second park on the inflight call.
	for runs.Load() == 0 {
		runtime.Gosched()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("Run called %d times for one path, want 1", n)
	}
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errs = %v", errs)
	}
	if locals[0] != locals[1] {
		t.Fatalf("waiters got different results: %q vs %q", locals[0], locals[1])
	}
}

func TestFetcherRecoversWhenTheCachedCopyIsGone(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	f := &SSHFetcher{Host: "g6", CacheDir: dir, MaxBytes: 1 << 20, Run: func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			return []byte("1700000000 1\n"), nil // header only: "you already have it"
		}
		return []byte("1700000000 1\nA"), nil
	}}
	local, _ := f.Localize(context.Background(), "/tmp/a.png")
	_ = os.Remove(local) // pruned, or the daemon restarted
	if _, err := f.Localize(context.Background(), "/tmp/a.png"); err == nil {
		t.Fatal("a lost cache entry must error once")
	}
	if _, err := f.Localize(context.Background(), "/tmp/a.png"); err != nil {
		t.Fatalf("and then recover by refetching, got %v", err)
	}
}

// ssh space-joins the post-host argv into ONE string that the remote login
// shell re-parses — that second parse is what shQuote has to survive, not the
// first. This drives an outer `sh -c` over the joined argv (standing in for
// the remote login shell) around an inner `sh -c "$1"` echo, exactly as
// Localize's argv shape does, so a break in either parse shows up here rather
// than only against a live remote.
func TestShQuoteSurvivesSSHsDoubleParse(t *testing.T) {
	weird := `/tmp/a "quoted" it's got spaces.png`
	args := []string{"sh", "-c", shQuote(`printf '%s' "$1"`), "_", shQuote(weird)}
	out, err := exec.Command("sh", "-c", strings.Join(args, " ")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != weird {
		t.Fatalf("round-trip = %q, want %q", out, weird)
	}
}
