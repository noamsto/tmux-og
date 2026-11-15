package remoteopen

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	usageLine     = "usage: og-remote-open <host> <sess> --into <local-session> [--window <selector>]"
	needsBoth     = "--into needs both a remote session and a local session name"
	errPrefix     = "og-remote-open: "
	windowBadFmt  = "--window must be @<id> or @<option>=<value>, got: "
	envCombineMsg = "--into cannot be combined with OG_REMOTE_NEW_DIR or OG_REMOTE_RESTORE"
)

func wantUsage(t *testing.T, err error, msg string) {
	t.Helper()
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("err = %v (%T), want *UsageError %q", err, err, msg)
	}
	if got := usageErr.Error(); got != errPrefix+msg {
		t.Fatalf("err = %q, want %q", got, errPrefix+msg)
	}
}

func TestParseArgsRefusals(t *testing.T) {
	bads := []string{"@a=$(touch x)", "@a=#{pid}", "@a=b c", "@a=b;c", "@a=b'c", "@01", "@=x", "@a=", "x=y", ""}
	type tc struct {
		name string
		args []string
		env  Env
		msg  string
	}
	cases := []tc{
		{"A1 no args", nil, Env{}, "usage: og-remote-open <host> [<sess> [<win>]]"},
		{"A2 h --into", []string{"h", "--into"}, Env{}, needsBoth},
		{"A3 window without into", []string{"h", "s", "1", "--window", "x"}, Env{}, "--window needs --into"},
		{"A3 window at pos 3", []string{"h", "s", "--window", "x"}, Env{}, "--window needs --into"},
		{"A3 window at pos 3 only", []string{"h", "s", "--window"}, Env{}, "--window needs --into"},
		{"A4 window, into not at 3/4", []string{"h", "s", "--window", "@1", "--into", "x"}, Env{}, usageLine},
		{"A4 into at 5 after junk", []string{"h", "s", "1", "2", "--into", "--window"}, Env{}, usageLine},
		{"A5 into at pos 4", []string{"h", "s", "1", "--into", "x"}, Env{}, "--into takes no window argument"},
		{"A5 into at pos 4 bare", []string{"h", "s", "1", "--into"}, Env{}, "--into takes no window argument"},
		{"A6 extra arg", []string{"h", "s", "--into", "x", "y"}, Env{}, usageLine},
		{"A6 five args window last", []string{"h", "s", "--into", "x", "--window"}, Env{}, usageLine},
		{"A6 four+window missing selector", []string{"h", "s", "--into", "x", "--window"}, Env{}, usageLine},
		{"A6 bad flag", []string{"h", "s", "--into", "x", "--nope", "@1"}, Env{}, usageLine},
		{"A6 seven args", []string{"h", "s", "--into", "x", "--window", "@1", "z"}, Env{}, usageLine},
		{"A7 restore env", []string{"h", "s", "--into", "x"}, Env{Restore: "1"}, envCombineMsg},
		{"A7 new-dir env", []string{"h", "s", "--into", "x"}, Env{NewDir: "/d"}, envCombineMsg},
		{"A8 empty sess", []string{"h", "", "--into", "x"}, Env{}, needsBoth},
		{"A8 empty into name", []string{"h", "s", "--into", ""}, Env{}, needsBoth},
		{"A8 missing into name", []string{"h", "s", "--into"}, Env{}, needsBoth},
		{"A8 empty host not A2", []string{"", "", "--into", "x"}, Env{}, needsBoth},
		{"A9 pipe host", []string{"bad|host", "s", "--into", "x"}, Env{}, "--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: bad|host"},
		{"A9 dash host", []string{"-x", "s", "--into", "x"}, Env{}, "--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: -x"},
		{"A9 empty host", []string{"", "s", "--into", "x"}, Env{}, "--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: "},
		{"A9 newline host", []string{"a\n", "s", "--into", "x"}, Env{}, "--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: a\n"},
	}
	for _, bad := range bads {
		cases = append(cases, tc{"A10 " + bad, []string{"h", "s", "--into", "x", "--window", bad}, Env{}, windowBadFmt + bashQuote(bad)})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseArgs(c.args, c.env)
			wantUsage(t, err, c.msg)
		})
	}
}

func TestParseArgsOrder(t *testing.T) {
	// A2 before A3/A9: sess is --into even with a bad host.
	_, err := ParseArgs([]string{"bad|host", "--into", "--window"}, Env{})
	wantUsage(t, err, needsBoth)
	// A7 before A8 and A9.
	_, err = ParseArgs([]string{"bad|host", "", "--into", "x"}, Env{Restore: "1"})
	wantUsage(t, err, envCombineMsg)
	// A8 before A9.
	_, err = ParseArgs([]string{"bad|host", "s", "--into", ""}, Env{})
	wantUsage(t, err, needsBoth)
	// A9 before A10.
	_, err = ParseArgs([]string{"bad|host", "s", "--into", "x", "--window", "zzz"}, Env{})
	wantUsage(t, err, "--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: bad|host")
	// A3 before A5: --window without --into never reaches the A5 check.
	_, err = ParseArgs([]string{"h", "s", "1", "--window"}, Env{})
	wantUsage(t, err, "--window needs --into")
}

func TestParseArgsAccepted(t *testing.T) {
	env := Env{}
	cases := []struct {
		name string
		args []string
		env  Env
		want Opts
	}{
		{"host only", []string{"h"}, env, Opts{Host: "h"}},
		{"sess", []string{"h", "s"}, env, Opts{Host: "h", Sess: "s"}},
		{"index", []string{"h", "s", "3"}, env, Opts{Host: "h", Sess: "s", Win: "3", WantMode: true}},
		{"at id", []string{"h", "s", "@5"}, env, Opts{Host: "h", Sess: "s", Win: "@5", WantMode: true}},
		{"junk ignored", []string{"h", "s", "3", "junk"}, env, Opts{Host: "h", Sess: "s", Win: "3", WantMode: true}},
		{"empty sess then win", []string{"h", "", "3"}, env, Opts{Host: "h", Win: "3"}},
		{"bad win survives parse", []string{"h", "s", "abc"}, env, Opts{Host: "h", Sess: "s", Win: "abc", WantMode: true}},
		{"new-dir no want", []string{"h", "s", "3"}, Env{NewDir: "/d"}, Opts{Host: "h", Sess: "s", Win: "3", NewDir: "/d"}},
		{"restore no want", []string{"h", "s", "@3"}, Env{Restore: "1", Tmpdir: "/t"}, Opts{Host: "h", Sess: "s", Win: "@3", Restore: "1", Tmpdir: "/t"}},
		{"placed", []string{"h", "s", "--into", "x"}, env, Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x"}},
		{"placed tmpdir", []string{"h", "s", "--into", "x"}, Env{Tmpdir: "/t"}, Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x", Tmpdir: "/t"}},
		{"placed window id", []string{"h", "s", "--into", "x", "--window", "@3"}, env, Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x", Selector: "@3"}},
		{"placed window @0", []string{"h", "s", "--into", "x", "--window", "@0"}, env, Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x", Selector: "@0"}},
		{"placed window option", []string{"h", "s", "--into", "x", "--window", "@crew_branch=B"}, env, Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x", Selector: "@crew_branch=B"}},
		{"placed option charset", []string{"h.a@b-c_1", "s", "--into", "x", "--window", "@_a-1=a.b/c@d+e:f-"}, env, Opts{Host: "h.a@b-c_1", Sess: "s", Placed: true, IntoName: "x", Selector: "@_a-1=a.b/c@d+e:f-"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseArgs(c.args, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseArgsSelectorLimits(t *testing.T) {
	name64 := "a" + strings.Repeat("b", 63)
	val255 := strings.Repeat("v", 255)
	for _, sel := range []string{"@" + name64 + "=" + val255} {
		if _, err := ParseArgs([]string{"h", "s", "--into", "x", "--window", sel}, Env{}); err != nil {
			t.Fatalf("%q: %v", sel, err)
		}
	}
	for _, sel := range []string{"@a" + name64 + "=v", "@a=" + val255 + "v"} {
		_, err := ParseArgs([]string{"h", "s", "--into", "x", "--window", sel}, Env{})
		wantUsage(t, err, windowBadFmt+bashQuote(sel))
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		o    Opts
		msg  string // empty = accepted
	}{
		{"ok plain", Opts{Host: "h", Sess: "s", Win: "3", WantMode: true}, ""},
		{"ok at id", Opts{Host: "h", Sess: "s", Win: "@5", WantMode: true}, ""},
		{"ok no win", Opts{Host: "h", Sess: "s"}, ""},
		{"ok tmpdir", Opts{Host: "h", Sess: "s", Tmpdir: "/run/user/1"}, ""},
		{"A11", Opts{Host: "h", Sess: "s", Win: "abc", WantMode: true}, "window must be a numeric index or an @id, got: abc"},
		{"A11 bare at", Opts{Host: "h", Sess: "s", Win: "@", WantMode: true}, "window must be a numeric index or an @id, got: @"},
		{"A11 before A13", Opts{Host: "h", Sess: "s", Win: "@x"}, "window must be a numeric index or an @id, got: @x"},
		{"A13 no sess", Opts{Host: "h", Win: "@5"}, "a window @id needs a live session to resolve against (not with OG_REMOTE_RESTORE, OG_REMOTE_NEW_DIR, or no session): @5"},
		{"A13 restore", Opts{Host: "h", Sess: "s", Win: "@5", Restore: "1"}, "a window @id needs a live session to resolve against (not with OG_REMOTE_RESTORE, OG_REMOTE_NEW_DIR, or no session): @5"},
		{"A13 new-dir", Opts{Host: "h", Sess: "s", Win: "@5", NewDir: "/d"}, "a window @id needs a live session to resolve against (not with OG_REMOTE_RESTORE, OG_REMOTE_NEW_DIR, or no session): @5"},
		{"A14", Opts{Host: "h", Sess: "s", NewDir: "/d", Restore: "1"}, "OG_REMOTE_NEW_DIR and OG_REMOTE_RESTORE are mutually exclusive"},
		{"A14 before A15", Opts{Host: "h", Sess: "s", NewDir: `/d\`, Restore: "1"}, "OG_REMOTE_NEW_DIR and OG_REMOTE_RESTORE are mutually exclusive"},
		{"A15", Opts{Host: "h", Sess: "s", NewDir: `/a\b`}, `OG_REMOTE_NEW_DIR contains a backslash, which no remote shell dialect can quote safely: /a\b`},
		{"A15 before A16", Opts{Host: "h", Sess: "s\x01", NewDir: `\`}, `OG_REMOTE_NEW_DIR contains a backslash, which no remote shell dialect can quote safely: \`},
		{"A16", Opts{Host: "h", Sess: "a\nb"}, "session name contains a control character, which no tmux session name can: $'a\\nb' — pass an explicit session name instead"},
		{"A16 del", Opts{Host: "h", Sess: "a\x7f"}, "session name contains a control character, which no tmux session name can: $'a\\177' — pass an explicit session name instead"},
		{"A17", Opts{Host: "h", Sess: `a\b`}, `session name contains a backslash, which no remote shell dialect can quote safely: a\b — pass an explicit session name instead`},
		{"A16 before A17", Opts{Host: "h", Sess: "\\\x01"}, "session name contains a control character, which no tmux session name can: $'\\\\\\001' — pass an explicit session name instead"},
		{"A18", Opts{Host: "h", Sess: "s", Tmpdir: "rel"}, "unusable remote tmpdir: rel"},
		{"A18 charset", Opts{Host: "h", Sess: "s", Tmpdir: "/a b"}, "unusable remote tmpdir: /a b"},
		{"A17 before A18", Opts{Host: "h", Sess: `\`, Tmpdir: "x"}, `session name contains a backslash, which no remote shell dialect can quote safely: \ — pass an explicit session name instead`},
		{"placed skips A11/A13", Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x"}, ""},
		{"placed still A17", Opts{Host: "h", Sess: `a\b`, Placed: true, IntoName: "x"}, `session name contains a backslash, which no remote shell dialect can quote safely: a\b — pass an explicit session name instead`},
		{"placed still A18", Opts{Host: "h", Sess: "s", Placed: true, IntoName: "x", Tmpdir: "bad"}, "unusable remote tmpdir: bad"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWin(c.o)
			if err == nil {
				err = Validate(c.o)
			}
			if c.msg == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantUsage(t, err, c.msg)
		})
	}
}

// The launcher runs the @og_bridge_predict check between the two, so a bad
// window is validateWin's alone.
func TestValidateLeavesWindowToValidateWin(t *testing.T) {
	o := Opts{Host: "h", Sess: "s", Win: "abc", WantMode: true}
	if err := Validate(o); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
	wantUsage(t, validateWin(o), "window must be a numeric index or an @id, got: abc")
}

func TestWantModeFalseUnderEnv(t *testing.T) {
	for _, env := range []Env{{NewDir: "/d"}, {Restore: "1"}} {
		o, err := ParseArgs([]string{"h", "s", "3"}, env)
		if err != nil {
			t.Fatal(err)
		}
		if o.WantMode {
			t.Fatalf("WantMode true under %+v", env)
		}
	}
}
