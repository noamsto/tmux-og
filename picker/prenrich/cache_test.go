package main

import "testing"

func TestSha1Key(t *testing.T) {
	cases := map[string]string{
		"a|b": "9abe6de24a871364bf412a1c301698b5ed30dbb7",
		"url|https://github.com/acme/widgets/pull/42": "1b1e7cdb3c607ea7e4721df4566ad3f9f7f7d504",
	}
	for in, want := range cases {
		if got := sha1Key(in); got != want {
			t.Errorf("sha1Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSrvSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/sockA,1,0", ".srv_tmp_sockA"},
		{"/tmp/sock A,1,0", ".srv_tmp_sock_A"},
		{"/tmp/a.b-c_d,9,1", ".srv_tmp_a.b-c_d"},
		{"", ""},
	}
	for _, c := range cases {
		if got := srvSuffix(c.in); got != c.want {
			t.Errorf("srvSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCacheDecision(t *testing.T) {
	const (
		ttl      = 60
		ttlNone  = 15
		terminal = 3600
	)
	cases := []struct {
		name     string
		force    bool
		exists   bool
		content  string
		age      int64
		terminal int64
		want     string
	}{
		{"missing cache always fetches", false, false, "", 0, 0, "fetch"},
		{"force fetches even with a fresh cache", true, true, `[{"number":42}]`, 1, 0, "fetch"},
		{"fresh real-PR cache is served", false, true, `[{"number":42}]`, 30, 0, "serve"},
		{"stale real-PR cache fetches on normal TTL", false, true, `[{"number":42}]`, 61, 0, "fetch"},
		{"empty-array cache uses the shorter TTL_NONE", false, true, `[]`, 20, 0, "fetch"},
		{"fresh empty-array cache is served within TTL_NONE", false, true, `[]`, 5, 0, "serve"},
		{"merged PR is served under TTL_TERMINAL", false, true, `[{"number":42,"state":"MERGED","title":"x"}]`, 61, terminal, "serve"},
		{"closed PR is served under TTL_TERMINAL", false, true, `[{"number":42,"state":"CLOSED","title":"x"}]`, 61, terminal, "serve"},
		{"terminal cache still expires past TTL_TERMINAL", false, true, `[{"number":42,"state":"MERGED","title":"x"}]`, 3601, terminal, "fetch"},
		{"open PR ignores TTL_TERMINAL", false, true, `[{"number":42,"state":"OPEN","title":"x"}]`, 61, terminal, "fetch"},
		{"escaped state literal in a title is not terminal", false, true, `[{"number":42,"state":"OPEN","title":"say \"state\":\"MERGED\" loudly"}]`, 61, terminal, "fetch"},
		{"malformed JSON uses ttl: fresh", false, true, `not json`, 30, terminal, "serve"},
		{"malformed JSON uses ttl: stale", false, true, `not json`, 61, terminal, "fetch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cacheDecision(c.force, c.exists, c.content, c.age, ttl, ttlNone, c.terminal); got != c.want {
				t.Fatalf("cacheDecision = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPendingClocks(t *testing.T) {
	cases := []struct{ in, check, slow, fast int64 }{
		{300, 30, 60, 300},
		{20, 20, 20, 20},
		{45, 30, 45, 45},
	}
	for _, c := range cases {
		check, slow, fast := pendingClocks(c.in)
		if check != c.check || slow != c.slow || fast != c.fast {
			t.Errorf("pendingClocks(%d) = %d,%d,%d want %d,%d,%d", c.in, check, slow, fast, c.check, c.slow, c.fast)
		}
	}
}
