package auth

import "net/http"

// ExtractorInfo describes how a configured Extractor pulls a token
// off an inbound request — surfaced in the client SDK manifest so a
// browser bundle knows where to put the access token (header /
// cookie / both).
//
// Strategy values:
//
//	"bearer"  HeaderName: "Authorization" (or override via custom
//	          extractor); SDK sends "Authorization: Bearer <token>".
//	"cookie"  CookieName populated; SDK lets the browser send the
//	          cookie automatically (just sets credentials: 'include').
//	"apikey"  HeaderName populated; SDK sends a bare header value.
//	"chain"   Chain populated with the underlying strategies in order.
//	"custom"  user-supplied extractor that doesn't satisfy Describable;
//	          SDK falls back to "include credentials" semantics and
//	          relies on the app to populate the request.
type ExtractorInfo struct {
	Strategy   string          `json:"strategy"`
	HeaderName string          `json:"headerName,omitempty"`
	CookieName string          `json:"cookieName,omitempty"`
	Chain      []ExtractorInfo `json:"chain,omitempty"`
}

// Describable is the optional contract Extractors implement to
// surface their config to the client SDK manifest. Built-in
// Bearer / Cookie / APIKey / Chain implement it; user-defined
// extractors that don't satisfy it surface as Strategy="custom".
type Describable interface {
	Describe() ExtractorInfo
}

// Describe returns the extractor's config as an ExtractorInfo.
// Resolves user-defined extractors that don't implement Describable
// to Strategy="custom" so the SDK has a deterministic fallback.
func Describe(e Extractor) ExtractorInfo {
	if e == nil {
		return ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"}
	}
	if d, ok := e.(Describable); ok {
		return d.Describe()
	}
	return ExtractorInfo{Strategy: "custom"}
}

// Info returns the auth module's runtime extractor configuration.
// Used by the client SDK at manifest-build time so the generated
// JS knows where to attach tokens. Returns the default Bearer
// shape when no extractor was configured (matches the runtime
// fallback in Module).
func (m *Manager) Info() ExtractorInfo {
	if m == nil || m.state == nil {
		return ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"}
	}
	return Describe(m.state.cfg.Extract)
}

// describable wrappers — internal types whose Describe() returns the
// matching ExtractorInfo. Built-in Bearer / Cookie / APIKey / Chain
// constructors return these so type-asserting via Describable in the
// SDK picks them up automatically.

type bearerExtractor struct{ Extractor }

func (bearerExtractor) Describe() ExtractorInfo {
	return ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"}
}

type cookieExtractor struct {
	Extractor
	name string
}

func (c cookieExtractor) Describe() ExtractorInfo {
	return ExtractorInfo{Strategy: "cookie", CookieName: c.name}
}

type apiKeyExtractor struct {
	Extractor
	header string
}

func (a apiKeyExtractor) Describe() ExtractorInfo {
	return ExtractorInfo{Strategy: "apikey", HeaderName: a.header}
}

type chainExtractor struct {
	Extractor
	parts []Extractor
}

func (c chainExtractor) Describe() ExtractorInfo {
	out := ExtractorInfo{Strategy: "chain"}
	for _, p := range c.parts {
		out.Chain = append(out.Chain, Describe(p))
	}
	return out
}

// keep the http import live for the file-as-package compile graph;
// describe types reference Extractor which uses *http.Request.
var _ *http.Request