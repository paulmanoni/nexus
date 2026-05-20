package config

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus"
)

// fakeWSServer extends fakeServer with the /__config/subscribe
// endpoint so the subscribe-loop tests have a working push
// channel. Each test creates a fresh server so cross-test state
// stays out of the way.
type fakeWSServer struct {
	*fakeServer
	subs     *subscribers
	srv      *httptest.Server
	tlsURL   string
}

func newFakeWSServer(t *testing.T, app, profile string, initial map[string]any) *fakeWSServer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fs := &fakeServer{
		t: t, priv: priv, pub: pub, kid: "test-kid",
		app: app, profile: profile,
	}
	fs.values.Store(&initial)
	v := "v1"
	fs.version.Store(&v)

	ws := &fakeWSServer{fakeServer: fs, subs: newSubscribers()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__config/snapshot/{app}/{profile}", fs.handleSnapshot)
	mux.HandleFunc("GET /__config/version/{app}/{profile}", fs.handleVersion)
	mux.HandleFunc("GET /__config/subscribe/{app}/{profile}", ws.handleSubscribe)
	ws.srv = httptest.NewTLSServer(mux)
	ws.tlsURL = ws.srv.URL
	return ws
}

// handleSubscribe mirrors the production server's path: upgrade,
// register the sub, push current version immediately, fan out
// on update.
func (ws *fakeWSServer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	conn, err := subscribeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := ws.subs.add()
	sub.app = r.PathValue("app")
	sub.profile = r.PathValue("profile")
	defer ws.subs.remove(sub)

	// Initial push.
	current := *ws.version.Load()
	_ = conn.WriteJSON(subscribeEvent{Version: current, KID: ws.kid})

	disconnect := make(chan struct{})
	go func() {
		defer close(disconnect)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-disconnect:
			return
		case ev, ok := <-sub.send:
			if !ok {
				return
			}
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		}
	}
}

// pushUpdate is the test-only trigger that bumps the version
// and fans out to subscribers. Mirrors what notifyReload would
// do in production.
func (ws *fakeWSServer) pushUpdate(t *testing.T, values map[string]any, version string) {
	t.Helper()
	ws.values.Store(&values)
	ws.version.Store(&version)
	ws.subs.fanout(ws.app, ws.profile, subscribeEvent{Version: version, KID: ws.kid})
}

// TestSubscribe_ReceivesInitialAndUpdates is the headline path:
// client dials WS, receives the immediate "current version"
// event, then a subsequent push after the server bumps. Each
// event triggers a snapshot refresh on the client and the
// installed values reflect the latest push.
func TestSubscribe_ReceivesInitialAndUpdates(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	ws := newFakeWSServer(t, "app1", "prod", map[string]any{"flag": false})
	defer ws.srv.Close()

	keyPath := ws.writeSignerKey(t, dir)
	cachePath := filepath.Join(dir, "config.cache")
	h := buildTestClient(t, ws.srv, keyPath, cachePath)
	// Initial install via the regular fetch path.
	if err := initClient(h); err != nil {
		t.Fatalf("initClient: %v", err)
	}
	if got := nexus.Get[bool]("flag"); got {
		t.Errorf("flag = true before push, want false")
	}

	// Start the subscription loop with the cancelable ctx pattern.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startSubscription(ctx)
	defer h.stopSubscription()

	// Allow time for the initial-version push to flow + the
	// version-equal short-circuit to fire (no refresh expected).
	// Then trigger a real update.
	waitFor(t, 1*time.Second, func() bool {
		// Wait until at least one subscriber registered server-side.
		return ws.subs.count() >= 1
	})
	ws.pushUpdate(t, map[string]any{"flag": true}, "v2")

	// The client should refresh + install the new value.
	waitFor(t, 2*time.Second, func() bool {
		return nexus.Get[bool]("flag")
	})
	if got := nexus.Get[bool]("flag"); !got {
		t.Errorf("flag = false after push, want true")
	}
}

// TestSubscribe_AutoReconnects proves the loop survives a
// transient WS disconnect. Kill the server's WS subscriber once;
// the client should re-dial within the backoff window and
// continue receiving pushes after.
func TestSubscribe_AutoReconnects(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	ws := newFakeWSServer(t, "app1", "prod", map[string]any{"flag": false})
	defer ws.srv.Close()

	keyPath := ws.writeSignerKey(t, dir)
	cachePath := filepath.Join(dir, "config.cache")
	h := buildTestClient(t, ws.srv, keyPath, cachePath)
	if err := initClient(h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startSubscription(ctx)
	defer h.stopSubscription()

	waitFor(t, 1*time.Second, func() bool { return ws.subs.count() >= 1 })

	// Disconnect every current subscriber.
	ws.subs.mu.Lock()
	for sub := range ws.subs.conns {
		close(sub.send)
	}
	ws.subs.conns = map[*subscription]struct{}{}
	ws.subs.mu.Unlock()

	// Wait for the auto-reconnect — the loop's backoff starts at
	// 1s, so we allow up to 5s for the reconnect to land.
	if !waitFor(t, 5*time.Second, func() bool { return ws.subs.count() >= 1 }) {
		t.Fatal("client did not reconnect within 5s")
	}

	// Push an update; client should still receive.
	ws.pushUpdate(t, map[string]any{"flag": true}, "v2")
	waitFor(t, 2*time.Second, func() bool { return nexus.Get[bool]("flag") })
	if !nexus.Get[bool]("flag") {
		t.Errorf("flag = false after reconnect push, want true")
	}
}

// TestBuildSubscribeURL covers scheme rewriting (http→ws,
// https→wss) + path construction across server URL shapes.
func TestBuildSubscribeURL(t *testing.T) {
	cases := []struct {
		in       string
		app      string
		profile  string
		want     string
	}{
		{"http://localhost:8080", "app1", "prod", "ws://localhost:8080/__config/subscribe/app1/prod"},
		{"https://configd.internal:7100", "app1", "prod", "wss://configd.internal:7100/__config/subscribe/app1/prod"},
		{"https://configd.internal:7100/", "app1", "prod", "wss://configd.internal:7100/__config/subscribe/app1/prod"},
	}
	for _, c := range cases {
		got, err := buildSubscribeURL(c.in, c.app, c.profile)
		if err != nil {
			t.Errorf("%q: err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

// waitFor polls every 10ms until cond returns true or timeout
// expires. Returns whether cond ever became true. Used by the
// async push tests so they don't sleep blindly.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// silence unused imports that are imported for future wiring +
// for keeping the test file's package-level identifier hygiene
// clean across edits.
var (
	_ = json.NewDecoder
	_ = httputil.DumpResponse
	_ url.URL
	_ = websocket.PingMessage
	_ = os.Stdout
	_ atomic.Int32
)
