package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// notifySpy is a Config wired so notifyLocal succeeds, plus the messages it
// showed. One client is attached, which is notifyLocal's whole precondition.
type notifySpy struct {
	msgs []string
}

func (n *notifySpy) cfg(host string) Config {
	return Config{
		RemoteHost:   host,
		LocalSess:    host + "-work",
		LocalTmuxOut: func(...string) (string, error) { return "/dev/pts/3\n", nil },
		LocalTmux: func(args ...string) error {
			n.msgs = append(n.msgs, args[len(args)-1])
			return nil
		},
	}
}

// verdictReplies scripts one reply block per verdict, in order. An empty
// verdict is a block with no body: the pane has not stamped yet.
func verdictReplies(verdicts ...string) string {
	var b strings.Builder
	for i, v := range verdicts {
		fmt.Fprintf(&b, "%%begin %d 1 1\n", i+1)
		if v != "" {
			b.WriteString(v + "\n")
		}
		fmt.Fprintf(&b, "%%end %d 1 1\n", i+1)
	}
	return b.String()
}

// pendingCount is the probe's unanswered-press count, for the tests that
// assert it stops reading. A method here rather than in carouselprobe.go:
// nothing in production asks.
func (p *carouselProbe) pendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func sinkSend(sent *[]string) func(...string) bool {
	return func(cmds ...string) bool {
		*sent = append(*sent, cmds...)
		return true
	}
}

func TestCarouselProbeReportsNoImages(t *testing.T) {
	rt, _ := scriptedRT(verdictReplies(carouselVerdictNoImages))
	spy := &notifySpy{}
	var sent []string

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(spy.cfg("g6"), rt, sinkSend(&sent))

	if len(spy.msgs) != 1 || !strings.Contains(spy.msgs[0], "no images yet for this pane on g6") {
		t.Errorf("messages = %q, want the no-images line naming the host", spy.msgs)
	}
	// The stamp has to go, or a verdict arriving after a later press would be
	// read as that press's answer.
	if want := carouselClearCmd("%7"); len(sent) != 1 || sent[0] != want {
		t.Errorf("sent = %q, want just %q", sent, want)
	}
	if p.pendingCount() != 0 {
		t.Error("press still pending after it was answered")
	}
}

func TestCarouselProbeReportsMissingBinary(t *testing.T) {
	rt, _ := scriptedRT(verdictReplies(carouselVerdictNoBin))
	spy := &notifySpy{}
	var sent []string

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(spy.cfg("g6"), rt, sinkSend(&sent))

	if len(spy.msgs) != 1 || !strings.Contains(spy.msgs[0], "not on PATH on g6") {
		t.Errorf("messages = %q, want the missing-binary line naming the host", spy.msgs)
	}
}

// A press that launched the carousel is an answer, not a message: the viewer
// is on screen and there is nothing to say about it.
func TestCarouselProbeSaysNothingOnLaunch(t *testing.T) {
	rt, _ := scriptedRT(verdictReplies(carouselVerdictOK))
	spy := &notifySpy{}
	var sent []string

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(spy.cfg("g6"), rt, sinkSend(&sent))

	if len(spy.msgs) != 0 {
		t.Errorf("messages = %q, want none for a launched carousel", spy.msgs)
	}
	if p.pendingCount() != 0 {
		t.Error("an ok verdict must stop the probe, not keep it reading")
	}
}

// The option is remote-derived, so only the verdicts this package writes get
// through — but an unrecognised one is still an answer, so the probe stops.
func TestCarouselProbeIgnoresUnknownVerdict(t *testing.T) {
	rt, _ := scriptedRT(verdictReplies("#{q:evil} %s"))
	spy := &notifySpy{}
	var sent []string

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(spy.cfg("g6"), rt, sinkSend(&sent))

	if len(spy.msgs) != 0 {
		t.Errorf("messages = %q, want none for an unknown verdict", spy.msgs)
	}
	if p.pendingCount() != 0 {
		t.Error("press still pending after an unknown verdict")
	}
}

// The stamp rides a run-shell -b, so the first read routinely finds nothing.
// An empty read is not "no images".
func TestCarouselProbeRetriesUntilStamped(t *testing.T) {
	rt, _ := scriptedRT(verdictReplies("", carouselVerdictNoImages))
	spy := &notifySpy{}
	var sent []string
	cfg := spy.cfg("g6")

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(cfg, rt, sinkSend(&sent))
	if len(spy.msgs) != 0 {
		t.Fatalf("messages = %q after an empty read, want none yet", spy.msgs)
	}
	if p.pendingCount() != 1 {
		t.Fatal("an empty read must keep the press pending")
	}

	p.poll(cfg, rt, sinkSend(&sent))
	if len(spy.msgs) != 1 {
		t.Errorf("messages = %q once the stamp lands, want one", spy.msgs)
	}
}

func TestCarouselProbeGivesUpWithinBudget(t *testing.T) {
	empties := make([]string, carouselProbeAttempts+2)
	rt, _ := scriptedRT(verdictReplies(empties...))
	spy := &notifySpy{}
	var sent []string
	cfg := spy.cfg("g6")

	p := newCarouselProbe()
	p.arm("%7")
	for range carouselProbeAttempts {
		p.poll(cfg, rt, sinkSend(&sent))
	}

	if p.pendingCount() != 0 {
		t.Error("a press that never answers must be dropped, not polled forever")
	}
	if len(spy.msgs) != 0 {
		t.Errorf("messages = %q, want none — an unanswered press is not a no-images press", spy.msgs)
	}
	if len(sent) != 0 {
		t.Errorf("sent = %q, want none — there was no stamp to clear", sent)
	}
}

// A dead pane answers with %error, which is no answer at all: retrying it only
// draws the same error again.
func TestCarouselProbeDropsPressOnReadError(t *testing.T) {
	rt, _ := scriptedRT("%begin 1 1 1\nno such pane\n%error 1 1 1\n")

	spy := &notifySpy{}
	var sent []string

	p := newCarouselProbe()
	p.arm("%7")
	p.poll(spy.cfg("g6"), rt, sinkSend(&sent))

	if p.pendingCount() != 0 {
		t.Error("press still pending after a failed read")
	}
	if len(spy.msgs) != 0 {
		t.Errorf("messages = %q, want none for a failed read", spy.msgs)
	}
}

func TestCarouselVerdictCmdShapes(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{carouselVerdictCmd("%7"), "show-options -pqv -t %7 @og_carousel"},
		{carouselStampCmd("%7", carouselVerdictNoImages), "set-option -p -t %7 @og_carousel noimages"},
		{carouselClearCmd("%7"), "set-option -pu -t %7 @og_carousel"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// A press whose remote command was never written has no verdict coming, so
// arming for it would spend the whole budget reading an option nothing stamps.
func TestHandleCtlDoesNotArmWhenTheSendFails(t *testing.T) {
	cst, rep, _, _ := handlerFixture(t, "foot", "foot")
	p := newCarouselProbe()

	err := handleCtl(cst, rep, p, carouselPress(), "rem", nil, func(...string) bool { return false })
	if err == nil {
		t.Fatal("want an error when the command could not be written")
	}
	if p.pendingCount() != 0 {
		t.Error("armed a probe for a press that was never sent")
	}
}

// The common press arms, or nothing would ever read the ok stamp back.
func TestHandleCtlArmsTheProbeOnASubmittedPress(t *testing.T) {
	cst, rep, _, sent := handlerFixture(t, "foot", "foot")
	p := newCarouselProbe()

	if err := handleCtl(cst, rep, p, carouselPress(), "rem", nil, sender(sent)); err != nil {
		t.Fatalf("handleCtl: %v", err)
	}
	if p.pendingCount() != 1 {
		t.Errorf("pending = %d, want 1", p.pendingCount())
	}
}
