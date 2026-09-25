// Package ssrhttp is the default Inertia SSRRenderer: it POSTs the page object
// to a Node SSR server and decodes the {head, body} response — the
// @inertiajs/server protocol.
//
//	inertia.Module(inertia.Config{ SSR: ssrhttp.New("") }) // "" → http://127.0.0.1:13714
//
// In production the renderer targets the SSR server you run (e.g.
// `node web/dist/ssr/ssr.js`, default port 13714). In development Inertia v3
// serves SSR through the Vite dev server, so while the engine follows one —
// the dev server named by nexus-vite-plugin's hot file (App.ViteHot, handed
// to Render as inertia.DevServer) — the renderer targets that instead, and
// `nexus dev` starts no separate Node process. NEXUS_VITE_DEV, when set,
// names a dev server for setups without the hot file. On any transport
// error the engine falls back to client-side rendering.
package ssrhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/extension/inertia"
)

// DefaultURL is the conventional @inertiajs/server production port.
const DefaultURL = "http://127.0.0.1:13714"

// devURLEnv mirrors inertia's: a dev server URL set by hand, the fallback
// when no hot file names one. `nexus dev` no longer sets it.
const devURLEnv = "NEXUS_VITE_DEV"

// devSSRPath is the SSR endpoint the Vite dev server exposes.
const devSSRPath = "/__inertia_ssr"

// Renderer is an inertia.SSRRenderer backed by an HTTP SSR server.
type Renderer struct {
	prodURL string
	client  *http.Client
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithHTTPClient overrides the HTTP client (e.g. for a custom timeout).
func WithHTTPClient(c *http.Client) Option { return func(r *Renderer) { r.client = c } }

// New returns a Renderer targeting prodURL (default DefaultURL when empty).
func New(prodURL string, opts ...Option) *Renderer {
	if prodURL == "" {
		prodURL = DefaultURL
	}
	r := &Renderer{
		prodURL: strings.TrimRight(prodURL, "/"),
		// A short timeout: SSR must never hold a request open — on timeout the
		// engine falls back to client rendering.
		client: &http.Client{Timeout: 5 * time.Second},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Render POSTs the page object and decodes the SSR result. In dev it targets the
// Vite dev server's SSR endpoint; otherwise the production SSR server.
func (r *Renderer) Render(ctx context.Context, page []byte) (inertia.SSRResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url(ctx), bytes.NewReader(page))
	if err != nil {
		return inertia.SSRResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return inertia.SSRResult{}, err
	}
	defer resp.Body.Close()
	var out inertia.SSRResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return inertia.SSRResult{}, err
	}
	return out, nil
}

// url picks the endpoint: the dev server the engine follows (its hot file),
// else NEXUS_VITE_DEV, else the production SSR server.
func (r *Renderer) url(ctx context.Context) string {
	if dev := inertia.DevServer(ctx); dev != "" {
		return strings.TrimRight(dev, "/") + devSSRPath
	}
	if dev := strings.TrimRight(os.Getenv(devURLEnv), "/"); dev != "" {
		return dev + devSSRPath
	}
	return r.prodURL + "/render"
}
