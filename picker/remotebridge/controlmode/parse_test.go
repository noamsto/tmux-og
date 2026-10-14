package controlmode

import (
	"strings"
	"testing"
)

func TestUnescape(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{`ls /`, []byte("ls /")},
		{`ls /\015\015\012`, []byte("ls /\r\r\n")},
		{`a\134b`, []byte(`a\b`)},                  // \134 == backslash
		{`\033[0m`, []byte("\x1b[0m")},             // ESC then literal
		{`\342\230\203`, []byte{0xe2, 0x98, 0x83}}, // a UTF-8 rune as raw bytes
	}
	for _, c := range cases {
		got := Unescape([]byte(c.in))
		if string(got) != string(c.want) {
			t.Errorf("Unescape(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseLine(t *testing.T) {
	l := ParseLine(`%output %3 ab\015`)
	if l.Kind != Output || l.Pane != "%3" || string(l.Data) != "ab\r" {
		t.Fatalf("output parse wrong: %+v", l)
	}
	if ParseLine(`%begin 1700000000 1 0`).Kind != Begin {
		t.Error("begin")
	}
	if ParseLine(`%end 1700000000 1 0`).Kind != End {
		t.Error("end")
	}
	if ParseLine(`%error 1700000000 1 0`).Kind != Error {
		t.Error("error")
	}
	if ParseLine(`%window-close @5`).Kind != WindowClose {
		t.Error("window-close")
	}
	if ParseLine(`%exit`).Kind != Exit {
		t.Error("exit")
	}
	if ParseLine(`%unlinked-window-add @9`).Kind != Other {
		t.Error("unlinked should be Other")
	}
}

func TestParseM22Notifications(t *testing.T) {
	if l := ParseLine(`%window-add @7`); l.Kind != WindowAdd || len(l.Args) != 1 || l.Args[0] != "@7" {
		t.Errorf("window-add: %+v", l)
	}
	l := ParseLine(`%window-renamed @7 my long name`)
	if l.Kind != WindowRenamed || l.Args[0] != "@7" || string(l.Data) != "my long name" {
		t.Errorf("window-renamed (name must stay whole): %+v", l)
	}
	if l := ParseLine(`%session-window-changed $2 @7`); l.Kind != SessionWindowChanged || l.Args[0] != "$2" || l.Args[1] != "@7" {
		t.Errorf("session-window-changed: %+v", l)
	}
	if l := ParseLine(`%window-pane-changed @7 %12`); l.Kind != WindowPaneChanged || l.Args[0] != "@7" || l.Args[1] != "%12" {
		t.Errorf("window-pane-changed: %+v", l)
	}
	if l := ParseLine(`%pause %12`); l.Kind != Pause || l.Args[0] != "%12" {
		t.Errorf("pause: %+v", l)
	}
	if l := ParseLine(`%continue %12`); l.Kind != Continue || l.Args[0] != "%12" {
		t.Errorf("continue: %+v", l)
	}
	if ParseLine(`%unlinked-window-add @9`).Kind != Other {
		t.Error("unlinked-window-add must stay Other")
	}
}

func TestParseExtendedOutput(t *testing.T) {
	// With flow control armed (refresh-client -f pause-after=N), tmux switches
	// the output notification to %extended-output %pane <age-ms> : <escaped>.
	// It must parse to the same Output line as %output (age dropped) or live
	// output is silently dropped (#183).
	l := ParseLine(`%extended-output %3 0 : ab\015`)
	if l.Kind != Output || l.Pane != "%3" || string(l.Data) != "ab\r" {
		t.Fatalf("extended-output parse wrong: %+v", l)
	}
	// Non-zero age, and data that itself contains " : " — split only on the
	// first separator so the payload stays whole.
	l2 := ParseLine(`%extended-output %0 42 : a : b`)
	if l2.Kind != Output || l2.Pane != "%0" || string(l2.Data) != "a : b" {
		t.Fatalf("extended-output age/colon wrong: %+v", l2)
	}
}

// TestReaderNextZeroCopyOutput verifies the Reader.Next() hot path — which
// parses directly off the scanner's reused buffer via parseLine(sc.Bytes())
// — still produces a correct, fully-owned Line for a %output line.
func TestReaderNextZeroCopyOutput(t *testing.T) {
	rd := NewReader(strings.NewReader("%output %3 hello world\n"))
	l, ok := rd.Next()
	if !ok || l.Kind != Output || l.Pane != "%3" || string(l.Data) != "hello world" {
		t.Fatalf("Next() = %+v, ok=%v, want Output %%3 %q", l, ok, "hello world")
	}
}

// TestReaderNextNoBufferAliasing guards against the bug class this change
// could introduce: parseLine must never retain a slice into the scanner's
// buffer, or a later Scan() would silently corrupt an earlier Line's Data.
func TestReaderNextNoBufferAliasing(t *testing.T) {
	rd := NewReader(strings.NewReader("%output %1 first\n%output %2 second\n"))
	l1, ok := rd.Next()
	if !ok || l1.Kind != Output {
		t.Fatalf("first Next() = %+v, ok=%v", l1, ok)
	}
	l2, ok := rd.Next()
	if !ok || l2.Kind != Output {
		t.Fatalf("second Next() = %+v, ok=%v", l2, ok)
	}
	if string(l1.Data) != "first" {
		t.Errorf("l1.Data corrupted by second Next(): got %q, want %q", l1.Data, "first")
	}
	if string(l2.Data) != "second" {
		t.Errorf("l2.Data = %q, want %q", l2.Data, "second")
	}
}

// TestReaderNextNoBufferAliasingRenamedName is TestReaderNextNoBufferAliasing
// for the other aliasing-risk seam: %window-renamed and %session-changed rely
// on an explicit bytes.Clone(name) rather than an implicit fresh-alloc like
// Unescape, so a dropped Clone would silently corrupt an earlier Line's Data
// once the scanner's buffer is reused.
func TestReaderNextNoBufferAliasingRenamedName(t *testing.T) {
	rd := NewReader(strings.NewReader("%window-renamed @1 first\n%window-renamed @2 second\n"))
	l1, ok := rd.Next()
	if !ok || l1.Kind != WindowRenamed {
		t.Fatalf("first Next() = %+v, ok=%v", l1, ok)
	}
	l2, ok := rd.Next()
	if !ok || l2.Kind != WindowRenamed {
		t.Fatalf("second Next() = %+v, ok=%v", l2, ok)
	}
	if string(l1.Data) != "first" {
		t.Errorf("l1.Data corrupted by second Next(): got %q, want %q", l1.Data, "first")
	}
	if string(l2.Data) != "second" {
		t.Errorf("l2.Data = %q, want %q", l2.Data, "second")
	}
}

func TestParseSubscriptionChanged(t *testing.T) {
	// Window scope: the pane field is '-', and the value holds a ':' of its own
	// (an issue title does), which must not be mistaken for the separator.
	l := ParseLine("%subscription-changed og_labels $0 @3 2 - : @3|nova|#89b4fa|fix: a:b")
	if l.Kind != SubscriptionChanged {
		t.Fatalf("kind = %v, want SubscriptionChanged", l.Kind)
	}
	if got, want := l.Args[0], "og_labels"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := string(l.Data), "@3|nova|#89b4fa|fix: a:b"; got != want {
		t.Errorf("value = %q, want %q", got, want)
	}

	// Pane scope carries the pane id in the ids, though the subscribed format
	// repeats it in the value — which is what consumers read.
	l = ParseLine("%subscription-changed og_agents $0 @3 2 %9 : %9|claude|waiting 1700000000 1||")
	if l.Kind != SubscriptionChanged || l.Args[4] != "%9" {
		t.Errorf("pane form = %v args %v", l.Kind, l.Args)
	}

	// An emptied value still reports, because tmux writes the separator
	// unconditionally: that line is how "the option was unset" arrives, and
	// dropping it would leave the last non-empty value stamped forever.
	l = ParseLine("%subscription-changed og_labels $0 @3 2 - : ")
	if l.Kind != SubscriptionChanged || string(l.Data) != "" {
		t.Errorf("empty value = %v, data %q", l.Kind, l.Data)
	}

	// Session-scope form, where tmux fills every id with '-'.
	if l := ParseLine("%subscription-changed s $0 - - - : v"); l.Kind != SubscriptionChanged {
		t.Errorf("session form kind = %v", l.Kind)
	}

	// No separator at all is not a subscription line we can read.
	if l := ParseLine("%subscription-changed mangled"); l.Kind != Other {
		t.Errorf("separatorless kind = %v, want Other", l.Kind)
	}
}

func TestParseClientNotifications(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		kind     Kind
		args     []string
		wantData string
	}{
		{"client-detached", "%client-detached /dev/ttys004", ClientDetached, []string{"/dev/ttys004"}, ""},
		{"client-session-changed", "%client-session-changed /dev/ttys004 $3 work", ClientSessionChanged, []string{"/dev/ttys004", "$3"}, "work"},
		{"session name with spaces", "%client-session-changed /dev/ttys004 $3 my proj", ClientSessionChanged, []string{"/dev/ttys004", "$3"}, "my proj"},
		{"truncated, no id", "%client-session-changed /dev/ttys004", ClientSessionChanged, []string{"/dev/ttys004", ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := ParseLine(c.raw)
			if l.Kind != c.kind {
				t.Fatalf("kind = %v, want %v", l.Kind, c.kind)
			}
			if len(l.Args) != len(c.args) {
				t.Fatalf("args = %q, want %q", l.Args, c.args)
			}
			for i := range c.args {
				if l.Args[i] != c.args[i] {
					t.Errorf("args[%d] = %q, want %q", i, l.Args[i], c.args[i])
				}
			}
			if string(l.Data) != c.wantData {
				t.Errorf("data = %q, want %q", l.Data, c.wantData)
			}
		})
	}
}
