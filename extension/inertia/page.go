package inertia

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"braces.dev/errtrace"

	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/registry"

	"github.com/paulmanoni/nexus/v2"
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
	if err := validatePage(method, path, component, fn); err != nil {
		return nexus.Error(err)
	}
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

// Component renders an action as the Inertia page component — what Page does
// for a handler, as an option, so a controller action (or any REST handler)
// becomes a page with no resource conventions and any component name:
//
//	nexus.Controller[*ListsController]("/lists").
//	    Get("/:pk/view", (*ListsController).Longlist, inertia.Component("Admin/AdvertLonglist"))
//
// On an inertia.Resource it overrides the conventional <Folder>/<Method>
// component. It is what the //@page annotation generates.
func Component(name string) nexus.RestOption {
	if strings.TrimSpace(name) == "" {
		panic("inertia.Component: component name is empty — name the client component, e.g. \"Users/Index\"")
	}
	return nexus.RestOptions(
		nexus.WithRenderer(pageRenderer{component: name}),
		nexus.WithIcon(Icon),
		nexus.Tag(registry.PageTag, name),
	)
}

// AsPage renders a controller action as the Inertia page named after it —
// <Folder>/<Method>, the folder from the controller type (UsersController →
// Users) — the Go form of a //@page annotation without a component:
//
//	nexus.Controller[*UsersController]("/users").
//	    Get("/:id", (*UsersController).Show, inertia.AsPage())   // Users/Show
//
// Only valid on a controller action; name the page with Component elsewhere.
func AsPage() nexus.RestOption {
	return nexus.ActionOption(func(ctrl reflect.Type, action string) nexus.RestOption {
		return Component(componentFolder(ctrl) + "/" + action)
	})
}

// validatePage rejects a malformed registration at option-build time, so a
// bad Page call (direct or via the //@inertia.Page decorator) fails the boot
// with a message naming the page, instead of surfacing later as a route that
// never matches or a client-side "component not found".
func validatePage(method, path, component string, fn any) error {
	for _, m := range splitMethods(method) {
		switch m {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			return fmt.Errorf("inertia.Page(%q, %q, %q): %q is not an HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)",
				method, path, component, m)
		}
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("inertia.Page(%q, %q, %q): path must start with \"/\"", method, path, component)
	}
	if strings.TrimSpace(component) == "" {
		return fmt.Errorf("inertia.Page(%q, %q, …): component name is empty — name the client component, e.g. \"Users/Index\"", method, path)
	}
	if fn == nil {
		return fmt.Errorf("inertia.Page(%q, %q, %q): handler is nil", method, path, component)
	}
	return nil
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
	err := eng.render(c, p.component, result)
	// A prop that fails to resolve (a Defer/Optional thunk returning an
	// error) surfaces here rather than from the handler; with an error page
	// configured it answers the same way a handler error does.
	if err != nil && eng.errorPage != "" && !c.Writer.Written() {
		return eng.renderError(c, err)
	}
	return err
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
	// Anything else fails the request. With Config.ErrorPage set it still
	// answers in the Inertia protocol.
	eng, ok := engineFromGin(c)
	if !ok || eng.errorPage == "" {
		return false, nil
	}
	return true, eng.renderError(c, err)
}

// renderError answers a failed page request in the Inertia protocol: the
// app's error page for a visit, a flashed global error for a form submit.
// The error is recorded on the request's trace either way.
func (e *Engine) renderError(c *httpx.Ctx, err error) error {
	_ = c.Error(errtrace.Wrap(err))
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		return writeValidationRedirect(c, &validationError{fields: map[string]string{nexus.GlobalErrorKey: err.Error()}})
	}
	status := http.StatusInternalServerError
	if mapped, ok := nexus.MapCRUDError(err); ok {
		status = mapped
	}
	return e.renderStatus(c, e.errorPage, ErrorProps{Status: status, Message: err.Error()}, status)
}
