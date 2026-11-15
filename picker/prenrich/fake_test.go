package main

import (
	"strings"
	"sync"
)

type call struct {
	kind string // output | run | detach
	dir  string
	name string
	args []string
}

func (c call) joined() string { return strings.Join(append([]string{c.name}, c.args...), " ") }

type rule struct {
	prefix string
	out    string
	err    error
}

// fakeRunner records every call in order and answers output from the longest
// matching prefix rule over "name arg arg...". Safe for concurrent use.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []call
	rules    []rule
	runErr   error
	binaries map[string]bool
}

func newFakeRunner(binaries ...string) *fakeRunner {
	f := &fakeRunner{binaries: map[string]bool{}}
	for _, b := range binaries {
		f.binaries[b] = true
	}
	return f
}

// on answers output calls whose "name args..." starts with prefix.
func (f *fakeRunner) on(prefix, out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{prefix, out, err})
}

func (f *fakeRunner) record(kind, dir, name string, args []string) {
	f.calls = append(f.calls, call{kind, dir, name, append([]string(nil), args...)})
}

func (f *fakeRunner) output(dir, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("output", dir, name, args)
	joined := strings.Join(append([]string{name}, args...), " ")
	best := -1
	var out string
	var err error
	for _, r := range f.rules {
		if strings.HasPrefix(joined, r.prefix) && len(r.prefix) >= best {
			best, out, err = len(r.prefix), r.out, r.err
		}
	}
	return out, err
}

func (f *fakeRunner) run(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("run", "", name, args)
	return f.runErr
}

func (f *fakeRunner) detach(name string, args ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("detach", "", name, args)
}

func (f *fakeRunner) lookPath(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.binaries[name]
}

func (f *fakeRunner) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeRunner) byName(name string) []call {
	var out []call
	for _, c := range f.snapshot() {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRunner) kind(kind string) []call {
	var out []call
	for _, c := range f.snapshot() {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}
