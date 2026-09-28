package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

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
//
// upstreamHost flips the Host choice for name-based virtual hosting (shared
// hosting, cPanel/ISPConfig, a CDN): the upstream sees its own host so the
// right vhost answers, and the response is made to point back at the proxy —
// absolute Location redirects to the upstream origin become path-only, and a
// Set-Cookie Domain naming the upstream host is dropped so the browser keeps
// the cookie on the proxy's domain.
func buildReverseProxy(upstream string, setHeaders map[string]string, rewritePath func(string) string, upstreamHost bool, transport http.RoundTripper) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream %q: %w", upstream, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("proxy: upstream %q must be an absolute URL (scheme://host)", upstream)
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme/host + joins target.Path with inbound path
			if upstreamHost {
				pr.Out.Host = target.Host
			} else {
				pr.Out.Host = pr.In.Host // preserve original Host (upstream ALLOWED_HOSTS/CSRF)
			}
			pr.SetXForwarded() // X-Forwarded-For/Host/Proto for the upstream
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
			if upstreamHost {
				rewriteUpstreamRefs(resp.Header, target)
			}
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

// rewriteUpstreamRefs points upstream-origin references in a response back at
// the proxy (see buildReverseProxy's upstreamHost).
func rewriteUpstreamRefs(h http.Header, target *url.URL) {
	if loc := h.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil && u.IsAbs() && strings.EqualFold(u.Host, target.Host) {
			u.Scheme, u.Host, u.User = "", "", nil
			if u.Path == "" {
				u.Path = "/"
			}
			h.Set("Location", u.String())
		}
	}
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	host := strings.ToLower(target.Hostname())
	out := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts := strings.Split(c, ";")
		kept := parts[:1]
		for _, a := range parts[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(a), "=")
			if strings.EqualFold(k, "domain") {
				d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "."))
				if d == host || strings.HasSuffix(host, "."+d) {
					continue
				}
			}
			kept = append(kept, a)
		}
		out = append(out, strings.Join(kept, ";"))
	}
	h.Del("Set-Cookie")
	for _, c := range out {
		h.Add("Set-Cookie", c)
	}
}
