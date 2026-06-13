package inertia

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
)

// Page registers an Inertia page route. The handler is an ordinary nexus
// reflective handler — deps are fx-injected, the optional trailing
// nexus.Params[T] binds the request — but its return value is the page's
// props rather than a JSON body:
//
//	inertia.Page("GET", "/users", "Users/Index", NewListUsers)
//
//	func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error) {
//	    return UsersProps{Users: svc.Page(p.Args)}, nil
//	}
//
// component is the client-side component name the Inertia adapter resolves
// (e.g. "Users/Index"). The route behaves like any AsRest endpoint — auth
// gates (auth.Required, deny-by-default, nexus.Public) and other RestOptions
// compose normally — except its successful return is rendered through the
// Inertia protocol (JSON page object for XHR visits, HTML shell for full
// loads) by the engine installed via Module.
func Page(method, path, component string, fn any, opts ...nexus.RestOption) nexus.Option {
	full := make([]nexus.RestOption, 0, len(opts)+1)
	full = append(full, nexus.WithRenderer(pageRenderer{component: component}))
	full = append(full, opts...)
	return nexus.AsRest(method, path, fn, full...)
}

// pageRenderer is the nexus.ResponseRenderer bound to a single page route. It
// carries only the component name; the per-app engine is resolved from the gin
// context (installed by Module's middleware), so the same renderer type works
// across multiple apps in one process.
type pageRenderer struct{ component string }

func (p pageRenderer) Render(c *gin.Context, result any) error {
	eng, ok := engineFromGin(c)
	if !ok {
		return errors.New("inertia: engine not installed — add inertia.Module(...) to your app")
	}
	return eng.render(c, p.component, result)
}

// RenderError implements nexus.ErrorRenderer: it claims inertia.Redirect /
// inertia.Location sentinels and writes them as 303/409 redirects. Any other
// error is left to the framework's standard error path (handled=false).
func (p pageRenderer) RenderError(c *gin.Context, err error) (bool, error) {
	var rd *redirect
	if errors.As(err, &rd) {
		return true, rd.write(c)
	}
	return false, nil
}
