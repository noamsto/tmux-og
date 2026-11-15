package main

import (
	"strconv"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

type mockPR struct {
	number, state, check, title, url, mergeable, draft, review, autoMerge, progress string
}

type opts struct {
	mode                string // tick | tickrun | tickrunpending | mock
	target, branch      string
	dir, url            string
	force               bool
	mock                mockPR
	refreshSeconds      int64
	checkRefreshSeconds int64
	reflow, notify      string
	libLog              string
	icons               enrichstate.Icons
	// cfgArgs is the config-flag subset of argv, re-passed verbatim to detached re-execs.
	cfgArgs []string
}

// parseArgs mirrors the bash argv loop: a later mode flag wins, unknown tokens
// are skipped, and a value flag with no value is ignored.
func parseArgs(argv []string) opts {
	o := opts{mode: "tick"}
	for i := 0; i < len(argv); i++ {
		flag := argv[i]
		switch flag {
		case "--tick":
			o.mode = "tick"
			continue
		case "--tick-run":
			o.mode = "tickrun"
			continue
		case "--tick-run-pending":
			o.mode = "tickrunpending"
			continue
		case "--force":
			o.force = true
			continue
		}
		rest := argv[i+1:]
		if len(rest) == 0 {
			continue
		}
		if o.setValue(flag, rest[0]) {
			i++
		}
	}
	return o
}

// setValue applies a value-taking flag and reports whether it consumed the value.
func (o *opts) setValue(flag, val string) bool {
	switch flag {
	case "--target":
		o.target = val
	case "--branch":
		o.branch = val
	case "--dir":
		o.dir = val
	case "--url":
		o.url = val
	case "--mock-pr-number":
		o.mock.number = val
		o.mode = "mock"
	case "--mock-pr-state":
		o.mock.state = val
	case "--mock-check-state":
		o.mock.check = val
	case "--mock-pr-title":
		o.mock.title = val
	case "--mock-pr-url":
		o.mock.url = val
	case "--mock-mergeable":
		o.mock.mergeable = val
	case "--mock-draft":
		o.mock.draft = val
	case "--mock-review":
		o.mock.review = val
	case "--mock-auto-merge":
		o.mock.autoMerge = val
	case "--mock-check-progress":
		o.mock.progress = val
	default:
		return o.setConfig(flag, val)
	}
	return true
}

func (o *opts) setConfig(flag, val string) bool {
	switch flag {
	case "--refresh-seconds":
		o.refreshSeconds, _ = strconv.ParseInt(val, 10, 64)
	case "--check-refresh-seconds":
		o.checkRefreshSeconds, _ = strconv.ParseInt(val, 10, 64)
	case "--reflow":
		o.reflow = val
	case "--notify":
		o.notify = val
	case "--lib-log":
		o.libLog = val
	case "--icon-pending":
		o.icons.Pending = val
	case "--icon-success":
		o.icons.Success = val
	case "--icon-failure":
		o.icons.Failure = val
	case "--icon-merged":
		o.icons.Merged = val
	case "--icon-closed":
		o.icons.Closed = val
	case "--icon-conflict":
		o.icons.Conflict = val
	case "--icon-draft":
		o.icons.Draft = val
	default:
		return false
	}
	o.cfgArgs = append(o.cfgArgs, flag, val)
	return true
}
