package auth

import (
	"net/http"
	"strings"
)

// Extractor pulls a raw token from an HTTP request. Returns ("", false)
// when no token is present — callers treat that as "anonymous request"
// rather than an error, so public endpoints still resolve a nil Identity
// and proceed.
type Extractor interface {
	Extract(r *http.Request) (string, bool)
}

// ExtractorFunc adapts a plain function to the Extractor interface.
type ExtractorFunc func(r *http.Request) (string, bool)

// Extract satisfies the Extractor interface for ExtractorFunc.
func (f ExtractorFunc) Extract(r *http.Request) (string, bool) { return f(r) }

// Bearer reads "Authorization: Bearer <token>". Case-insensitive on the
// scheme name per RFC 7235. Empty tokens ("Bearer ") are treated as
// absent so downstream Required() correctly fires 401.
func Bearer() Extractor {
	return ExtractorFunc(func(r *http.Request) (string, bool) {
		h := r.Header.Get("Authorization")
		if h == "" {
			return "", false
		}
		const prefix = "bearer "
		if len(h) < len(prefix) {
			return "", false
		}
		if !strings.EqualFold(h[:len(prefix)], prefix) {
			return "", false
		}
		tok := strings.TrimSpace(h[len(prefix):])
		if tok == "" {
			return "", false
		}
		return tok, true
	})
}

// Cookie reads the value of a named cookie (typically a session ID).
// Non-existent cookie → ("", false), same treatment as a missing Bearer
// header, so public routes keep working.
func Cookie(name string) Extractor {
	return ExtractorFunc(func(r *http.Request) (string, bool) {
		c, err := r.Cookie(name)
		if err != nil || c.Value == "" {
			return "", false
		}
		return c.Value, true
	})
}

// APIKey reads a named header (e.g. "X-API-Key"). Trailing whitespace
// stripped for ergonomics; empty values are treated as absent.
func APIKey(header string) Extractor {
	return ExtractorFunc(func(r *http.Request) (string, bool) {
		v := strings.TrimSpace(r.Header.Get(header))
		if v == "" {
			return "", false
		}
		return v, true
	})
}

// Chain runs extractors in order and returns the first hit. Useful when
// an app accepts both a Bearer token (programmatic clients) and a
// session cookie (browser clients) on the same handler:
//
//	auth.Chain(auth.Bearer(), auth.Cookie("session"))
func Chain(extractors ...Extractor) Extractor {
	return ExtractorFunc(func(r *http.Request) (string, bool) {
		for _, e := range extractors {
			if tok, ok := e.Extract(r); ok {
				return tok, true
			}
		}
		return "", false
	})
}