package auth

import (
	"net/http"
	"time"

	"github.com/paulmanoni/nexus/httpx"
)

// DefaultSessionCookieName is used when SessionCookie.Name is empty.
const DefaultSessionCookieName = "session"

// SessionCookie is the auth session cookie an app issues at login and clears at
// logout — the token store for server-rendered navigations (e.g. Inertia
// pages) that carry no Authorization header. It owns the cookie's name and
// attributes in one place and hands back a matching Extractor so the read side
// (the scheme) and the write side (login/logout) can't drift:
//
//	var session = auth.SessionCookie{Name: "access_token", MaxAge: 7 * 24 * time.Hour}
//
//	auth.Module(auth.Config{Authentication: auth.Authentication{
//	    Schemes: []auth.Scheme{{
//	        Extract: auth.Chain(auth.Bearer(), session.Extractor()),
//	        Resolve: resolve,
//	    }},
//	}})
//
//	func login(c *httpx.Ctx, …) { session.Set(c, token) }   // on success
//	func logout(c *httpx.Ctx)   { session.Clear(c) }         // on sign-out
//
// The cookie is always HttpOnly (never readable by JS). Set Secure once you are
// behind TLS.
type SessionCookie struct {
	// Name is the cookie name. Defaults to DefaultSessionCookieName.
	Name string
	// Path defaults to "/".
	Path string
	// Domain scopes the cookie; empty means the request host.
	Domain string
	// MaxAge is the cookie lifetime. Defaults to 7 days when zero.
	MaxAge time.Duration
	// Secure marks the cookie HTTPS-only. Leave false for plain-http dev;
	// set true in production.
	Secure bool
	// SameSite defaults to http.SameSiteLaxMode (sends the cookie on
	// top-level navigations, which is what page visits need).
	SameSite http.SameSite
}

func (s SessionCookie) name() string {
	if s.Name != "" {
		return s.Name
	}
	return DefaultSessionCookieName
}

func (s SessionCookie) path() string {
	if s.Path != "" {
		return s.Path
	}
	return "/"
}

func (s SessionCookie) sameSite() http.SameSite {
	if s.SameSite != 0 {
		return s.SameSite
	}
	return http.SameSiteLaxMode
}

// Set writes the session cookie carrying token (HttpOnly).
func (s SessionCookie) Set(c *httpx.Ctx, token string) {
	maxAge := int(s.MaxAge.Seconds())
	if maxAge <= 0 {
		maxAge = int((7 * 24 * time.Hour).Seconds())
	}
	c.SetSameSite(s.sameSite())
	c.SetCookie(s.name(), token, maxAge, s.path(), s.Domain, s.Secure, true)
}

// Clear expires the session cookie (used on logout).
func (s SessionCookie) Clear(c *httpx.Ctx) {
	c.SetSameSite(s.sameSite())
	c.SetCookie(s.name(), "", -1, s.path(), s.Domain, s.Secure, true)
}

// Extractor returns a Cookie extractor bound to this cookie's name, so the
// scheme reads exactly what Set wrote.
func (s SessionCookie) Extractor() Extractor {
	return Cookie(s.name())
}
