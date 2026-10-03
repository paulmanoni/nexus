package inertia

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/maskhook"
	"github.com/paulmanoni/nexus/v2/registry"
)

// Resource registers a controller's conventional page actions as Inertia
// pages and its write actions as redirecting form endpoints — whichever of
// these methods controller type T defines:
//
//	Index    GET    /users            page  Users/Index
//	New      GET    /users/new        page  Users/New
//	Show     GET    /users/:id        page  Users/Show
//	Edit     GET    /users/:id/edit   page  Users/Edit
//	Create   POST   /users            → 303 to the new record's Show (Index without one)
//	Update   PUT    /users/:id        → 303 to Show (Index without one); PATCH too
//	Destroy  DELETE /users/:id        → 303 to Index (back without one)
//
// The component folder comes from the type name (UsersController → Users);
// ResourceAs names it explicitly. Page actions return their props; Create
// returns the created record, whose ID field (or json "id") picks the Show to
// redirect to. A write action that returns nexus.Invalid() sends the user back to
// the form with the field errors, as any Inertia form does.
//
// It is a nexus.Controller underneath: path parameters bind to bare scalar
// parameters (Show(ctx, id int64)), shared gates and Authorize apply, and
// Member/Collection/Get/... add custom actions, which follow the same rules
// with the method name as the action: a GET renders the page <folder>/<Method>
// (pageUrl('Users/Stats')), any other verb redirects back — to the Referer,
// else one segment up — after success (pageAction('Users/Suspend', {id})).
// Pass nexus.NoActionDefaults() for a plain JSON action instead.
//
//	inertia.Resource[*UsersController]("/users", auth.Required()).
//	    Provide(NewUsersController).
//	    Member("POST", "suspend", (*UsersController).Suspend).   // → back
//	    Collection("GET", "stats", (*UsersController).Stats)      // page Users/Stats
func Resource[T any](prefix string, shared ...nexus.MiddlewareOption) *nexus.ControllerRouter[T] {
	return ResourceAs[T](componentFolder(reflect.TypeFor[T]()), prefix, shared...)
}

// ResourceAs is Resource with the component folder named explicitly:
// ResourceAs[*UsersController]("Admin/Users", "/admin/users") renders
// Admin/Users/Index, Admin/Users/Show, ….
func ResourceAs[T any](folder, prefix string, shared ...nexus.MiddlewareOption) *nexus.ControllerRouter[T] {
	c := nexus.Controller[T](prefix, shared...)
	t := reflect.TypeFor[T]()
	has := func(name string) bool { _, ok := t.MethodByName(name); return ok }
	found := false
	register := func(action string, verbs []string, path string, opts ...nexus.RestOption) {
		m, ok := t.MethodByName(action)
		if !ok {
			return
		}
		found = true
		for _, verb := range verbs {
			c.Rest(verb, path, m.Func.Interface(), opts...)
		}
	}
	folder = strings.TrimSuffix(folder, "/")
	c.ActionDefaults(func(method, _, action string) []nexus.RestOption {
		component := folder + "/" + action
		if method == http.MethodGet {
			return []nexus.RestOption{
				nexus.WithRenderer(pageRenderer{component: component}),
				nexus.WithIcon(Icon),
				nexus.Tag(registry.PageTag, component),
			}
		}
		return []nexus.RestOption{
			nexus.WithRenderer(resourceRedirect{to: toBack}),
			nexus.WithIcon(Icon),
			nexus.Tag(registry.PageActionTag, component),
		}
	})
	redirect := func(to redirectTo) nexus.RestOption { return nexus.WithRenderer(resourceRedirect{to: to}) }

	register("Index", []string{"GET"}, "")
	register("New", []string{"GET"}, "/new")
	register("Show", []string{"GET"}, "/:id")
	register("Edit", []string{"GET"}, "/:id/edit")

	createTo, updateTo, destroyTo := toSelf, toSelf, toBack
	if has("Show") {
		createTo = toCreated
	}
	if !has("Show") {
		updateTo = toParent
	}
	if has("Index") {
		destroyTo = toParent
	}
	register("Create", []string{"POST"}, "", redirect(createTo))
	register("Update", []string{"PUT", "PATCH"}, "/:id", redirect(updateTo))
	register("Destroy", []string{"DELETE"}, "/:id", redirect(destroyTo))

	if !found {
		c.RequireActions(fmt.Sprintf(
			"inertia: Resource[%s] has no actions — it defines none of Index, New, Show, Edit, Create, Update, Destroy, and no other action was added", t))
	}
	return c
}

// redirectTo is where a write action sends the browser on success, relative
// to the request's own path (so any route prefix is already in it).
type redirectTo uint8

const (
	toSelf    redirectTo = iota // the request path: /users/5 after an update
	toParent                    // one segment up: /users after DELETE /users/5
	toCreated                   // <path>/<id of the result>: /users/7 after POST /users
	toBack                      // the Referer, else one segment up
)

// resourceRedirect answers a successful write action with a 303 — the status
// Inertia requires after PUT/PATCH/DELETE — and hands errors to the page
// renderer (validation flash + back, Redirect/Location, the ErrorPage).
type resourceRedirect struct{ to redirectTo }

func (r resourceRedirect) Render(c *httpx.Ctx, result any) error {
	c.Redirect(http.StatusSeeOther, r.target(c, result))
	return nil
}

func (r resourceRedirect) RenderEmpty(c *httpx.Ctx) error {
	c.Redirect(http.StatusSeeOther, r.target(c, nil))
	return nil
}

func (r resourceRedirect) RenderError(c *httpx.Ctx, err error) (bool, error) {
	return pageRenderer{}.RenderError(c, err)
}

func (r resourceRedirect) target(c *httpx.Ctx, result any) string {
	path := c.Request.URL.Path
	switch r.to {
	case toParent:
		if i := strings.LastIndex(strings.TrimSuffix(path, "/"), "/"); i > 0 {
			return path[:i]
		}
		return "/"
	case toCreated:
		if id, ok := resultID(result); ok {
			return strings.TrimSuffix(path, "/") + "/" + id
		}
		return path
	case toBack:
		if ref := c.GetHeader("Referer"); ref != "" {
			return ref
		}
		return resourceRedirect{to: toParent}.target(c, nil)
	}
	return path
}

// resultID reads the ID of a created record: a field named ID or Id, or one
// tagged json:"id". Integer ids are masked when extension/maskid is on, so
// the redirect carries the same form the rest of the app shows.
func resultID(result any) (string, bool) {
	v := reflect.ValueOf(result)
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return "", false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		jsonName := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if !f.IsExported() || (f.Name != "ID" && f.Name != "Id" && jsonName != "id") {
			continue
		}
		fv := v.Field(i)
		for fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				return "", false
			}
			fv = fv.Elem()
		}
		switch fv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if fv.Int() == 0 {
				return "", false
			}
			return maskedID(fv.Int()), true
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if fv.Uint() == 0 {
				return "", false
			}
			return maskedID(int64(fv.Uint())), true
		case reflect.String:
			return fv.String(), fv.String() != ""
		}
	}
	return "", false
}

func maskedID(n int64) string {
	if s, ok := maskhook.MaskID("id", n); ok {
		return s
	}
	return strconv.FormatInt(n, 10)
}

// componentFolder names a controller's page folder after its type:
// *UsersController → "Users".
func componentFolder(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := strings.TrimSuffix(t.Name(), "Controller"); name != "" {
		return name
	}
	return t.Name()
}
