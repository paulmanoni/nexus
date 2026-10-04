package auth

import (
	"net/http"
	"strings"
)

// extractor finds a scheme's credential on a request.
type extractor interface {
	extract(r *http.Request) (string, bool)
}

type extractorFunc func(r *http.Request) (string, bool)

func (f extractorFunc) extract(r *http.Request) (string, bool) { return f(r) }

// bearerShaped reads "Authorization: Bearer <token>" for one token shape: a
// JWT (three dot-separated parts) for a jwt scheme, an opaque nexus token
// (no dot) for a bearer one — so the two share the header without claiming
// each other's tokens.
func bearerShaped(jwt bool) extractor {
	return extractorFunc(func(r *http.Request) (string, bool) {
		h := r.Header.Get("Authorization")
		const prefix = "bearer "
		if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
			return "", false
		}
		tok := strings.TrimSpace(h[len(prefix):])
		if tok == "" || (strings.Count(tok, ".") == 2) != jwt {
			return "", false
		}
		return tok, true
	})
}

// apiKey reads a header.
func apiKey(header string) extractor {
	return extractorFunc(func(r *http.Request) (string, bool) {
		v := strings.TrimSpace(r.Header.Get(header))
		return v, v != ""
	})
}
