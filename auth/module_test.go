package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/auth"
)

// TestModule_EndToEnd wires auth.Module through a full nexus.Run-style
// app (Config + Module + a REST handler) and verifies:
//   - Anonymous request → handler runs, sees no identity.
//   - Bearer request → handler runs, sees resolved identity.
//   - Second request with the same token → resolver NOT called (cache hit).
func TestModule_EndToEnd(t *testing.T) {
	var calls int32
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		atomic.AddInt32(&calls, 1)
		if tok == "bad" {
			return nil, fmt.Errorf("invalid token")
		}
		return &auth.Identity{
			ID:    tok,
			Roles: []string{"reader"},
			Extra: &testUser{Name: "paul"},
		}, nil
	}

	addr := startTestApp(t, auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(500 * time.Millisecond),
	})

	// Anonymous: handler sees nil identity. No 401 because the handler
	// has no auth.Required attached — global middleware stays permissive.
	body, status := httpGet(t, addr+"/whoami", "")
	if status != 200 {
		t.Fatalf("anonymous: status=%d body=%s", status, body)
	}
	var anon whoamiResp
	mustJSON(t, body, &anon)
	if anon.Authed {
		t.Fatalf("anonymous request should see no identity; got %+v", anon)
	}

	// Authed: resolver fires, handler sees identity.
	body, status = httpGet(t, addr+"/whoami", "tok-1")
	if status != 200 {
		t.Fatalf("authed: status=%d body=%s", status, body)
	}
	var authed whoamiResp
	mustJSON(t, body, &authed)
	if !authed.Authed || authed.ID != "tok-1" || authed.UserName != "paul" {
		t.Fatalf("authed request did not see identity: %+v", authed)
	}

	// Cache hit: same token, no second resolve.
	before := atomic.LoadInt32(&calls)
	httpGet(t, addr+"/whoami", "tok-1")
	after := atomic.LoadInt32(&calls)
	if after != before {
		t.Fatalf("cache should suppress second resolve for same token; calls before=%d after=%d", before, after)
	}

	// Resolve failure path: bad token → OnFail-equivalent; handler
	// sees no identity (but request continues). We only assert the
	// handler-visible side here — middleware doesn't 401 anonymously
	// unless auth.Required is attached.
	body, status = httpGet(t, addr+"/whoami", "bad")
	if status != 200 {
		t.Fatalf("bad token: status=%d body=%s", status, body)
	}
	var bad whoamiResp
	mustJSON(t, body, &bad)
	if bad.Authed {
		t.Fatal("bad token must not produce an identity")
	}
}

// whoamiResp is what the /whoami test handler returns.
type whoamiResp struct {
	Authed   bool   `json:"authed"`
	ID       string `json:"id"`
	UserName string `json:"userName"`
}

// startTestApp boots a nexus app on an ephemeral port with auth.Module
// and one REST handler that reports identity state. Returns the
// base URL. Uses AsRest so the test also exercises the full
// transport chain, not just a bare gin handler.
func startTestApp(t *testing.T, cfg auth.Config) string {
	t.Helper()

	go func() {
		nexus.Run(
			nexus.Config{Server: nexus.ServerConfig{Addr: "127.0.0.1:8799"}, TraceCapacity: 10},
			auth.Module(cfg),
			nexus.AsRest("GET", "/whoami", whoamiHandler),
		)
	}()

	// Wait for the listener to come up; abort quickly on a hung start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://127.0.0.1:8799/whoami"); err == nil {
			return "http://127.0.0.1:8799"
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("test app did not start within 3s")
	return ""
}

// whoamiHandler uses auth.IdentityFrom + auth.User[T] on the request
// ctx. Registered via AsRest so the nexus reflective path + metrics
// middleware + graphRecorder etc are all exercised too.
func whoamiHandler(ctx context.Context) (whoamiResp, error) {
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return whoamiResp{Authed: false}, nil
	}
	user, _ := auth.User[testUser](ctx)
	r := whoamiResp{Authed: true, ID: id.ID}
	if user != nil {
		r.UserName = user.Name
	}
	return r, nil
}

func httpGet(t *testing.T, url, bearer string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 2048)
	n, _ := res.Body.Read(buf)
	return string(buf[:n]), res.StatusCode
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.NewDecoder(strings.NewReader(s)).Decode(v); err != nil {
		t.Fatalf("bad JSON %q: %v", s, err)
	}
}