package project

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestParseTailscaleStatus(t *testing.T) {
	got, ok := ParseTailscaleStatus([]byte(readFixture(t, "tailscale-status.json")))
	want := []Peer{
		{Name: "cabin", Online: true, OS: "linux"},
		{Name: "lab", Online: true, OS: "linux"},
		{Name: "mbp", Online: false, OS: "macOS"},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseTailscaleStatus = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestParseTailscaleStatusNoData(t *testing.T) {
	for name, in := range map[string]string{
		"needs login": `{"BackendState":"NeedsLogin","Peer":{"k":{"DNSName":"lab.example.ts.net.","OS":"linux","Online":true}}}`,
		"stopped":     `{"BackendState":"Stopped","Peer":{"k":{"DNSName":"lab.example.ts.net.","OS":"linux","Online":true}}}`,
		"no state":    `{"Peer":{"k":{"DNSName":"lab.example.ts.net.","OS":"linux","Online":true}}}`,
		"bad json":    `{"BackendState":"Running","Peer":`,
		"empty":       ``,
	} {
		if got, ok := ParseTailscaleStatus([]byte(in)); ok || got != nil {
			t.Errorf("%s: ParseTailscaleStatus = %+v, %v; want nil, false", name, got, ok)
		}
	}
}

func TestParseTailscaleStatusRunningWithoutPeers(t *testing.T) {
	got, ok := ParseTailscaleStatus([]byte(`{"BackendState":"Running"}`))
	if !ok || len(got) != 0 {
		t.Errorf("ParseTailscaleStatus = %+v, %v; want no peers, true", got, ok)
	}
}

func TestTailscalePeersDisabledRunsNothing(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "tailscale-status.json")}
	if got := TailscalePeers(context.Background(), false, r); got != nil {
		t.Errorf("TailscalePeers(disabled) = %+v, want nil", got)
	}
	if len(r.calls) != 0 {
		t.Errorf("disabled ran %q", r.calls)
	}
}

func TestTailscalePeersArgv(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "tailscale-status.json")}
	got := TailscalePeers(context.Background(), true, r)
	if len(got) != 3 {
		t.Errorf("TailscalePeers = %+v, want 3 peers", got)
	}
	want := [][]string{{"tailscale", "status", "--json"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}
}

func TestTailscalePeersFailureIsNoSource(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "tailscale-status.json"), err: errors.New("failed to connect to local tailscaled")}
	if got := TailscalePeers(context.Background(), true, r); got != nil {
		t.Errorf("runner error: TailscalePeers = %+v, want nil", got)
	}
	r = &fakeRunner{stdout: `{"BackendState":"Stopped"}`}
	if got := TailscalePeers(context.Background(), true, r); got != nil {
		t.Errorf("stopped: TailscalePeers = %+v, want nil", got)
	}
}

func TestTailscalePeersTimesOut(t *testing.T) {
	// A hung daemon call ends at the deadline TailscalePeers sets itself, not the caller's.
	r := &deadlineRunner{}
	if got := TailscalePeers(context.Background(), true, r); got != nil {
		t.Errorf("TailscalePeers = %+v, want nil", got)
	}
	if !r.hadDeadline || r.remaining > TailscaleTimeout {
		t.Errorf("deadline %v (set %v), want one within %v", r.remaining, r.hadDeadline, TailscaleTimeout)
	}
}

type deadlineRunner struct {
	hadDeadline bool
	remaining   time.Duration
}

func (d *deadlineRunner) Run(ctx context.Context, _ []string) (string, string, error) {
	var dl time.Time
	dl, d.hadDeadline = ctx.Deadline()
	d.remaining = time.Until(dl)
	return "", "", context.DeadlineExceeded
}

func TestSSHHostname(t *testing.T) {
	r := &fakeRunner{stdout: "user noams\nhostname Lab.Example.ts.net\nport 22\nidentityfile ~/.ssh/id\n"}
	if got := SSHHostname(context.Background(), r, "labalias"); got != "lab" {
		t.Errorf("SSHHostname = %q, want lab", got)
	}
	if want := [][]string{{"ssh", "-G", "labalias"}}; !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}
}

func TestSSHHostnameFailures(t *testing.T) {
	r := &fakeRunner{err: errors.New("boom"), stdout: "hostname lab.example.ts.net\n"}
	if got := SSHHostname(context.Background(), r, "lab"); got != "" {
		t.Errorf("runner error: SSHHostname = %q, want empty", got)
	}
	r = &fakeRunner{stdout: "user noams\nport 22\n"}
	if got := SSHHostname(context.Background(), r, "lab"); got != "" {
		t.Errorf("no hostname line: SSHHostname = %q, want empty", got)
	}
}

func TestSSHHostnameRefusesBadAlias(t *testing.T) {
	for _, alias := range []string{"", "-oProxyCommand=x", "lab host", "lab;id", "lab\n"} {
		r := &fakeRunner{stdout: "hostname lab.example.ts.net\n"}
		if got := SSHHostname(context.Background(), r, alias); got != "" || len(r.calls) != 0 {
			t.Errorf("SSHHostname(%q) = %q, ran %q", alias, got, r.calls)
		}
	}
}

func TestMatchPeers(t *testing.T) {
	peers := []Peer{
		{Name: "cabin", Online: true},
		{Name: "lab", Online: true},
		{Name: "mbp", Online: false},
		{Name: "nas", Online: false},
	}
	hostnames := map[string]string{"work": "mbp", "lab": "lab", "home": "nas", "elsewhere": "other"}
	offline, suggestions := MatchPeers(peers, []string{"work", "lab", "home", "elsewhere", "unresolved"}, func(alias string) string {
		return hostnames[alias]
	})
	if want := []string{"work", "home"}; !reflect.DeepEqual(offline, want) {
		t.Errorf("offline = %q, want %q", offline, want)
	}
	if want := []string{"cabin"}; !reflect.DeepEqual(suggestions, want) {
		t.Errorf("suggestions = %q, want %q", suggestions, want)
	}
}

func TestMatchPeersNothingConfigured(t *testing.T) {
	peers := []Peer{{Name: "lab", Online: true}, {Name: "cabin", Online: false}}
	offline, suggestions := MatchPeers(peers, nil, func(string) string { return "" })
	if len(offline) != 0 {
		t.Errorf("offline = %q, want none", offline)
	}
	if want := []string{"cabin", "lab"}; !reflect.DeepEqual(suggestions, want) {
		t.Errorf("suggestions = %q, want %q", suggestions, want)
	}
}
