package nexus

import (
	"context"
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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/httpx/stdrouter"
)

// TestHealth_AliveFlagsToggle verifies /__nexus/health returns 200 once
// fx Start completes and reverts to 503 after Stop. This is the basic
// liveness contract orchestrators rely on.
func TestHealth_AliveFlagsToggle(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(config.Runtime{Server: config.Server{Addr: "127.0.0.1:0"}}),
		di.Populate(&app),
	)
	fxApp.RequireStart()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/health", nil))
	if w.Code != http.StatusOK {
		t.Errorf("alive after Start: want 200, got %d", w.Code)
	}

	fxApp.RequireStop()
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("alive after Stop: want 503, got %d", w.Code)
	}
}

// TestReady_MonolithReadyImmediately verifies a deployment with no peers
// becomes ready as soon as it's alive — the monolith case.
func TestReady_MonolithReadyImmediately(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(config.Runtime{Server: config.Server{Addr: "127.0.0.1:0"}}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/ready", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("monolith ready: want 200, got %d", w.Code)
	}
}

// TestParseIntrospectionNetworks_HappyPath confirms a mixed list of
// IPv4 + loopback + RFC1918 CIDRs all compile, and unhappy entries
// fail fast with a clear error rather than silently no-op'ing — the
// gate is a security knob; misconfiguration must surface at boot.
func TestParseIntrospectionNetworks_HappyPath(t *testing.T) {
	cidrs := []string{"127.0.0.0/8", "192.168.1.0/24", "10.0.0.0/8"}
	nets, err := parseIntrospectionNetworks(cidrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nets) != 3 {
		t.Fatalf("len: got %d, want 3", len(nets))
	}
	for _, c := range []struct {
		ip      string
		matches bool
	}{
		{"127.0.0.1", true},
		{"127.255.255.255", true},
		{"192.168.1.5", true},
		{"192.168.2.5", false},
		{"10.0.0.1", true},
		{"8.8.8.8", false},
	} {
		ip := net.ParseIP(c.ip)
		got := false
		for _, n := range nets {
			if n.Contains(ip) {
				got = true
				break
			}
		}
		if got != c.matches {
			t.Errorf("%s: got match=%v, want %v", c.ip, got, c.matches)
		}
	}
}

// TestParseIntrospectionNetworks_BadCIDRFailsFast pins the boot-
// time validation. A single typo in IntrospectionNetworks must
// surface as an error, not silently drop the entry — silent drops
// turn a security gate into a false sense of security.
func TestParseIntrospectionNetworks_BadCIDRFailsFast(t *testing.T) {
	_, err := parseIntrospectionNetworks([]string{"127.0.0.0/8", "not-a-cidr"})
	if err == nil {
		t.Fatal("expected error on invalid CIDR")
	}
	if !strings.Contains(err.Error(), "not-a-cidr") {
		t.Errorf("error should name the bad entry: %v", err)
	}
}

// TestParseIntrospectionNetworks_EmptyIsNil documents the contract:
// an empty input means "no allowlist", which the gate logic treats
// as strict mode. Returning nil (not []*net.IPNet{}) lets callers
// short-circuit on len(nets) == 0 without a separate flag.
func TestParseIntrospectionNetworks_EmptyIsNil(t *testing.T) {
	if nets, err := parseIntrospectionNetworks(nil); err != nil || nets != nil {
		t.Errorf("empty: got (%v, %v), want (nil, nil)", nets, err)
	}
	if nets, err := parseIntrospectionNetworks([]string{}); err != nil || nets != nil {
		t.Errorf("zero-len: got (%v, %v), want (nil, nil)", nets, err)
	}
}

// TestIntrospectionGate_OpenInDevMode pins the dev-only bypass:
// NEXUS_DEV=1 makes introspectionGate return nil so /__nexus/* routes
// stay reachable for the operator running `nexus dev`. Production
// binaries never see NEXUS_DEV=1, so strict-mode stays strict.
func TestIntrospectionGate_OpenInDevMode(t *testing.T) {
	t.Setenv(dev.Env, "1")
	if gate := introspectionGate(false, nil); gate != nil {
		t.Fatal("gate should be nil under NEXUS_DEV=1")
	}
}

// TestIntrospectionGate_BlocksByDefault is the v0.30 contract: with
// Introspection: false and an empty allowlist, every request to a
// gated route 404s — indistinguishable from "never mounted" so
// anonymous scanners learn nothing.
func TestIntrospectionGate_BlocksByDefault(t *testing.T) {
	gate := introspectionGate(false, nil)
	if gate == nil {
		t.Fatal("gate should be installed when Introspection is false")
	}
	r := stdrouter.New()
	r.Use(gate)
	r.GET("/__nexus/secret", func(c *httpx.Ctx) {
		c.String(http.StatusOK, "leaked")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/__nexus/secret", nil)
	req.RemoteAddr = "8.8.8.8:54321"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "leaked") {
		t.Error("gate let the body through — handler must not have run")
	}
}

// TestIntrospectionGate_AllowedNetworkBypasses confirms a TCP peer
// inside the CIDR allowlist reaches the handler — the office-LAN /
// VPN / loopback scenario the user designed this for.
func TestIntrospectionGate_AllowedNetworkBypasses(t *testing.T) {
	nets, _ := parseIntrospectionNetworks([]string{"127.0.0.0/8", "192.168.1.0/24"})
	gate := introspectionGate(false, nets)
	r := stdrouter.New()
	r.Use(gate)
	r.GET("/__nexus/secret", func(c *httpx.Ctx) {
		c.String(http.StatusOK, "ok")
	})
	for _, c := range []struct {
		peer   string
		expect int
	}{
		{"127.0.0.1:1", http.StatusOK},
		{"192.168.1.50:1", http.StatusOK},
		{"192.168.2.50:1", http.StatusNotFound}, // adjacent /24, NOT in list
		{"8.8.8.8:1", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/__nexus/secret", nil)
		req.RemoteAddr = c.peer
		r.ServeHTTP(w, req)
		if w.Code != c.expect {
			t.Errorf("peer=%s: got %d, want %d", c.peer, w.Code, c.expect)
		}
	}
}

// TestIntrospectionGate_OpenWhenIntrospectionTrue: with the master
// flag on, the gate factory returns nil (no middleware) so the hot
// path stays empty — dev / internal listeners pay zero cost.
func TestIntrospectionGate_OpenWhenIntrospectionTrue(t *testing.T) {
	if gate := introspectionGate(true, nil); gate != nil {
		t.Error("gate should be nil when Introspection is true")
	}
	if gate := introspectionGate(true, []*net.IPNet{{}}); gate != nil {
		t.Error("Introspection:true must short-circuit even with networks set")
	}
}

// TestIntrospectionGate_IgnoresXForwardedFor pins the unspoofable-
// peer contract. ClientIP would honor X-Forwarded-For if Gin's
// TrustedProxies were configured — wrong default for a security
// gate. RemoteIP is the actual TCP peer; spoofing it requires
// controlling the network path, not just sending a header.
func TestIntrospectionGate_IgnoresXForwardedFor(t *testing.T) {
	nets, _ := parseIntrospectionNetworks([]string{"127.0.0.0/8"})
	gate := introspectionGate(false, nets)
	r := stdrouter.New()
	r.Use(gate)
	r.GET("/__nexus/secret", func(c *httpx.Ctx) {
		c.String(http.StatusOK, "ok")
	})

	// Peer is public (8.8.8.8); X-Forwarded-For claims loopback —
	// an attacker hitting the public listener directly + spoofing
	// the header. Must NOT bypass.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/__nexus/secret", nil)
	req.RemoteAddr = "8.8.8.8:54321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("X-Forwarded-For spoofing bypassed the gate (status=%d) — RemoteIP must win", w.Code)
	}
}

// TestIntrospection_DashboardGated_End2End is the full-stack pin:
// di.New constructs an App with Dashboard.Enabled and
// Introspection:false (the v0.30 default). A public-IP request to
// /__nexus/config 404s; a loopback request reaches the handler.
func TestIntrospection_DashboardGated_End2End(t *testing.T) {
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(config.Runtime{
			Server:                config.Server{Addr: "127.0.0.1:0"},
			Dashboard:             config.Dashboard{Enabled: true, Name: "test"},
			Introspection:         false,
			IntrospectionNetworks: []string{"127.0.0.0/8"},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	ts := httptest.NewServer(app)
	defer ts.Close()

	// Loopback: TS host loops back to 127.0.0.1 by default — request
	// arrives with peer = 127.0.0.1, which is in the allowlist.
	r, err := http.Get(ts.URL + "/__nexus/config")
	if err != nil {
		t.Fatalf("loopback GET: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("loopback /__nexus/config: got %d, want 200", r.StatusCode)
	}

	// Health stays public regardless — orchestration probes don't
	// come from the allowlist and must always succeed.
	r2, err := http.Get(ts.URL + "/__nexus/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("health: got %d, want 200 (must stay unconditional)", r2.StatusCode)
	}
}

// TestIntrospection_BadCIDRPanicsAtBoot pins the fail-fast contract.
// nexus.New invokes panic() on a malformed CIDR so the operator
// sees the bug at startup, not at the first dashboard request.
func TestIntrospection_BadCIDRPanicsAtBoot(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on invalid CIDR")
		}
		msg, _ := r.(error)
		if msg == nil || !strings.Contains(msg.Error(), "not-a-cidr") {
			t.Errorf("panic message should name the bad entry, got: %v", r)
		}
	}()
	New(config.Runtime{
		Dashboard:             config.Dashboard{Enabled: true},
		IntrospectionNetworks: []string{"127.0.0.0/8", "not-a-cidr"},
	})
}

// listenerBoundAddr returns "127.0.0.1:<port>" for the listener whose
// scope matches want. The scope table is keyed by port (so dual-stack
// IPv6/IPv4 binds resolve correctly at request time); tests rebuild
// the dial-able address by gluing the loopback host to that port.
func listenerBoundAddr(app *App, want config.ListenerScope) string {
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
		fxBootOptions(config.Runtime{
			Dashboard:     config.Dashboard{Enabled: true},
			Introspection: true, // test exercises gated routes; opt in
			TraceCapacity: 100,
			Server: config.Server{
				Listeners: map[string]config.Listener{
					"public":   {Addr: "127.0.0.1:0", Scope: config.ScopePublic},
					"internal": {Addr: "127.0.0.1:0", Scope: config.ScopeInternal},
					"admin":    {Addr: "127.0.0.1:0", Scope: config.ScopeAdmin},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, config.ScopePublic)
	adminAddr := listenerBoundAddr(app, config.ScopeAdmin)
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
	app.Router().GET("/ping", func(c *httpx.Ctx) { c.String(http.StatusOK, "pong") })
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
		fxBootOptions(config.Runtime{
			Dashboard:     config.Dashboard{Enabled: true},
			Introspection: true, // test exercises gated routes; opt in
			TraceCapacity: 100,
			Server: config.Server{
				Listeners: map[string]config.Listener{
					// Bare host elides → dual-stack. ln.Addr()
					// comes back as "[::]:<port>"; request
					// LocalAddr arrives as "127.0.0.1:<port>".
					"public": {Addr: ":0", Scope: config.ScopePublic},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, config.ScopePublic)
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
	in := map[string]config.Listener{
		"public":   {},
		"admin":    {Scope: config.ScopeAdmin},
		"internal": {Scope: config.ScopeInternal},
		"explicit": {Addr: "127.0.0.1:5555", Scope: config.ScopeAdmin},
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
	in := map[string]config.Listener{
		"public": {},
		"admin":  {Scope: config.ScopeAdmin},
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
		fxBootOptions(config.Runtime{
			Dashboard:     config.Dashboard{Enabled: true},
			Introspection: true,
			TraceCapacity: 100,
			Server: config.Server{
				Listeners: map[string]config.Listener{
					"public": {Addr: "127.0.0.1:0", Scope: config.ScopePublic},
					"admin":  {Addr: "127.0.0.1:0", Scope: config.ScopeAdmin, TLS: tlsCfg},
				},
			},
		}),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	adminAddr := listenerBoundAddr(app, config.ScopeAdmin)
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
		fxBootOptions(config.Runtime{
			Server:        config.Server{Addr: "127.0.0.1:0"},
			Dashboard:     config.Dashboard{Enabled: true},
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

func TestShutdownTimeoutResolution(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	if got := shutdownTimeout(config.Runtime{}); got != DefaultShutdownTimeout {
		t.Fatalf("production default = %s, want %s", got, DefaultShutdownTimeout)
	}
	cfg := config.Runtime{Server: config.Server{ShutdownTimeout: 2 * time.Second}}
	if got := shutdownTimeout(cfg); got != 2*time.Second {
		t.Fatalf("explicit config = %s, want 2s", got)
	}

	t.Setenv("NEXUS_DEV", "1")
	if got := shutdownTimeout(config.Runtime{}); got != DevShutdownTimeout {
		t.Fatalf("dev default = %s, want %s", got, DevShutdownTimeout)
	}
	// Explicit config still wins in dev — an operator who asked for a drain
	// gets one wherever they're running.
	if got := shutdownTimeout(cfg); got != 2*time.Second {
		t.Fatalf("explicit config in dev = %s, want 2s", got)
	}
}

func TestShutdownTimeoutFromTOML(t *testing.T) {
	cfg, err := config.Parse([]byte("[runtime.server]\naddr = \":9999\"\nshutdown_timeout = \"3s\"\n"), "test")
	if err != nil {
		t.Fatalf("configFromTOML: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 3s", cfg.Server.ShutdownTimeout)
	}
	// A malformed duration degrades to the default rather than refusing to
	// boot — shutdown timing is a tuning knob, not a correctness one.
	cfg, err = config.Parse([]byte("[runtime.server]\nshutdown_timeout = \"soon\"\n"), "test")
	if err != nil {
		t.Fatalf("configFromTOML with a bad duration: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 0 {
		t.Fatalf("bad duration produced %s, want 0 (fall through to default)", cfg.Server.ShutdownTimeout)
	}
}

// The regression this whole change exists for: a request still in flight must
// not hold shutdown open for the full grace window. The handler selects on its
// request context, so cancelling the server's BaseContext lets it return and
// Shutdown completes immediately.
func TestShutdownCancelsInFlightRequests(t *testing.T) {
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	entered := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		BaseContext:       func(l net.Listener) context.Context { return reqCtx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done() // hangs until someone cancels us
			w.WriteHeader(http.StatusOK)
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered

	start := time.Now()
	cancelReqs()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Shutdown took %s with one in-flight request; the cancel didn't reach the handler", el)
	}
}

// With [runtime.telemetry] otlp_endpoint set, request spans reach the
// collector — flushed at shutdown at the latest.
func TestTelemetryExportsRequests(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer collector.Close()

	cfg := config.Runtime{Telemetry: config.Telemetry{OTLPEndpoint: collector.URL, ServiceName: "orders"}}
	app, stop, err := InProcess(cfg, AsRest("GET", "/orders", func(ctx context.Context) ([]string, error) { return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/orders", nil))
	time.Sleep(50 * time.Millisecond)
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(bodies, "\n")
	if !strings.Contains(all, `"stringValue":"orders"`) || !strings.Contains(all, `"/orders"`) {
		t.Fatalf("collector got %q", all)
	}
}
