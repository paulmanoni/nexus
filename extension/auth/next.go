package auth

import (
	"context"
	"net/url"
	"strings"
)

// safeNext returns raw when it is a same-site, root-relative path — the
// only kind of next a sign-in redirects to — and "" otherwise. It is checked
// as given and after every round of URL decoding, so an encoded "//evil" or
// "/\evil" is refused like a plain one: no scheme, no authority, no
// backslash, no control character, no protocol-relative "//".
func safeNext(raw string) string {
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	v := raw
	for range 4 {
		if !nextShapeOK(v) {
			return ""
		}
		d, err := url.PathUnescape(v)
		if err != nil {
			return ""
		}
		if d == v {
			return raw
		}
		v = d
	}
	return "" // still decoding after four rounds: refuse
}

func nextShapeOK(v string) bool {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return false
	}
	for _, r := range v {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil
}

// Next returns the request's validated next — the page a sign-in should
// return to, from the query ([auth] next_param) — or "".
func Next(ctx context.Context) string {
	r, _ := ctx.Value(ctxRequestInfo).(requestInfo)
	return r.next
}

// requestInfo is what the auth middleware records of the HTTP request, for
// gates and sign-in code that only see a context.
type requestInfo struct {
	method, path, uri, accept, inertia, next string
}

const ctxRequestInfo ctxCredential = 100

// landing is where a sign-in goes: a valid next — dropped when it lies in an
// area the identity's kind doesn't belong to — else the home of the area
// the sign-in happened in, else [auth] home.
func (rs *resolvedSettings) landing(ctx context.Context, id *Identity, next string) string {
	if n := safeNext(next); n != "" {
		if a := rs.area(pathOf(n)); a == nil || kindIn(id.Kind, a.Kinds) {
			return n
		}
	}
	r, _ := ctx.Value(ctxRequestInfo).(requestInfo)
	if a := rs.area(r.path); a != nil {
		return a.Home
	}
	return rs.home
}

func pathOf(n string) string {
	if u, err := url.Parse(n); err == nil {
		return u.Path
	}
	return n
}

// kindIn reports whether kind may enter an area with kinds (empty = any).
func kindIn(kind string, kinds []string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
