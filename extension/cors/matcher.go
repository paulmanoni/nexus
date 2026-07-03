package cors

import "strings"

// matcher decides whether an Origin is allowed by the policy and, if
// so, what to echo back in Access-Control-Allow-Origin. Built once
// from Config at boot to amortize string-parse cost across requests.
//
// Three strategies, picked at build time:
//
//   - wildcard:  any origin → reflect "*" (only when no credentials)
//   - func:      Config.AllowOriginFunc owns the decision
//   - list:      pre-parsed entries; each is either exact or has a
//     single-segment wildcard left of the dot-separated
//     hostname (e.g. "*.example.com")
//
// The decision is hot — happens per request — so we keep allocations
// out of the per-request path. allowedHosts is pre-split so the
// runtime check is a couple of substring/equality calls.
type matcher struct {
	wildcard bool // "*" present → allow all
	fn       func(origin string) (allow bool, matched string)
	exact    map[string]struct{} // origins matched literally
	wild     []wildHost          // *.example.com style
}

// wildHost is a parsed subdomain-wildcard entry. Schema + suffix
// pair lets the per-request check stay branch-free.
//
// "https://*.example.com" → {scheme: "https://", suffix: ".example.com"}
//
// An origin matches when it shares the scheme AND its host ends in
// `suffix` AND the part before suffix has no further dots (so
// "*.example.com" matches "a.example.com" but NOT "a.b.example.com" —
// the spec leaves this open; we take the stricter reading).
type wildHost struct {
	scheme string
	suffix string
}

func buildMatcher(cfg *Config) matcher {
	if cfg.AllowOriginFunc != nil {
		return matcher{fn: cfg.AllowOriginFunc}
	}
	m := matcher{
		exact: make(map[string]struct{}, len(cfg.AllowOrigins)),
	}
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			m.wildcard = true
			continue
		}
		if w, ok := parseWildHost(o); ok {
			m.wild = append(m.wild, w)
			continue
		}
		m.exact[o] = struct{}{}
	}
	return m
}

// parseWildHost recognizes "<scheme>://*.<suffix>" entries. Returns
// the split + true on match, or zero + false otherwise. The caller
// then routes the entry to exact/wild as appropriate.
func parseWildHost(o string) (wildHost, bool) {
	for _, scheme := range []string{"https://", "http://"} {
		if !strings.HasPrefix(o, scheme) {
			continue
		}
		host := o[len(scheme):]
		if !strings.HasPrefix(host, "*.") {
			return wildHost{}, false
		}
		return wildHost{scheme: scheme, suffix: host[1:]}, true // suffix = ".example.com"
	}
	return wildHost{}, false
}

// match returns (allow, echo). When allow=true, echo is the origin
// the response should reflect in Access-Control-Allow-Origin — for
// list-based matchers we always reflect the actual request origin so
// vary-based caching works; for the wildcard branch we echo "*"
// unless credentials are involved (validated at config time).
//
// Empty origin (non-cross-origin requests) is signaled by
// allow=false, echo="" — caller skips CORS headers entirely. The
// browser doesn't apply CORS to same-origin requests, so this is
// the correct no-op.
func (m matcher) match(origin string) (allow bool, echo string) {
	if origin == "" {
		return false, ""
	}
	if m.fn != nil {
		return m.fn(origin)
	}
	if m.wildcard {
		return true, "*"
	}
	if _, ok := m.exact[origin]; ok {
		return true, origin
	}
	for _, w := range m.wild {
		if !strings.HasPrefix(origin, w.scheme) {
			continue
		}
		host := origin[len(w.scheme):]
		if !strings.HasSuffix(host, w.suffix) {
			continue
		}
		// Reject multi-level subdomains: a.b.example.com would
		// pass HasSuffix(".example.com") but the bit BEFORE suffix
		// ("a.b") contains a dot — reject. Single-level is
		// "*.example.com" matches "a.example.com" only.
		prefix := host[:len(host)-len(w.suffix)]
		if strings.Contains(prefix, ".") {
			continue
		}
		return true, origin
	}
	return false, ""
}
