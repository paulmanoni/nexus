package inertia

import (
	"errors"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/registry"

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
//
// method may name several HTTP verbs (comma- or space-separated, e.g.
// "GET,POST") to mount the SAME handler for each — a Django-style view that
// renders on GET and mutates on POST. The handler branches on the verb via
// nexus.Params[T].Method:
//
//	inertia.Page("GET,POST", "/login", "Login", NewLogin, nexus.Public())
//
//	func NewLogin(c *httpx.Ctx, p nexus.Params[LoginArgs]) (any, error) {
//	    if p.Method == http.MethodGet { return LoginProps{}, nil } // render
//	    // POST: authenticate, set cookie…
//	    return nil, inertia.Redirect("/dashboard")
//	}
//
// The route is tagged registry.PageTag = component, which the client SDK
// projects into a typed NexusPageProps entry (the handler's return type) and
// the Vite plugin checks against the pages directory. A page is rendered,
// not called, so it is not emitted as a REST call in the SDK.
//
// Icon is the lucide-style icon inertia brands its pages and dashboard entry
// with. Pages registered via Page (explicitly or through the //@inertia.Page
// decorator) carry it so the dashboard shows them as inertia pages.
const Icon = "app-window"

func Page(method, path, component string, fn any, opts ...nexus.RestOption) nexus.Option {
	full := make([]nexus.RestOption, 0, len(opts)+3)
	full = append(full, nexus.WithRenderer(pageRenderer{component: component}))
	full = append(full, nexus.WithIcon(Icon))
	full = append(full, nexus.Tag(registry.PageTag, component))
	full = append(full, opts...)

	methods := splitMethods(method)
	if len(methods) == 1 {
		return nexus.AsRest(methods[0], path, fn, full...)
	}
	out := make([]nexus.Option, 0, len(methods))
	for _, m := range methods {
		out = append(out, nexus.AsRest(m, path, fn, full...))
	}
	return nexus.Options(out...)
}

// splitMethods parses a method spec like "GET", "GET,POST", or "GET POST"
// into uppercased verbs. A spec with no separators returns the single verb.
func splitMethods(spec string) []string {
	fields := strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return []string{strings.ToUpper(strings.TrimSpace(spec))}
	}
	for i := range fields {
		fields[i] = strings.ToUpper(fields[i])
	}
	return fields
}

// pageRenderer is the nexus.ResponseRenderer bound to a single page route. It
// carries only the component name; the per-app engine is resolved from the gin
// context (installed by Module's middleware), so the same renderer type works
// across multiple apps in one process.
type pageRenderer struct{ component string }

func (p pageRenderer) Render(c *httpx.Ctx, result any) error {
	eng, ok := engineFromGin(c)
	if !ok {
		return errors.New("inertia: engine not installed — add inertia.Module(...) to your app")
	}
	return eng.render(c, p.component, result)
}

// RenderError implements nexus.ErrorRenderer: it claims inertia.Redirect /
// inertia.Location sentinels and writes them as 303/409 redirects. Any other
// error is left to the framework's standard error path (handled=false).
func (p pageRenderer) RenderError(c *httpx.Ctx, err error) (bool, error) {
	var rd *redirect
	if errors.As(err, &rd) {
		return true, rd.write(c)
	}
	// inertia.Invalid → flash the field errors and redirect back so the form
	// re-renders with page.props.errors populated (the useForm convention).
	var ve *validationError
	if errors.As(err, &ve) {
		return true, writeValidationRedirect(c, ve)
	}
	// nexus.Errors (the core accumulator, field + global) rides the same
	// flash + 303 flow: first message per field, the global messages under
	// errors._global — one object, the one useForm already watches.
	var ne *nexus.Errors
	if errors.As(err, &ne) {
		return true, writeValidationRedirect(c, &validationError{fields: ne.First()})
	}
	return false, nil
}
