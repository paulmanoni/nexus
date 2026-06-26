// Package ssrhttp is the default Inertia SSRRenderer: it POSTs the page object
// to a Node SSR server and decodes the {head, body} response — the
// @inertiajs/server protocol.
//
//	inertia.Module(inertia.Config{ SSR: ssrhttp.New("") }) // "" → http://127.0.0.1:13714
//
// In production the renderer targets the SSR server you run (e.g.
// `node web/dist/ssr/ssr.js`, default port 13714). In development Inertia v3
// serves SSR through the Vite dev server, so when NEXUS_VITE_DEV is set the
// renderer targets that instead — `nexus dev` starts no separate Node process.
// On any transport error the engine falls back to client-side rendering.
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

// devURLEnv mirrors inertia's: set by `nexus dev` to the Vite/viteless dev
// server URL, which (with @inertiajs/vite) also serves SSR.
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
	url := r.prodURL + "/render"
	if dev := strings.TrimRight(os.Getenv(devURLEnv), "/"); dev != "" {
		url = dev + devSSRPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(page))
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
