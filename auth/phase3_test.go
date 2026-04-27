package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/auth"
)

func TestManager_InvalidateByIdentity(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		// Map tokens "t1-*" onto user "user-1", tokens "t2-*" onto user-2.
		// Lets us seed multiple cache entries for one identity and
		// verify the sweep.
		uid := "user-1"
		if strings.HasPrefix(tok, "t2-") {
			uid = "user-2"
		}
		return &auth.Identity{ID: uid}, nil
	}
	mgr := bootWithManager(t, "127.0.0.1:8793", auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})

	ctx := context.Background()
	// 3 distinct tokens, 2 users.
	for _, tok := range []string{"t1-a", "t1-b", "t2-c"} {
		_, _ = mgr.Resolve(ctx, tok)
	}
	if n := len(mgr.Identities()); n != 3 {
		t.Fatalf("expected 3 cached entries, got %d", n)
	}

	dropped := mgr.InvalidateByIdentity("user-1")
	if dropped != 2 {
		t.Fatalf("expected 2 dropped (two tokens for user-1), got %d", dropped)
	}
	if n := len(mgr.Identities()); n != 1 {
		t.Fatalf("expected 1 remaining, got %d", n)
	}
	// Unknown ID — zero drops, no error.
	if dropped := mgr.InvalidateByIdentity("nobody"); dropped != 0 {
		t.Errorf("unknown id should drop 0, got %d", dropped)
	}
}

func TestDashboard_AuthRoute(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: "user-" + tok, Roles: []string{"reader"}}, nil
	}
	addr := "127.0.0.1:8792"
	mgr := bootWithManager(t, addr, auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})

	// Seed two identities.
	_, _ = mgr.Resolve(context.Background(), "alpha-long-token")
	_, _ = mgr.Resolve(context.Background(), "beta-long-token")

	body, status := httpGet(t, "http://"+addr+"/__nexus/auth", "")
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var payload struct {
		Identities     []auth.CachedIdentity `json:"identities"`
		CachingEnabled bool                  `json:"cachingEnabled"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&payload); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
	if !payload.CachingEnabled {
		t.Error("cachingEnabled should be true")
	}
	if len(payload.Identities) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(payload.Identities))
	}
	for _, id := range payload.Identities {
		// Redaction sanity: no full token should ever appear.
		if strings.Contains(id.TokenPrefix, "long-token") {
			t.Errorf("full token leaked: %q", id.TokenPrefix)
		}
	}
}

func TestDashboard_AuthInvalidate(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: "user-X"}, nil
	}
	addr := "127.0.0.1:8791"
	mgr := bootWithManager(t, addr, auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})
	for _, tok := range []string{"t-1", "t-2", "t-3"} {
		_, _ = mgr.Resolve(context.Background(), tok)
	}

	payload, _ := json.Marshal(map[string]string{"id": "user-X"})
	res, err := http.Post("http://"+addr+"/__nexus/auth/invalidate",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	var out struct {
		Dropped int `json:"dropped"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out.Dropped != 3 {
		t.Errorf("expected 3 dropped, got %d", out.Dropped)
	}
	if n := len(mgr.Identities()); n != 0 {
		t.Errorf("cache not cleared: %d remaining", n)
	}
}

func TestRejectEvent_FiresOnUnauthenticated(t *testing.T) {
	// Wire an auth-gated REST route, hit it without a token, and
	// confirm the global trace bus sees an auth.reject event with
	// reason=unauthenticated. Uses a Run-based boot because the bus
	// lives on *App behind the trace middleware.
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: "u"}, nil
	}
	addr := "127.0.0.1:8790"
	appCh := make(chan *nexus.App, 1)
	go func() {
		nexus.Run(
			nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 100},
			auth.Module(auth.Config{Resolve: resolver}),
			nexus.Invoke(func(app *nexus.App) { appCh <- app }),
			nexus.AsRest("GET", "/gated", func(ctx context.Context) (map[string]string, error) {
				return map[string]string{"ok": "yes"}, nil
			}, auth.Required()),
		)
	}()
	var app *nexus.App
	select {
	case app = <-appCh:
	case <-time.After(3 * time.Second):
		t.Fatal("app never booted")
	}
	// Wait for listener.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Subscribe to the trace bus BEFORE the rejected request so we
	// capture the auth.reject event as it's published.
	_, ch, cancel := app.Bus().Subscribe(0, 16)
	defer cancel()

	_, status := httpGet(t, "http://"+addr+"/gated", "")
	if status != 401 {
		t.Fatalf("expected 401, got %d", status)
	}

	// Drain events with a timeout looking for auth.reject.
	deadline2 := time.After(1 * time.Second)
	found := false
	for !found {
		select {
		case e := <-ch:
			if e.Kind == "auth.reject" {
				if reason, _ := e.Meta["reason"].(string); reason != "unauthenticated" {
					t.Errorf("expected reason=unauthenticated, got %v", e.Meta["reason"])
				}
				found = true
			}
		case <-deadline2:
			t.Fatal("auth.reject event not published within 1s")
		}
	}
}