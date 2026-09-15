package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// bootApp builds an in-process app with the session module and a few
// probe routes, returning the handler and stop func.
func bootApp(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	app, stop, err := nexus.InProcess(nexus.Config{},
		Module(cfg),
		nexus.AsRestHandler("GET", "/read", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				s := Get(c.Request.Context())
				c.String(200, s.GetString("who"))
			}
		}),
		nexus.AsRestHandler("POST", "/write", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				s := Get(c.Request.Context())
				s.Set("who", "alice")
				c.String(200, "ok")
			}
		}),
		nexus.AsRestHandler("POST", "/cycle", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				s := Get(c.Request.Context())
				s.Cycle()
				c.String(200, s.ID())
			}
		}),
		nexus.AsRestHandler("POST", "/logout", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				Get(c.Request.Context()).Destroy()
				c.String(200, "bye")
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

func do(t *testing.T, h http.Handler, method, path, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSession_Lifecycle(t *testing.T) {
	h := bootApp(t, Config{})

	// Anonymous read: no session created, no cookie set.
	w := do(t, h, "GET", "/read", "")
	if w.Body.String() != "" {
		t.Fatalf("fresh read = %q", w.Body.String())
	}
	if c := sessionCookie(t, w, defaultCookie); c != nil {
		t.Fatalf("read-only request must not set a cookie, got %v", c)
	}

	// First write mints the ID and sets the cookie.
	w = do(t, h, "POST", "/write", "")
	ck := sessionCookie(t, w, defaultCookie)
	if ck == nil {
		t.Fatal("write must set the session cookie")
	}
	if !ck.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", ck.SameSite)
	}
	if !validID(ck.Value) {
		t.Fatalf("cookie value %q is not a session ID", ck.Value)
	}

	// The data comes back on the next request carrying the cookie…
	w = do(t, h, "GET", "/read", defaultCookie+"="+ck.Value)
	if w.Body.String() != "alice" {
		t.Fatalf("read after write = %q, want alice", w.Body.String())
	}
	// …and a read does not re-set the cookie.
	if c := sessionCookie(t, w, defaultCookie); c != nil {
		t.Fatal("read must not refresh the cookie")
	}

	// A foreign cookie value is ignored, not sent to the store.
	w = do(t, h, "GET", "/read", defaultCookie+"=../../etc/passwd")
	if w.Body.String() != "" {
		t.Fatalf("foreign cookie read = %q", w.Body.String())
	}
}

func TestSession_CycleKeepsDataRotatesID(t *testing.T) {
	h := bootApp(t, Config{})

	w := do(t, h, "POST", "/write", "")
	old := sessionCookie(t, w, defaultCookie)

	w = do(t, h, "POST", "/cycle", defaultCookie+"="+old.Value)
	fresh := sessionCookie(t, w, defaultCookie)
	if fresh == nil || fresh.Value == old.Value {
		t.Fatalf("Cycle must rotate the ID (old=%s new=%v)", old.Value, fresh)
	}

	// Data survived under the new ID; the old ID is dead.
	if got := do(t, h, "GET", "/read", defaultCookie+"="+fresh.Value).Body.String(); got != "alice" {
		t.Fatalf("data lost on cycle: %q", got)
	}
	if got := do(t, h, "GET", "/read", defaultCookie+"="+old.Value).Body.String(); got != "" {
		t.Fatalf("old ID still resolves after cycle: %q", got)
	}
}

func TestSession_Destroy(t *testing.T) {
	h := bootApp(t, Config{})

	w := do(t, h, "POST", "/write", "")
	ck := sessionCookie(t, w, defaultCookie)

	w = do(t, h, "POST", "/logout", defaultCookie+"="+ck.Value)
	gone := sessionCookie(t, w, defaultCookie)
	if gone == nil || gone.MaxAge >= 0 || gone.Value != "" {
		t.Fatalf("Destroy must expire the cookie, got %+v", gone)
	}
	if got := do(t, h, "GET", "/read", defaultCookie+"="+ck.Value).Body.String(); got != "" {
		t.Fatalf("session readable after Destroy: %q", got)
	}
}

func TestSession_InertWithoutModule(t *testing.T) {
	s := Get(context.Background())
	s.Set("k", "v") // must not panic and must not stick
	if s.Get("k") != nil {
		t.Fatal("inert session must drop writes")
	}
	s.Cycle()
	s.Destroy()
	if s.ID() != "" {
		t.Fatal("inert session must have no ID")
	}
}

func TestMemoryStore_TTLAndDevState(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()

	if err := m.Save(ctx, "live", map[string]any{"a": "b"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, "dead", map[string]any{"x": "y"}, -time.Second); err != nil {
		t.Fatal(err)
	}
	if d, _ := m.Load(ctx, "dead"); d != nil {
		t.Fatal("expired entry must load as absent")
	}
	if d, _ := m.Load(ctx, "live"); d == nil || d["a"] != "b" {
		t.Fatalf("live entry = %v", d)
	}

	// Load hands out a copy — mutating it must not corrupt the store.
	d, _ := m.Load(ctx, "live")
	d["a"] = "mutated"
	if d2, _ := m.Load(ctx, "live"); d2["a"] != "b" {
		t.Fatal("Load must return an isolated copy")
	}

	// Dev-state round trip drops expired entries and merges losing to
	// entries the new process already wrote.
	snap, err := m.SnapshotDev()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snap), `"dead"`) {
		t.Fatal("snapshot must drop expired entries")
	}
	m2 := NewMemoryStore()
	_ = m2.Save(ctx, "live", map[string]any{"a": "newer"}, time.Hour)
	if err := m2.RestoreDev(snap); err != nil {
		t.Fatal(err)
	}
	if d, _ := m2.Load(ctx, "live"); d["a"] != "newer" {
		t.Fatalf("restore must not clobber the new process's write, got %v", d)
	}
}
