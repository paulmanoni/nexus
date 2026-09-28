package session

import (
	"context"
	"net/http"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// TestRequiredGate: session.Required() admits only requests that arrive with
// an established session — 428 for a fresh request or a bogus cookie, 200
// once a prior request stored state and handed out the cookie.
func TestRequiredGate(t *testing.T) {
	cfg := Config{Store: NewMemoryStore()}
	app, stop, err := nexus.InProcess(nexus.Config{},
		Module(cfg),
		nexus.AsRestHandler("POST", "/start", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				Get(c.Request.Context()).Set("step", 1)
				c.String(200, "started")
			}
		}),
		nexus.AsRestHandler("GET", "/gated", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) {
				if !Get(c.Request.Context()).Established() {
					t.Error("gate admitted a request Established() reports false for")
				}
				c.String(200, "in")
			}
		}, Required()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	name := cfg.withDefaults().CookieName

	// Fresh request: no session → 428, handler never runs.
	if w := do(t, app, "GET", "/gated", ""); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("fresh request: status = %d, want 428", w.Code)
	}

	// A bogus cookie is not an established session.
	if w := do(t, app, "GET", "/gated", name+"=not-a-real-id"); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("bogus cookie: status = %d, want 428", w.Code)
	}

	// Start the flow, then the gate admits the cookie-bearing request.
	start := do(t, app, "POST", "/start", "")
	ck := sessionCookie(t, start, name)
	if ck == nil {
		t.Fatal("no session cookie from /start")
	}
	if w := do(t, app, "GET", "/gated", name+"="+ck.Value); w.Code != http.StatusOK {
		t.Fatalf("established session: status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
}
