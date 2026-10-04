package main

import (
	"context"
	"errors"
	"sync/atomic"
)

// killItemResult is the outcome of one target's kill attempt.
type killItemResult struct {
	item      listItem
	err       error // nil = success; classified error otherwise (same states killRemoteSessions handled)
	cancelled bool  // true when the run loop observed context.Canceled for this attempt; err is unspecified/ignored
}

type killRunOutcome int8

const (
	killRunDone killRunOutcome = iota
	killRunCancelled
)

type killResult struct {
	outcome killRunOutcome
	results []killItemResult // completed attempts, in target order, up to the cancel point
}

// killProgress announces the target about to be attempted.
type killProgress struct {
	index, total int
	item         listItem
}

// killRun supervises one batch of remote-kill attempts, forked off the Update
// goroutine, with cancel via context. Unlike attachRun, each attempt is
// already a bounded, complete round trip, so this is a plain loop.
type killRun struct {
	ctx      context.Context
	stop     context.CancelFunc
	targets  []listItem
	progress chan killProgress // lossy, buffered len(targets): one per target about to start
	done     chan struct{}
	res      killResult // valid once done is closed
	started  atomic.Bool
}

// newKillRun only allocates: the fork happens in run, off the Update path.
func newKillRun(targets []listItem) *killRun {
	ctx, stop := context.WithCancel(context.Background())
	return &killRun{
		ctx:      ctx,
		stop:     stop,
		targets:  targets,
		progress: make(chan killProgress, len(targets)),
		done:     make(chan struct{}),
	}
}

func (r *killRun) cancel() { r.stop() }

func (r *killRun) result() killResult { return r.res }

func (r *killRun) emit(p killProgress) {
	select {
	case r.progress <- p:
	default:
	}
}

func (r *killRun) finish(res killResult) {
	r.res = res
	close(r.done)
}

// run iterates targets in order; before each, a non-nil ctx.Err() stops the
// loop (killRunCancelled) without attempting it. The ssh kill derives its own
// remoteProbeTimeout deadline from ctx, so a cancel propagates into the
// in-flight ssh instead of only gating the next target.
func (r *killRun) run() {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	total := len(r.targets)
	var results []killItemResult
	for i, item := range r.targets {
		if r.ctx.Err() != nil {
			r.finish(killResult{outcome: killRunCancelled, results: results})
			return
		}
		r.emit(killProgress{index: i, total: total, item: item})
		var err error
		if item.remoteWindowID != "" {
			err = sshKillRemoteWindowCtx(r.ctx, item.remoteHost, item.remoteSessionID, item.remoteWindowID)
		} else {
			err = sshKillRemoteSessionCtx(r.ctx, item.remoteHost, item.remoteSess)
		}
		cancelled := err != nil && errors.Is(r.ctx.Err(), context.Canceled)
		results = append(results, killItemResult{item: item, err: err, cancelled: cancelled})
	}
	r.finish(killResult{outcome: killRunDone, results: results})
}
