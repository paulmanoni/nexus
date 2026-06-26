package inertia_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

// fakeSSR is an inertia.SSRRenderer that returns a canned result (or error) and
// counts calls — to assert SSR runs only on the initial load, not XHR visits.
type fakeSSR struct {
	res   inertia.SSRResult
	err   error
	calls int32
}

func (f *fakeSSR) Render(ctx context.Context, page []byte) (inertia.SSRResult, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.res, f.err
}

func bootSSR(t *testing.T, addr string, ssr inertia.SSRRenderer, onErr func(error), strict bool) {
	t.Helper()
	fsys := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
	ready := make(chan struct{})
	go func() {
		nexus.Run(nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 10},
			inertia.Module(inertia.Config{Frontend: fsys, Root: "dist", SSR: ssr, OnSSRError: onErr, SSRStrict: strict}),
			inertia.Page("GET", "/p", "P", NewWidgets),
			nexus.Invoke(func() { close(ready) }),
		)
	}()
	<-ready
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("ssr app didn't bind HTTP within 3s")
}

// TestSSR_FullLoadInjectsAndXHRSkips asserts SSR runs on the initial HTML load
// (head + body injected, root flagged) but NOT on XHR visits (JSON only).
func TestSSR_FullLoadInjectsAndXHRSkips(t *testing.T) {
	addr := "127.0.0.1:8841"
	f := &fakeSSR{res: inertia.SSRResult{Head: []string{"<title>SSR</title>"}, Body: "<main>server</main>"}}
	bootSSR(t, addr, f, nil, false)

	// XHR visit: JSON page object, SSR not invoked.
	_, xhr := req(t, addr, "/p", map[string]string{"X-Inertia": "true"})
	if strings.Contains(xhr, "<main>server</main>") {
		t.Fatalf("XHR response must be JSON, not SSR HTML: %s", xhr)
	}
	if n := atomic.LoadInt32(&f.calls); n != 0 {
		t.Fatalf("SSR must be skipped on XHR visits, got %d calls", n)
	}

	// Full load: SSR head hoisted, body inside the flagged root div.
	_, full := req(t, addr, "/p", nil)
	if !strings.Contains(full, "<title>SSR</title>") {
		t.Errorf("SSR head not injected: %s", full)
	}
	if !strings.Contains(full, `data-server-rendered="true"`) {
		t.Errorf("root not flagged for hydration: %s", full)
	}
	if !strings.Contains(full, "<main>server</main></div>") {
		t.Errorf("SSR body not inside root div: %s", full)
	}
	if n := atomic.LoadInt32(&f.calls); n != 1 {
		t.Fatalf("SSR should run once on full load, got %d", n)
	}
}

// TestSSR_FallbackOnError asserts a renderer error degrades to client rendering
// (200, empty root) and fires OnSSRError.
func TestSSR_FallbackOnError(t *testing.T) {
	addr := "127.0.0.1:8842"
	var gotErr atomic.Value
	f := &fakeSSR{err: errors.New("ssr down")}
	bootSSR(t, addr, f, func(e error) { gotErr.Store(e.Error()) }, false)

	res, body := req(t, addr, "/p", nil)
	if res.StatusCode != 200 {
		t.Fatalf("SSR failure must fall back to a 200 client render, got %d", res.StatusCode)
	}
	if strings.Contains(body, "data-server-rendered") {
		t.Errorf("fallback must leave the root unflagged: %s", body)
	}
	if v, _ := gotErr.Load().(string); v != "ssr down" {
		t.Errorf("OnSSRError not called with the failure; got %q", v)
	}
}

// TestSSR_Strict asserts SSRStrict turns a renderer error into a 500 instead of
// a silent fallback.
func TestSSR_Strict(t *testing.T) {
	addr := "127.0.0.1:8843"
	f := &fakeSSR{err: errors.New("boom")}
	bootSSR(t, addr, f, nil, true)

	res, _ := req(t, addr, "/p", nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("SSRStrict should 500 on renderer error, got %d", res.StatusCode)
	}
}
