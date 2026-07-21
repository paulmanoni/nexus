package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/paulmanoni/nexus/httpx"
)

// buildReverseProxy constructs a single *httputil.ReverseProxy targeting the
// upstream base URL, shared across every route that forwards there. Stdlib
// only — no third-party proxy dependency.
//
// Header handling: ReverseProxy copies the inbound request wholesale, so
// Authorization / Cookie / CSRF headers flow to the upstream unchanged — which
// is exactly what a strangler migration needs (the legacy app keeps doing its
// own auth). NewSingleHostReverseProxy leaves req.Host as the inbound Host, so
// the upstream sees the original host (Django ALLOWED_HOSTS / cookie-domain /
// CSRF stay happy). setHeaders adds/overrides request headers; rewritePath, if
// set, rewrites the path before forwarding (e.g. strip a dev-only prefix).
func buildReverseProxy(upstream string, setHeaders map[string]string, rewritePath func(string) string, transport http.RoundTripper) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream %q: %w", upstream, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("proxy: upstream %q must be an absolute URL (scheme://host)", upstream)
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)          // scheme/host + joins target.Path with inbound path
			pr.Out.Host = pr.In.Host   // preserve original Host (upstream ALLOWED_HOSTS/CSRF)
			pr.SetXForwarded()         // X-Forwarded-For/Host/Proto for the upstream
			if rewritePath != nil {
				pr.Out.URL.Path = rewritePath(pr.Out.URL.Path)
			}
			for k, v := range setHeaders {
				pr.Out.Header.Set(k, v)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Observability breadcrumb: served by forwarding, not a native handler.
			resp.Header.Set("X-Nexus-Proxied", "1")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			// Upstream down / dial failure → 502 with a JSON body matching
			// nexus's default error shape (ReverseProxy defaults to plaintext).
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
		},
	}
	if transport != nil {
		rp.Transport = transport
	}
	return rp, nil
}

// proxyHandlerFunc adapts a ReverseProxy to an httpx.HandlerFunc. httpx.Ctx's
// Writer embeds http.ResponseWriter and Request is the live *http.Request, so
// the forward is a direct ServeHTTP with no adapter allocation per request.
func proxyHandlerFunc(rp *httputil.ReverseProxy) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		rp.ServeHTTP(c.Writer, c.Request)
	}
}
