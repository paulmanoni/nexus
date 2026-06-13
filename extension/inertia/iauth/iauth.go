// Package iauth bridges nexus's auth extension to Inertia: an auth.ErrorHandler
// that renders auth denials the way each caller expects — a redirect for page
// navigations (Inertia visits and full-page loads), the error in the array for
// GraphQL, and JSON for API/SDK clients. It lives in its own package so apps
// using inertia without auth don't pull the auth extension (and its
// dependencies) into the build.
//
//	import "github.com/paulmanoni/nexus/extension/inertia/iauth"
//
//	auth.Module(auth.Config{
//	    Authentication: auth.Authentication{Schemes: ...},
//	    // Page nav → /login redirect; GraphQL → error; API → app envelope.
//	    OnError: iauth.ErrorHandler("/login", apiErrors{}),
//	})
package iauth

import (
	"net/http"
	"strings"

	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/middleware"
)

const (
	headerInertia  = "X-Inertia"
	headerLocation = "X-Inertia-Location"
)

// errorHandler implements auth.ErrorHandler with Inertia-aware rendering.
type errorHandler struct {
	loginURL string
	// api handles denials for non-page, non-GraphQL requests (programmatic
	// API/SDK clients). Lets an app keep its own JSON envelope; nil falls back
	// to the framework's default {"error": ...} body.
	api auth.ErrorHandler
}

// ErrorHandler returns an auth.ErrorHandler that branches by request kind:
//
//   - Page navigation (an Inertia visit, or a full-page browser load) →
//     redirect to loginURL: an Inertia XHR gets 409 + X-Inertia-Location (the
//     client does a hard redirect), a plain load gets a 302.
//   - GraphQL → the error surfaces in the errors array.
//   - Everything else (API/SDK) → delegated to the optional apiFallback so the
//     app keeps its JSON envelope; without one, the framework default applies.
//
// Forbidden (403) is never redirected — a missing permission is not a login
// problem — so it goes to apiFallback / the default for non-GraphQL callers.
func ErrorHandler(loginURL string, apiFallback ...auth.ErrorHandler) auth.ErrorHandler {
	h := errorHandler{loginURL: loginURL}
	if len(apiFallback) > 0 {
		h.api = apiFallback[0]
	}
	return h
}

func (h errorHandler) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	if rc.Transport == middleware.TransportGraphQL {
		return err
	}
	if rc.Header(headerInertia) != "" {
		// Inertia XHR visit — a normal redirect would be followed
		// transparently by fetch, so use the 409 + location protocol.
		rc.SetHeader(headerLocation, h.loginURL)
		return rc.Reject(http.StatusConflict, err)
	}
	if isFullPageLoad(rc) {
		rc.SetHeader("Location", h.loginURL)
		return rc.Reject(http.StatusFound, err)
	}
	if h.api != nil {
		return h.api.Unauthenticated(rc, err)
	}
	return rc.Reject(http.StatusUnauthorized, err)
}

func (h errorHandler) Forbidden(rc *middleware.RequestCtx, err error) error {
	if rc.Transport == middleware.TransportGraphQL {
		return err
	}
	if h.api != nil {
		return h.api.Forbidden(rc, err)
	}
	return rc.Reject(http.StatusForbidden, err)
}

// isFullPageLoad reports whether the request is a browser navigation expecting
// an HTML document (so a 302 to login lands the visitor on the login screen),
// as opposed to a programmatic JSON client.
func isFullPageLoad(rc *middleware.RequestCtx) bool {
	return strings.Contains(rc.Header("Accept"), "text/html")
}
