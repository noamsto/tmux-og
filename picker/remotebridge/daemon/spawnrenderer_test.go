package daemon

import (
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRendererSpawnArgsIsCommandNotEnv(t *testing.T) {
	got := rendererSpawnArgs(Config{SockPath: "/run/sock", RendererBin: "/nix/store/renderer"}, "%2")
	want := []string{"/nix/store/renderer", "/run/sock", "%2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rendererSpawnArgs = %v, want %v", got, want)
	}
	for _, w := range got {
		if strings.HasPrefix(w, "-e") || strings.Contains(w, "OG_RENDER") {
			t.Errorf("env wiring in rendererSpawnArgs: %v", got)
		}
	}
}

func TestSpawnRendererArgvIsSockThenRemotePane(t *testing.T) {
	var got []string
	cfg := Config{
		SockPath:    "/run/sock",
		RendererBin: "/nix/store/renderer",
		LocalTmux: func(argv ...string) error {
			if len(argv) > 0 && argv[0] == "respawn-pane" {
				got = append([]string(nil), argv...)
			}
			return nil
		},
	}
	if err := spawnRenderer(cfg, "%l1", "%2"); err != nil {
		t.Fatalf("spawnRenderer: %v", err)
	}
	want := []string{"respawn-pane", "-k", "-t", "%l1", "--", "/nix/store/renderer", "/run/sock", "%2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("respawn-pane argv = %v, want %v", got, want)
	}
}

func TestRebindRendererReplacesTheSink(t *testing.T) {
	oldConn, oldPeer := net.Pipe()
	defer func() { _ = oldPeer.Close() }()
	newConn, newPeer := net.Pipe()
	defer func() { _ = newPeer.Close() }()

	reg := newRegistry()
	mw := reg.add("@1", "@101")
	mw.remotePanes = []string{"%2"}
	mw.conns["%2"] = oldConn

	router := NewRouter()
	router.Register("%2", newOutputSink(oldConn, nil))

	rebindRenderer(Config{}, helloConn{paneID: "%2", conn: newConn}, func(string) {}, router, reg,
		setupWindowRT(oneSeedScript("REBOUND-SEED")))

	if mw.conns["%2"] != newConn {
		t.Fatal("conns[%2] was not replaced with the redialed renderer")
	}
	_ = oldPeer.SetDeadline(time.Now().Add(time.Second))
	if _, err := oldPeer.Read(make([]byte, 1)); err == nil {
		t.Error("old renderer conn still open")
	}
	readSeedThenResize(t, newPeer, "%2")
}

func TestRebindRendererDropsAnUnknownPane(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	rebindRenderer(Config{}, helloConn{paneID: "%9", conn: conn}, func(string) {}, NewRouter(), newRegistry(),
		setupWindowRT(oneSeedScript("UNUSED")))
	_ = peer.SetDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Error("unknown-pane hello was kept open")
	}
}
