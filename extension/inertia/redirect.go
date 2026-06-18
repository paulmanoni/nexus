package inertia

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// redirect is the sentinel a page handler returns (as its error) to issue an
// Inertia redirect instead of rendering a page. The framework's ErrorRenderer
// hook hands it to pageRenderer.RenderError, which writes the right status.
type redirect struct {
	url      string
	external bool
}

func (r *redirect) Error() string { return "inertia: redirect to " + r.url }

// write emits the redirect. Internal redirects use 303 See Other so the
// browser (and the Inertia client) re-issues the follow-up as a GET — correct
// after a POST/PUT/PATCH/DELETE and harmless after a GET. External redirects
// to an Inertia XHR visit can't be a normal HTTP redirect (XHR would follow it
// transparently and try to parse a non-Inertia response), so they use the
// 409 + X-Inertia-Location protocol that tells the client to do a hard
// window.location change; a non-XHR request just gets a normal 302.
func (r *redirect) write(c *httpx.Ctx) error {
	if r.external {
		if c.GetHeader(headerInertia) != "" {
			c.Header(headerLocation, r.url)
			c.AbortWithStatus(http.StatusConflict)
			return nil
		}
		c.Redirect(http.StatusFound, r.url)
		return nil
	}
	c.Redirect(http.StatusSeeOther, r.url)
	return nil
}

// Redirect returns from a page handler to send the visitor to another route
// within the app (303 See Other). Return it as the handler's error:
//
//	func NewCreateUser(svc *UserService, p nexus.Params[NewUser]) (any, error) {
//	    if err := svc.Create(p.Args); err != nil { return nil, err }
//	    return nil, inertia.Redirect("/users")
//	}
func Redirect(url string) error { return &redirect{url: url} }

// Location sends the visitor to an external URL (or forces a full reload of an
// in-app URL). For an Inertia XHR visit this is a 409 + X-Inertia-Location;
// for a plain request it's a 302. Return it as the handler's error.
func Location(url string) error { return &redirect{url: url, external: true} }
