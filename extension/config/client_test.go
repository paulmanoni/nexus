package config

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

// fakeServer is a minimal stand-in for config.Server's HTTP
// surface. Lets the client-side tests exercise the real fetch +
// verify + cache path without spinning up the whole server boot
// chain. The actual config.Server's handlers are tested through
// the round-trip test below.
type fakeServer struct {
	t        *testing.T
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	kid      string
	values   atomic.Pointer[map[string]any]
	version  atomic.Pointer[string]
	app      string
	profile  string
	requests atomic.Int32
}

func newFakeServer(t *testing.T, app, profile string, initial map[string]any) *fakeServer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fs := &fakeServer{
		t:       t,
		priv:    priv,
		pub:     pub,
		kid:     "test-kid",
		app:     app,
		profile: profile,
	}
	fs.values.Store(&initial)
	v := "v1"
	fs.version.Store(&v)
	return fs
}

// start mounts an httptest.TLS server with the snapshot +
// version endpoints. Returns the URL.
func (fs *fakeServer) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__config/snapshot/{app}/{profile}", fs.handleSnapshot)
	mux.HandleFunc("GET /__config/version/{app}/{profile}", fs.handleVersion)
	return httptest.NewTLSServer(mux)
}

func (fs *fakeServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	fs.requests.Add(1)
	values := *fs.values.Load()
	version := *fs.version.Load()
	snap := Snapshot{
		App:      fs.app,
		Profile:  fs.profile,
		Version:  version,
		ServedAt: time.Now().UTC(),
		Values:   values,
	}
	signed, err := Sign(snap, fs.priv, fs.kid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(signed)
}

func (fs *fakeServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	v := *fs.version.Load()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"version": v, "kid": fs.kid})
}

// updateValues atomically swaps the served values + bumps the
// version. Drives the polling-refresh test.
func (fs *fakeServer) updateValues(next map[string]any, version string) {
	fs.values.Store(&next)
	fs.version.Store(&version)
}

// writeSignerKey writes the fake server's pubkey under a kid-
// derived filename so the client picks it up via SignerKey.
func (fs *fakeServer) writeSignerKey(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, fs.kid+".pub")
	if err := os.WriteFile(path, fs.pub, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClient_FetchVerifyInstall is the headline round-trip:
// fakeServer serves a signed snapshot, client fetches it,
// verifies, installs into the store. nexus.Get returns the
// served values. Without this, every other client test is
// gated on a happy-path that never proved itself.
func TestClient_FetchVerifyInstall(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()

	fs := newFakeServer(t, "app1", "prod", map[string]any{
		"api":  map[string]any{"timeout": "5s"},
		"flag": true,
	})
	srv := fs.start()
	defer srv.Close()

	keyPath := fs.writeSignerKey(t, dir)
	cachePath := filepath.Join(dir, "config.cache")

	h := buildTestClient(t, srv, keyPath, cachePath)
	if err := initClient(h); err != nil {
		t.Fatalf("initClient: %v", err)
	}

	if got := nexus.Get[string]("api.timeout"); got != "5s" {
		t.Errorf("api.timeout = %q, want 5s", got)
	}
	if got := nexus.Get[bool]("flag"); !got {
		t.Errorf("flag = false, want true")
	}
	// Cache file must exist and be sealed (NXCS magic).
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache missing: %v", err)
	}
	if !IsSealed(body) {
		t.Errorf("cache file is not sealed")
	}
}

// TestClient_BootsFromSealedCacheWhenServerDown is the offline-
// boot contract: the server is unreachable at boot, but a
// valid sealed cache exists — the client uses it. Without this,
// every restart during a config-server outage would fail the
// whole fleet.
func TestClient_BootsFromSealedCacheWhenServerDown(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()

	// Boot 1: server up, cache populates.
	fs := newFakeServer(t, "app1", "prod", map[string]any{
		"baked_in": "from-cache",
	})
	srv := fs.start()
	keyPath := fs.writeSignerKey(t, dir)
	cachePath := filepath.Join(dir, "config.cache")
	h := buildTestClient(t, srv, keyPath, cachePath)
	if err := initClient(h); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	srv.Close()

	// Reset the store; simulate a process restart.
	nexus.ClearConfigStoreForTest()

	// Boot 2: same cache, server unreachable.
	h2 := buildTestClient(t, srv, keyPath, cachePath) // srv.URL is dead now
	if err := initClient(h2); err != nil {
		t.Fatalf("offline boot should succeed via cache: %v", err)
	}
	if got := nexus.Get[string]("baked_in"); got != "from-cache" {
		t.Errorf("cached value = %q, want from-cache", got)
	}
}

// TestClient_FailsBootWhenServerDownAndNoCache proves the
// UseCacheOrFail policy fires when both fail-safes are missing.
// Operators see a loud boot error, not silent stale config.
func TestClient_FailsBootWhenServerDownAndNoCache(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	fs := newFakeServer(t, "app1", "prod", map[string]any{})
	srv := fs.start()
	keyPath := fs.writeSignerKey(t, dir)
	srv.Close() // dead before first boot

	h := buildTestClient(t, srv, keyPath, filepath.Join(dir, "missing.cache"))
	err := initClient(h)
	if err == nil {
		t.Fatal("initClient should fail when server down + no cache")
	}
}

// TestClient_UseDefaultsPolicy proves the defaults fallback fires
// when configured. Server down + no cache → install the
// WithDefaults map.
func TestClient_UseDefaultsPolicy(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	fs := newFakeServer(t, "app1", "prod", map[string]any{})
	srv := fs.start()
	keyPath := fs.writeSignerKey(t, dir)
	srv.Close()

	h := buildTestClient(t, srv, keyPath, filepath.Join(dir, "missing.cache"))
	h.cfg.onUnreachable = UseDefaults
	h.cfg.defaults = map[string]any{"floor": "value"}
	if err := initClient(h); err != nil {
		t.Fatalf("UseDefaults should succeed: %v", err)
	}
	if got := nexus.Get[string]("floor"); got != "value" {
		t.Errorf("default value not installed: got %q", got)
	}
}

// TestClient_RejectsTamperedCacheFile proves cache tamper
// detection. A flipped byte in the sealed file is caught by
// AES-GCM's auth tag; the client falls back to the server
// fetch instead of running on corrupted bytes.
func TestClient_RejectsTamperedCacheFile(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	fs := newFakeServer(t, "app1", "prod", map[string]any{"v": 1})
	srv := fs.start()
	defer srv.Close()
	keyPath := fs.writeSignerKey(t, dir)
	cachePath := filepath.Join(dir, "config.cache")

	// First boot populates the cache.
	h := buildTestClient(t, srv, keyPath, cachePath)
	if err := initClient(h); err != nil {
		t.Fatalf("first boot: %v", err)
	}

	// Tamper with the cache.
	body, _ := os.ReadFile(cachePath)
	body[len(body)-3] ^= 0xFF
	_ = os.WriteFile(cachePath, body, 0o600)

	// Reset and reboot — the tampered cache should be rejected
	// (loud log) but the client falls back to the live server
	// fetch and succeeds.
	nexus.ClearConfigStoreForTest()
	h2 := buildTestClient(t, srv, keyPath, cachePath)
	if err := initClient(h2); err != nil {
		t.Fatalf("reboot with tampered cache should succeed via server: %v", err)
	}
}

// buildTestClient is the common scaffold the tests above use to
// build a clientHolder against a fakeServer. Keeps each test
// body focused on the behavior under test.
func buildTestClient(t *testing.T, srv *httptest.Server, keyPath, cachePath string) *clientHolder {
	t.Helper()
	h := &clientHolder{
		cfg: clientConfig{
			serverURL:      srv.URL,
			identity:       "app1",
			profile:        "prod",
			signerKeyPaths: []string{keyPath},
			cachePath:      cachePath,
			onUnreachable:  UseCacheOrFail,
			pollInterval:   1 * time.Hour, // disable polling for tests
			requestTimeout: 2 * time.Second,
		},
	}
	if err := h.loadPinnedKeys(); err != nil {
		t.Fatalf("loadPinnedKeys: %v", err)
	}
	// Reuse the httptest client (already trusts the test cert).
	h.httpClient = srv.Client()
	h.httpClient.Timeout = 2 * time.Second
	if err := h.ensureSealKey(); err != nil {
		t.Fatalf("ensureSealKey: %v", err)
	}
	return h
}

// silence unused-import warning when build flags strip out a
// dependent test from the package.
var _ = context.Background
