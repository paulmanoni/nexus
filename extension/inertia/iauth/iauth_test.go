package iauth_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/extension/inertia/iauth"
	"github.com/paulmanoni/nexus/middleware"
)

const manifestJSON = `{"src/main.ts":{"file":"assets/m.js","isEntry":true}}`

func okHandler(ctx context.Context) (map[string]string, error) {
	return map[string]string{"ok": "y"}, nil
}

// apiErrs is the app-specific JSON envelope iauth delegates to for API clients.
type apiErrs struct{}

func (apiErrs) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	return rc.RejectJSON(http.StatusUnauthorized, map[string]any{"envelope": "unauth"})
}
func (apiErrs) Forbidden(rc *middleware.RequestCtx, err error) error {
	return rc.RejectJSON(http.StatusForbidden, map[string]any{"envelope": "forbidden"})
}

// TestErrorHandler_BranchesByRequestKind boots a deny-by-default app with a
// gated Inertia page and asserts iauth renders each denial kind correctly:
// Inertia XHR → 409+Location, full-page → 302+Location, API → app envelope.
func TestErrorHandler_BranchesByRequestKind(t *testing.T) {
	addr := "127.0.0.1:8831"
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	fsys := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
	ready := make(chan struct{})
	go func() {
		nexus.Run(nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 5},
			inertia.Module(inertia.Config{Frontend: fsys, Root: "dist"}),
			auth.Module(auth.Config{
				Authentication: auth.Authentication{Schemes: []auth.Scheme{{Resolve: resolver}}},
				Authorization:  auth.Authorization{Default: auth.Authenticated()},
				OnError:        iauth.ErrorHandler("/login", apiErrs{}),
			}),
			inertia.Page("GET", "/secret", "Secret", okHandler),
			nexus.Invoke(func() { close(ready) }),
		)
	}()
	<-ready
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	do := func(accept, inertiaHdr string) (*http.Response, string) {
		r, _ := http.NewRequest("GET", "http://"+addr+"/secret", nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		if inertiaHdr != "" {
			r.Header.Set("X-Inertia", inertiaHdr)
		}
		res, err := noFollow.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	// Inertia XHR visit → 409 + X-Inertia-Location.
	if res, _ := do("", "true"); res.StatusCode != http.StatusConflict || res.Header.Get("X-Inertia-Location") != "/login" {
		t.Fatalf("inertia: status=%d loc=%q", res.StatusCode, res.Header.Get("X-Inertia-Location"))
	}
	// Full-page browser load → 302 to login.
	if res, _ := do("text/html", ""); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login" {
		t.Fatalf("html: status=%d loc=%q", res.StatusCode, res.Header.Get("Location"))
	}
	// API/SDK client → the app's JSON envelope (delegated).
	if res, body := do("application/json", ""); res.StatusCode != http.StatusUnauthorized || !contains(body, `"envelope":"unauth"`) {
		t.Fatalf("api: status=%d body=%s", res.StatusCode, body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
