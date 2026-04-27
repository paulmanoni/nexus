package auth_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/auth"
)

// bootWithManager starts a nexus.Run app on a test port and captures the
// *auth.Manager into a shared channel. The captured manager lets tests
// call Invalidate / InvalidateAll / Resolve / Identities directly
// against live auth state without going through HTTP.
func bootWithManager(t *testing.T, addr string, cfg auth.Config, extra ...nexus.Option) *auth.Manager {
	t.Helper()
	mgrCh := make(chan *auth.Manager, 1)
	opts := append([]nexus.Option{
		auth.Module(cfg),
		nexus.Invoke(func(m *auth.Manager) { mgrCh <- m }),
	}, extra...)
	go func() {
		nexus.Run(nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 10}, opts...)
	}()
	select {
	case m := <-mgrCh:
		// Wait for HTTP listener — one GET on a known ping path
		// before returning so subsequent test requests don't race.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
				return m
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("app started but HTTP listener didn't bind within 3s")
	case <-time.After(3 * time.Second):
		t.Fatal("manager never wired; fx boot stalled")
	}
	return nil
}

func TestManager_InvalidateDropsCache(t *testing.T) {
	var calls int32
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		atomic.AddInt32(&calls, 1)
		return &auth.Identity{ID: tok}, nil
	}
	mgr := bootWithManager(t, "127.0.0.1:8797", auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})

	ctx := context.Background()
	_, _ = mgr.Resolve(ctx, "abc")
	_, _ = mgr.Resolve(ctx, "abc")
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("cache hit failed: calls=%d want=1", c)
	}
	mgr.Invalidate("abc")
	_, _ = mgr.Resolve(ctx, "abc")
	if c := atomic.LoadInt32(&calls); c != 2 {
		t.Fatalf("invalidate did not force re-resolve: calls=%d want=2", c)
	}
}

func TestManager_InvalidateAllClears(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	mgr := bootWithManager(t, "127.0.0.1:8796", auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})
	for _, tok := range []string{"a", "b", "c"} {
		_, _ = mgr.Resolve(context.Background(), tok)
	}
	if n := len(mgr.Identities()); n != 3 {
		t.Fatalf("expected 3 cached, got %d", n)
	}
	mgr.InvalidateAll()
	if n := len(mgr.Identities()); n != 0 {
		t.Fatalf("InvalidateAll should clear, got %d", n)
	}
}

func TestManager_IdentitiesRedactsTokens(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	mgr := bootWithManager(t, "127.0.0.1:8795", auth.Config{
		Resolve: resolver,
		Cache:   auth.CacheFor(5 * time.Minute),
	})
	longToken := "very-long-secret-token-xyz123"
	_, _ = mgr.Resolve(context.Background(), longToken)
	ids := mgr.Identities()
	if len(ids) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(ids))
	}
	if ids[0].TokenPrefix == longToken {
		t.Fatal("full token leaked via Identities()")
	}
	if !strings.HasSuffix(ids[0].TokenPrefix, "…") {
		t.Errorf("expected ellipsis suffix, got %q", ids[0].TokenPrefix)
	}
}

func TestOnUnauthenticated_CustomEnvelope(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	addr := "127.0.0.1:8794"
	_ = bootWithManager(t, addr, auth.Config{
		Resolve: resolver,
		OnUnauthenticated: func(c *gin.Context, err error) {
			c.AbortWithStatusJSON(401, gin.H{
				"success": false,
				"error":   err.Error(),
				"code":    "UNAUTH",
			})
		},
	}, nexus.AsRest("GET", "/gated", func(ctx context.Context) (map[string]string, error) {
		return map[string]string{"ok": "yes"}, nil
	}, auth.Required()))

	body, status := httpGet(t, "http://"+addr+"/gated", "")
	if status != 401 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"success":false`) || !strings.Contains(body, `"code":"UNAUTH"`) {
		t.Errorf("custom envelope missing: %s", body)
	}
}

func TestAnyOf_PassesOnAnyMatch(t *testing.T) {
	fn := auth.AnyOf("admin", "editor")
	if !fn(&auth.Identity{Roles: []string{"editor"}}, nil) {
		t.Fatal("editor should satisfy AnyOf(admin|editor)")
	}
	if fn(&auth.Identity{Roles: []string{"viewer"}}, nil) {
		t.Fatal("viewer should NOT satisfy AnyOf(admin|editor)")
	}
	if fn(nil, nil) {
		t.Fatal("nil identity must never pass")
	}
}

func TestAllOf_RequiresEveryPerm(t *testing.T) {
	fn := auth.AllOf("a", "b")
	if !fn(&auth.Identity{Roles: []string{"a", "b", "c"}}, nil) {
		t.Fatal("a+b+c should satisfy AllOf(a,b)")
	}
	if fn(&auth.Identity{Roles: []string{"a"}}, nil) {
		t.Fatal("a alone should fail AllOf(a,b)")
	}
}