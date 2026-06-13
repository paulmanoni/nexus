// Package iauth bridges nexus's auth extension to Inertia: an auth.ErrorHandler
// that turns auth denials into Inertia-correct redirects instead of JSON 401s.
// It lives in its own package so apps using inertia without auth don't pull the
// auth extension (and its dependencies) into the build.
//
//	import "github.com/paulmanoni/nexus/extension/inertia/iauth"
//
//	auth.Module(auth.Config{
//	    Authentication: auth.Authentication{Schemes: ...},
//	    Authorization:  auth.Authorization{Default: auth.Authenticated()}, // deny-by-default
//	    OnError:        iauth.ErrorHandler("/login"),
//	})
package iauth

import (
	"net/http"

	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/middleware"
)

const (
	headerInertia  = "X-Inertia"
	headerLocation = "X-Inertia-Location"
)

// errorHandler implements auth.ErrorHandler with Inertia-aware redirects.
type errorHandler struct{ loginURL string }

// ErrorHandler returns an auth.ErrorHandler that sends unauthenticated visitors
// to loginURL: an Inertia XHR visit gets a 409 + X-Inertia-Location (the
// client does a hard redirect), a plain browser request gets a 302, and a
// GraphQL request gets the error in its errors array. Forbidden (403) denials
// render normally — a missing permission is not a login problem.
func ErrorHandler(loginURL string) auth.ErrorHandler {
	return errorHandler{loginURL: loginURL}
}

func (h errorHandler) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	if rc.Transport == middleware.TransportGraphQL {
		return err
	}
	if rc.Header(headerInertia) != "" {
		rc.SetHeader(headerLocation, h.loginURL)
		return rc.Reject(http.StatusConflict, err)
	}
	rc.SetHeader("Location", h.loginURL)
	return rc.Reject(http.StatusFound, err)
}

func (h errorHandler) Forbidden(rc *middleware.RequestCtx, err error) error {
	if rc.Transport == middleware.TransportGraphQL {
		return err
	}
	return rc.Reject(http.StatusForbidden, err)
}
