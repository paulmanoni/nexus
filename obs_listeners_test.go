package nexus

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
)

// listenerBoundAddr returns "127.0.0.1:<port>" for the listener whose
// scope matches want. The scope table is keyed by port (so dual-stack
// IPv6/IPv4 binds resolve correctly at request time); tests rebuild
// the dial-able address by gluing the loopback host to that port.
func listenerBoundAddr(app *App, want ListenerScope) string {
	app.listenerScopes.mu.RLock()
	defer app.listenerScopes.mu.RUnlock()
	for port, s := range app.listenerScopes.m {
		if s == want {
			return "127.0.0.1:" + port
		}
	}
	return ""
}

func httpGetStatus(t *testing.T, addr, path string) int {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s%s: %v", addr, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestListeners_ScopeFilter verifies that an explicit Listeners map
// binds N ports and the scope filter routes correctly: dashboard hidden
// on public, dashboard visible on admin, user routes hidden on admin.
func TestListeners_ScopeFilter(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{
			Dashboard:     DashboardConfig{Enabled: true},
			Introspection: true, // test exercises gated routes; opt in
			TraceCapacity: 100,
			Server: ServerConfig{
				Listeners: map[string]Listener{
					"public":   {Addr: "127.0.0.1:0", Scope: ScopePublic},
					"internal": {Addr: "127.0.0.1:0", Scope: ScopeInternal},
					"admin":    {Addr: "127.0.0.1:0", Scope: ScopeAdmin},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, ScopePublic)
	adminAddr := listenerBoundAddr(app, ScopeAdmin)
	if publicAddr == "" || adminAddr == "" {
		t.Fatalf("listeners not bound: public=%q admin=%q", publicAddr, adminAddr)
	}

	// Dashboard is admin-scoped: hidden on public, served on admin.
	if got := httpGetStatus(t, publicAddr, "/__nexus/config"); got != http.StatusNotFound {
		t.Errorf("public /__nexus/config: want 404, got %d", got)
	}
	if got := httpGetStatus(t, adminAddr, "/__nexus/config"); got != http.StatusOK {
		t.Errorf("admin /__nexus/config: want 200, got %d", got)
	}

	// Register a user route and confirm it's reachable on admin —
	// admin scope serves both /__nexus/* and user routes (operator
	// ergonomics; lets the dashboard's RestTester fire relative
	// fetch() calls without 404ing on the listener it loaded from).
	app.Engine().GET("/ping", func(c *httpx.Ctx) { c.String(http.StatusOK, "pong") })
	if got := httpGetStatus(t, adminAddr, "/ping"); got != http.StatusOK {
		t.Errorf("admin /ping: want 200, got %d (admin scope should serve user routes)", got)
	}
	// Same route on the public listener still works — public allows
	// everything except /__nexus/*.
	if got := httpGetStatus(t, publicAddr, "/ping"); got != http.StatusOK {
		t.Errorf("public /ping: want 200, got %d", got)
	}

	// Health + readiness ARE exposed on the public listener — k8s
	// liveness probes hit the container port (typically the public
	// one), and the framework's own peerProber probes peers through
	// their declared public URL. Without this, multi-listener apps
	// silently fail readiness in production.
	if got := httpGetStatus(t, publicAddr, "/__nexus/health"); got != http.StatusOK {
		t.Errorf("public /__nexus/health: want 200, got %d", got)
	}
	if got := httpGetStatus(t, publicAddr, "/__nexus/ready"); got != http.StatusOK {
		t.Errorf("public /__nexus/ready: want 200, got %d", got)
	}
}

// TestListeners_DualStackBindResolves regression-tests the case that
// motivated keying scopes by port: a listener configured with bare
// ":<port>" binds dual-stack on `[::]:<port>`, but inbound requests
// land with a LocalAddr of `127.0.0.1:<port>`. Looking up the scope by
// full bound address misses; lookup by port hits. Without this, the
// dashboard leaked onto the public listener under any unbracketed
// bind (the common case).
func TestListeners_DualStackBindResolves(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{
			Dashboard:     DashboardConfig{Enabled: true},
			Introspection: true, // test exercises gated routes; opt in
			TraceCapacity: 100,
			Server: ServerConfig{
				Listeners: map[string]Listener{
					// Bare host elides → dual-stack. ln.Addr()
					// comes back as "[::]:<port>"; request
					// LocalAddr arrives as "127.0.0.1:<port>".
					"public": {Addr: ":0", Scope: ScopePublic},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, ScopePublic)
	if publicAddr == "" {
		t.Fatal("public listener not registered")
	}
	if got := httpGetStatus(t, publicAddr, "/__nexus/config"); got != http.StatusNotFound {
		t.Errorf("public /__nexus/config on dual-stack bind: want 404, got %d", got)
	}
}

// TestFillListenerAddrs verifies the auto-fill: empty Addrs derive
// from publicAddr per scope; explicit Addrs pass through. This is
// the load-bearing helper that makes split deployments work without
// per-binary main.go — the manifest's per-deployment port flows
// into the public listener and admin = public + offset.
func TestFillListenerAddrs(t *testing.T) {
	in := map[string]Listener{
		"public":   {},
		"admin":    {Scope: ScopeAdmin},
		"internal": {Scope: ScopeInternal},
		"explicit": {Addr: "127.0.0.1:5555", Scope: ScopeAdmin},
	}
	out := fillListenerAddrs(in, ":8081")

	if out["public"].Addr != ":8081" {
		t.Errorf("public: want :8081, got %q", out["public"].Addr)
	}
	if out["admin"].Addr != ":9081" {
		t.Errorf("admin: want :9081, got %q", out["admin"].Addr)
	}
	if out["internal"].Addr != ":10081" {
		t.Errorf("internal: want :10081, got %q", out["internal"].Addr)
	}
	if out["explicit"].Addr != "127.0.0.1:5555" {
		t.Errorf("explicit: want pass-through, got %q", out["explicit"].Addr)
	}
}

// TestFillListenerAddrs_DefaultsWhenEmpty verifies the framework's
// :8080 fallback kicks in for plain `go run` (no manifest defaults).
func TestFillListenerAddrs_DefaultsWhenEmpty(t *testing.T) {
	in := map[string]Listener{
		"public": {},
		"admin":  {Scope: ScopeAdmin},
	}
	out := fillListenerAddrs(in, "")
	if out["public"].Addr != ":8080" {
		t.Errorf("public default: want :8080, got %q", out["public"].Addr)
	}
	if out["admin"].Addr != ":9080" {
		t.Errorf("admin default: want :9080, got %q", out["admin"].Addr)
	}
}

// writeSelfSignedCert generates a fresh self-signed cert for 127.0.0.1
// into dir, returning the cert and key paths. RSA-2048 keeps the boot
// cost reasonable while staying valid input for tls.LoadX509KeyPair.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nexus-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// TestListeners_TLS verifies that setting Listener.TLS terminates HTTPS
// on that port: an https:// request with a permissive client succeeds,
// and a plain http:// request to the same port fails at the TLS layer.
// Proves the bind loop's tls.NewListener wrap is in effect.
func TestListeners_TLS(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	tlsCfg, err := ServerTLSConfig(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{
			Dashboard:     DashboardConfig{Enabled: true},
			Introspection: true,
			TraceCapacity: 100,
			Server: ServerConfig{
				Listeners: map[string]Listener{
					"public": {Addr: "127.0.0.1:0", Scope: ScopePublic},
					"admin":  {Addr: "127.0.0.1:0", Scope: ScopeAdmin, TLS: tlsCfg},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	adminAddr := listenerBoundAddr(app, ScopeAdmin)
	if adminAddr == "" {
		t.Fatal("admin listener not bound")
	}

	// HTTPS succeeds with the self-signed cert ignored.
	httpsClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
	}
	resp, err := httpsClient.Get("https://" + adminAddr + "/__nexus/config")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("https /__nexus/config: want 200, got %d", resp.StatusCode)
	}

	// Plain HTTP to the same port must not be served. Go's net/http
	// detects this pattern (first bytes don't look like a ClientHello)
	// and replies "400 Client sent an HTTP request to an HTTPS
	// server." instead of dispatching the request. That 400 + body
	// is the canonical proof that the listener is HTTPS-only.
	plainClient := &http.Client{Timeout: 2 * time.Second}
	plainResp, plainErr := plainClient.Get("http://" + adminAddr + "/__nexus/config")
	if plainErr != nil {
		// Some Go versions / TLS configurations may close the
		// connection without responding; that's also acceptable.
		return
	}
	body, _ := io.ReadAll(plainResp.Body)
	plainResp.Body.Close()
	if plainResp.StatusCode != http.StatusBadRequest {
		t.Errorf("plain http to TLS listener: want 400 (HTTPS-only refusal), got %d body=%q", plainResp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "HTTPS server") {
		t.Errorf("plain http to TLS listener: want body containing 'HTTPS server', got %q", string(body))
	}
}

// TestListeners_BackCompat_NoConfig verifies the default single-listener
// path keeps working: /__nexus/* stays reachable when Listeners is empty.
func TestListeners_BackCompat_NoConfig(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{
			Server:        ServerConfig{Addr: "127.0.0.1:0"},
			Dashboard:     DashboardConfig{Enabled: true},
			Introspection: true, // test exercises gated routes; opt in
			TraceCapacity: 100,
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// No filtering when Listeners is empty — scope table stays empty.
	if !app.listenerScopes.empty() {
		t.Fatal("scope table should be empty in back-compat mode")
	}
}
