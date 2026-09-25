package inertia_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/extension/inertia/ssrhttp"
)

// devSSRServer is a Vite dev server that also renders (@inertiajs/vite's
// /__inertia_ssr), counting renders.
func devSSRServer(t *testing.T, body string) (origin string, renders *atomic.Int32) {
	t.Helper()
	renders = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@vite/client"):
			_, _ = w.Write([]byte("export {}"))
		case r.Method == http.MethodPost && r.URL.Path == "/__inertia_ssr":
			renders.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"head":[],"body":"` + body + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, renders
}

// TestSSRHTTP_FollowsTheHotFile: in development, ssrhttp renders through the
// dev server the hot file names — what `nexus dev` relies on now that it sets
// no NEXUS_VITE_DEV — and the hot file wins over a NEXUS_VITE_DEV left in the
// environment.
func TestSSRHTTP_FollowsTheHotFile(t *testing.T) {
	stale, staleRenders := devSSRServer(t, "<main>from NEXUS_VITE_DEV</main>")
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{
		SSR: ssrhttp.New("http://127.0.0.1:1"), // production target: unreachable
	})
	t.Setenv("NEXUS_VITE_DEV", stale)
	vite, renders := devSSRServer(t, "<main>from the hot file</main>")
	writeHot(t, dist, hotFor(vite, "/", "src/main.ts"))

	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, "<main>from the hot file</main>")
	if renders.Load() != 1 || staleRenders.Load() != 0 {
		t.Errorf("renders: hot-file server %d, NEXUS_VITE_DEV server %d; want 1, 0", renders.Load(), staleRenders.Load())
	}
}

// TestDevServer_UnsetOutsideARender: the context key is the engine's to set.
func TestDevServer_UnsetOutsideARender(t *testing.T) {
	if got := inertia.DevServer(context.Background()); got != "" {
		t.Fatalf("DevServer = %q on a bare context", got)
	}
}
