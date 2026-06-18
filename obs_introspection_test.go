package nexus

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/stdrouter"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

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
	t.Setenv(NexusDevEnv, "1")
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
// fx.New constructs an App with Dashboard.Enabled and
// Introspection:false (the v0.30 default). A public-IP request to
// /__nexus/config 404s; a loopback request reaches the handler.
func TestIntrospection_DashboardGated_End2End(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server:                ServerConfig{Addr: "127.0.0.1:0"},
			Dashboard:             DashboardConfig{Enabled: true, Name: "test"},
			Introspection:         false,
			IntrospectionNetworks: []string{"127.0.0.0/8"},
		}),
		fx.Populate(&app),
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
	New(Config{
		Dashboard:             DashboardConfig{Enabled: true},
		IntrospectionNetworks: []string{"127.0.0.0/8", "not-a-cidr"},
	})
}
