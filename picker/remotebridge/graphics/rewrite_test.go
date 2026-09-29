package graphics

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

var errFake = errors.New("fake")

type fakeLocalizer struct {
	local string
	err   error
	asked []string
}

func (f *fakeLocalizer) Localize(ctx context.Context, remote string) (string, error) {
	f.asked = append(f.asked, remote)
	return f.local, f.err
}

func seqOf(t *testing.T, raw string) *Seq {
	t.Helper()
	cs := NewScanner().Feed([]byte(raw))
	if len(cs) != 1 || cs[0].Seq == nil {
		t.Fatalf("not a single sequence: %q", raw)
	}
	return cs[0].Seq
}

func TestRewriteLocalisesFilePayload(t *testing.T) {
	f := &fakeLocalizer{local: "/local/cache/abc.bin"}
	q, drop, err := Rewrite(context.Background(), seqOf(t, bareSeq), f)
	if err != nil || drop {
		t.Fatalf("drop=%v err=%v", drop, err)
	}
	if len(f.asked) != 1 || f.asked[0] != "/tmp/x.png" {
		t.Fatalf("asked = %v, want [/tmp/x.png]", f.asked)
	}
	if q == nil {
		t.Fatal("Rewrite returned nil seq")
	}
	got, _ := base64.StdEncoding.DecodeString(string(q.Payload))
	if string(got) != "/local/cache/abc.bin" {
		t.Fatalf("payload = %q", got)
	}
	if string(q.Keys) != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("control keys mutated: %q", q.Keys)
	}
}

// t=t ("transmit, then delete the file") localises exactly as t=f does. Left
// out of the localising case it falls through to the pass-through default,
// which hands the local terminal a REMOTE path — the unlocalised store D7
// exists to forbid.
func TestRewriteLocalisesTransmitAndDelete(t *testing.T) {
	f := &fakeLocalizer{local: "/local/cache/abc.bin"}
	q, drop, err := Rewrite(context.Background(), seqOf(t, "\x1b_Gi=1,a=T,t=t;L3RtcC94LnBuZw==\x1b\\"), f)
	if err != nil || drop {
		t.Fatalf("drop=%v err=%v", drop, err)
	}
	if len(f.asked) != 1 || f.asked[0] != "/tmp/x.png" {
		t.Fatalf("asked = %v, want [/tmp/x.png]", f.asked)
	}
	if q == nil {
		t.Fatal("Rewrite returned nil seq")
	}
	got, _ := base64.StdEncoding.DecodeString(string(q.Payload))
	if string(got) != "/local/cache/abc.bin" {
		t.Fatalf("payload = %q", got)
	}
}

// The delete-after-read contract of t=t was with the sender's own temp file,
// which never crosses the bridge — the payload now names our local cache
// copy, so the output must downgrade to t=f or the local terminal would
// unlink the cache behind our back.
func TestRewriteDowngradesTransmitAndDeleteToTransmit(t *testing.T) {
	f := &fakeLocalizer{local: "/local/cache/abc.bin"}
	in := seqOf(t, "\x1b_Gi=1,a=T,t=t;L3RtcC94LnBuZw==\x1b\\")
	inKeys := append([]byte(nil), in.Keys...)
	q, drop, err := Rewrite(context.Background(), in, f)
	if err != nil || drop {
		t.Fatalf("drop=%v err=%v", drop, err)
	}
	if q == nil {
		t.Fatal("Rewrite returned nil seq")
	}
	if q.Get("t") != "f" {
		t.Fatalf("t = %q, want f", q.Get("t"))
	}
	got, _ := base64.StdEncoding.DecodeString(string(q.Payload))
	if string(got) != "/local/cache/abc.bin" {
		t.Fatalf("payload = %q", got)
	}
	if string(in.Keys) != string(inKeys) {
		t.Fatalf("input Seq mutated: %q, want %q", in.Keys, inKeys)
	}
}

func TestRewritePassesThroughInlineAndDelete(t *testing.T) {
	for _, raw := range []string{
		"\x1b_Gi=31,a=T,U=1,f=100,t=d;aGVsbG8=\x1b\\",
		"\x1b_Ga=d,d=I,i=31,q=2\x1b\\",
	} {
		f := &fakeLocalizer{}
		q, drop, err := Rewrite(context.Background(), seqOf(t, raw), f)
		if err != nil || drop {
			t.Fatalf("%q: drop=%v err=%v", raw, drop, err)
		}
		if len(f.asked) != 0 {
			t.Fatalf("%q: fetched needlessly", raw)
		}
		if q == nil {
			t.Fatalf("%q: Rewrite returned nil seq", raw)
		}
		if string(q.Encode()) != raw {
			t.Fatalf("%q: mutated to %q", raw, q.Encode())
		}
	}
}

func TestRewriteDropsSharedMemoryAndFetchFailures(t *testing.T) {
	if _, drop, _ := Rewrite(context.Background(), seqOf(t, "\x1b_Gi=1,a=T,t=s;c2htMQ==\x1b\\"), &fakeLocalizer{}); !drop {
		t.Fatal("t=s must be dropped: shared memory cannot cross hosts")
	}
	_, drop, err := Rewrite(context.Background(), seqOf(t, bareSeq), &fakeLocalizer{err: errFake})
	if !drop || err == nil {
		t.Fatal("a failed fetch must drop the store, never emit a stale local path")
	}
	if _, drop, _ := Rewrite(context.Background(), seqOf(t, "\x1b_Gi=1,a=T,t=f;!!!not-base64!!!\x1b\\"), &fakeLocalizer{}); !drop {
		t.Fatal("undecodable payload must be dropped")
	}
}
