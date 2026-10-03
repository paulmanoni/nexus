package nexus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"

	"github.com/paulmanoni/nexus/v2/di"
)

// ControllerRouter is a Router bound to one controller type: a struct whose
// methods are the actions. The controller is constructed once through DI (its
// dependencies are its constructor's parameters), every action shares the
// router's prefix and gates, and the whole controller shows as one module on
// the dashboard.
//
//	type UsersController struct{ users *UserService }
//
//	func NewUsersController(users *UserService) *UsersController { … }
//
//	func (c *UsersController) Show(ctx context.Context, id int64) (*User, error)
//	func (c *UsersController) Suspend(ctx context.Context, id int64, in SuspendInput) error
//
//	nexus.Controller[*UsersController]("/users", auth.Required()).
//	    Provide(NewUsersController).
//	    Get("/:id", (*UsersController).Show).
//	    Post("/:id/suspend", (*UsersController).Suspend, auth.Requires("users:suspend"))
//
// Actions are method expressions on the controller type. Bare scalar
// parameters bind from the route's path parameters by position — the route
// above gives Show and Suspend their id with no nexus.Arg — and a trailing
// struct is the request body (see nexus.Arg). An explicit nexus.Arg on the
// action overrides the inference.
//
// A controller that implements ActionAuthorizer has Authorize called before
// every action, with the request's context and the action's method name.
//
// A *ControllerRouter is an Option (pass it to Boot/Run, or include its
// Router in a parent with Include).
type ControllerRouter[T any] struct {
	*Router
	ctrl     reflect.Type
	authz    bool
	defaults []func(method, path, action string) []RestOption
	slash    bool
}

// ActionAuthorizer is implemented by a controller that authorizes each action
// itself — the typed, explicit form of a before-action hook:
//
//	func (c *UsersController) Authorize(ctx context.Context, action string) error {
//	    if action == "Destroy" && !auth.Can(ctx, "users:delete") {
//	        return errors.New("you cannot delete users")
//	    }
//	    return nil
//	}
//
// It runs after the router's gates (so the identity is resolved) and before
// the action. A non-nil error ends the request through the action's normal
// error path as nexus.Forbidden with the error's message — a REST 403, a
// GraphQL FORBIDDEN, the Inertia ErrorPage — unless it already carries a code
// (nexus.Err(nexus.NotFound, …) stays a 404; nexus.Invalid() stays a
// validation response).
type ActionAuthorizer interface {
	Authorize(ctx context.Context, action string) error
}

// Controller creates a ControllerRouter for controller type T (typically a
// pointer to a struct) mounted at prefix. The dashboard module is named after
// the type: UsersController → "users". The prefix is a REST prefix: GraphQL
// actions serve on the endpoint of the enclosing nexus.Module (or the app's),
// never on <prefix>/graphql. Inside Module("x", Path("/x"), …) the prefix
// stacks under /x.
func Controller[T any](prefix string, shared ...MiddlewareOption) *ControllerRouter[T] {
	return newController[T](controllerName(reflect.TypeFor[T]()), prefix, shared...)
}

func newController[T any](name, prefix string, shared ...MiddlewareOption) *ControllerRouter[T] {
	t := reflect.TypeFor[T]()
	r := NewRouter(name, prefix, shared...)
	r.restOnly = true
	c := &ControllerRouter[T]{
		Router: r,
		ctrl:   t,
		authz:  t.Implements(reflect.TypeFor[ActionAuthorizer]()),
	}
	r.claims = append(r.claims, t)
	r.hooks = append(r.hooks, func() {
		for _, fn := range annotatedActions(t) {
			fn.(func(*ControllerRouter[T]))(c)
		}
	})
	return c
}

// ControllerActions records actions for controller type T — what the
// //nexus:page, //nexus:rest, //nexus:query and //nexus:mutation annotations on T's methods
// generate when T carries no //nexus:controller:
//
//	nexus.ControllerActions(func(c *nexus.ControllerRouter[*AdminController]) {
//	    c.Rest("GET", "/", (*AdminController).Dashboard, inertia.Component("Admin/Dashboard"))
//	    c.Query((*AdminController).AdminStats)
//	})
//
// A nexus.Controller[T] or nexus.Resource[T] declared in Go takes them: they
// mount under its prefix, its module's Path, its gates and Authorize — so the
// Go code says where the controller lives and the annotations say what it
// does. With no such controller in the app, they register on their own,
// under the module the returned Option sits in.
func ControllerActions[T any](fn func(*ControllerRouter[T])) Option {
	t := reflect.TypeFor[T]()
	controllerActionsReg.Lock()
	controllerActionsReg.m[t] = append(controllerActionsReg.m[t], fn)
	controllerActionsReg.Unlock()
	return &actionsFallback[T]{}
}

var controllerActionsReg = struct {
	sync.Mutex
	m map[reflect.Type][]any
}{m: map[reflect.Type][]any{}}

func annotatedActions(t reflect.Type) []any {
	controllerActionsReg.Lock()
	defer controllerActionsReg.Unlock()
	return append([]any(nil), controllerActionsReg.m[t]...)
}

// actionsFallback registers T's recorded actions on a controller of their
// own when no controller for T in the app claimed them.
type actionsFallback[T any] struct {
	enclosing, publicPath, module string
	ctrl                          *ControllerRouter[T]
}

func (f *actionsFallback[T]) setEnclosing(prefix, publicPath string) {
	f.enclosing, f.publicPath = prefix+f.enclosing, publicPath
}

func (f *actionsFallback[T]) setModule(name string) { f.module = name }

func (f *actionsFallback[T]) nexusOption() di.Option {
	return di.Defer(func() di.Option {
		t := reflect.TypeFor[T]()
		if claimedIn(t, currentBuild()) {
			return nil
		}
		if f.ctrl == nil {
			name := f.module // group under the enclosing module, as plain registrations would
			if name == "" {
				name = controllerName(t)
			}
			f.ctrl = newController[T](name, "")
			f.ctrl.enclosing, f.ctrl.gqlHome = f.enclosing, f.publicPath
		}
		return f.ctrl.Router.resolve()
	})
}

// RequireActions fails the boot with msg when the controller ends up with no
// actions at all — conventional, custom or annotated.
func (c *ControllerRouter[T]) RequireActions(msg string) *ControllerRouter[T] {
	c.requireActions = msg
	return c
}

// Resource creates a ControllerRouter for T and registers its conventional
// actions — whichever of these methods the controller defines:
//
//	Index    GET    <prefix>
//	Show     GET    <prefix>/:id
//	Create   POST   <prefix>
//	Update   PUT    <prefix>/:id   (and PATCH)
//	Destroy  DELETE <prefix>/:id
//
// Path parameters bind as for Controller: Show(ctx, id int64) gets the :id,
// and a nested resource at "/posts/:postId/comments" gives its actions the
// postId too (Show(ctx, postID, id int64)). Add further actions with Member,
// Collection or the verb methods.
func Resource[T any](prefix string, shared ...MiddlewareOption) *ControllerRouter[T] {
	c := Controller[T](prefix, shared...)
	found := false
	for _, a := range resourceActions {
		m, ok := c.ctrl.MethodByName(a.method)
		if !ok {
			continue
		}
		found = true
		for _, verb := range a.verbs {
			c.rest(verb, a.path, m.Func.Interface(), a.method, nil)
		}
	}
	if !found {
		c.RequireActions(fmt.Sprintf(
			"nexus: Resource[%s] has no actions — it defines none of Index, Show, Create, Update, Destroy, and no other action (annotated, Member, Collection or a verb method) was added", c.ctrl))
	}
	return c
}

var resourceActions = []struct {
	method string
	verbs  []string
	path   string
}{
	{"Index", []string{"GET"}, ""},
	{"Create", []string{"POST"}, ""},
	{"Show", []string{"GET"}, "/:id"},
	{"Update", []string{"PUT", "PATCH"}, "/:id"},
	{"Destroy", []string{"DELETE"}, "/:id"},
}

// Get registers action at GET path (relative to the controller's prefix).
func (c *ControllerRouter[T]) Get(path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest("GET", path, action, opts...)
}

// Post registers action at POST path.
func (c *ControllerRouter[T]) Post(path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest("POST", path, action, opts...)
}

// Put registers action at PUT path.
func (c *ControllerRouter[T]) Put(path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest("PUT", path, action, opts...)
}

// Patch registers action at PATCH path.
func (c *ControllerRouter[T]) Patch(path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest("PATCH", path, action, opts...)
}

// Delete registers action at DELETE path.
func (c *ControllerRouter[T]) Delete(path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest("DELETE", path, action, opts...)
}

// Rest registers action at method + path.
func (c *ControllerRouter[T]) Rest(method, path string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.rest(method, path, action, "", opts)
}

// Member registers a resource member action at <prefix>/:id/<name>:
// Member("POST", "publish", (*PostsController).Publish) → POST /posts/:id/publish.
func (c *ControllerRouter[T]) Member(method, name string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest(method, "/:id/"+strings.TrimPrefix(name, "/"), action, opts...)
}

// Collection registers a resource collection action at <prefix>/<name>:
// Collection("GET", "search", (*PostsController).Search) → GET /posts/search.
func (c *ControllerRouter[T]) Collection(method, name string, action any, opts ...RestOption) *ControllerRouter[T] {
	return c.Rest(method, "/"+strings.TrimPrefix(name, "/"), action, opts...)
}

// ActionDefaults sets the options every REST action on c starts from, chosen
// per action — the hook a flavour of controller uses to give custom actions
// its own rendering. extension/inertia's Resource sets it so a custom GET is
// a page and a custom write redirects, exactly like the conventional actions:
//
//	c.ActionDefaults(func(method, path, action string) []nexus.RestOption {
//	    return []nexus.RestOption{nexus.WithIcon("zap")}
//	})
//
// method and path are the action's (path relative to the prefix); action is
// its method name. The defaults apply before the action's own options, so an
// explicit option wins; nexus.NoActionDefaults() on an action skips them. It
// applies to actions registered before and after the call. Calls add up: each
// function's options apply after the previous one's, so defaults set on an
// inertia.Resource extend its page rendering rather than replace it.
func (c *ControllerRouter[T]) ActionDefaults(fn func(method, path, action string) []RestOption) *ControllerRouter[T] {
	c.defaults = append(c.defaults, fn)
	return c
}

// TrailingSlash registers every REST action at its path and at the path with
// a trailing slash — /users/:id and /users/:id/ — for apps whose existing
// links use both (a Django port, say). An action at the prefix itself serves
// /users and /users/. It applies to actions registered before and after the
// call.
func (c *ControllerRouter[T]) TrailingSlash() *ControllerRouter[T] {
	c.slash = true
	return c
}

// ActionOption is a REST option chosen by the controller action it is given
// to: fn receives the controller type and the action's method name. It is how
// an option names something after its action — inertia.AsPage() renders
// (*UsersController).Show as the page Users/Show:
//
//	nexus.Controller[*UsersController]("/users").
//	    Get("/:id", (*UsersController).Show, inertia.AsPage())
//
// Outside a controller action (a plain AsRest) it fails the boot.
func ActionOption(fn func(ctrl reflect.Type, action string) RestOption) RestOption {
	return restOptionFn(func(c *restConfig) {
		if c.action == nil {
			if c.optErr == nil {
				c.optErr = errors.New("an action option (e.g. inertia.AsPage) is only valid on a controller action — name the value explicitly here")
			}
			return
		}
		if o := fn(c.action.ctrl, c.action.name); o != nil {
			o.applyToRest(c)
		}
	})
}

// actionContext tells the options that follow which controller action they
// are applied to.
type actionContext struct {
	ctrl reflect.Type
	name string
}

func (a *actionContext) applyToRest(c *restConfig) { c.action = a }

// NoActionDefaults exempts one action from its controller's ActionDefaults —
// a plain JSON endpoint on an Inertia resource, say.
func NoActionDefaults() RestOption { return noActionDefaults{} }

type noActionDefaults struct{}

func (noActionDefaults) applyToRest(*restConfig) {}

// Query registers action as a GraphQL query (its op name comes from the method
// name). Scalar parameters need an explicit nexus.Arg — GraphQL has no path.
func (c *ControllerRouter[T]) Query(action any, opts ...GqlOption) *ControllerRouter[T] {
	return c.gqlAction("query", AsQuery, action, opts)
}

// Mutation registers action as a GraphQL mutation.
func (c *ControllerRouter[T]) Mutation(action any, opts ...GqlOption) *ControllerRouter[T] {
	return c.gqlAction("mutation", AsMutation, action, opts)
}

// Provide adds constructors (usually the controller's own) under the
// controller's module.
func (c *ControllerRouter[T]) Provide(fns ...any) *ControllerRouter[T] {
	c.Router.Provide(fns...)
	return c
}

// Supply adds a ready-made controller (or other values) under the
// controller's module — the alternative to a constructor via Provide.
func (c *ControllerRouter[T]) Supply(vals ...any) *ControllerRouter[T] {
	c.Router.Register(Supply(vals...))
	return c
}

// rest registers one REST action. name is the action name Authorize sees;
// empty means "derive it from the method expression".
func (c *ControllerRouter[T]) rest(method, path string, action any, name string, opts []RestOption) *ControllerRouter[T] {
	fn, name, err := c.prepare(action, name)
	if err != nil {
		c.errs = append(c.errs, err)
		return c
	}
	explicitArg, skipDefaults := false, false
	for _, o := range opts {
		switch o.(type) {
		case ArgOption:
			explicitArg = true
		case noActionDefaults:
			skipDefaults = true
		}
	}
	// Each route is its own builder: a module stamps its prefix only on the
	// ops it holds directly, not on ones bundled inside an Options.
	build := func(route string, twin bool) func(string, []MiddlewareOption) Option {
		return func(full string, sh []MiddlewareOption) Option {
			if twin && (!c.slash || strings.HasSuffix(path, "/")) {
				return Options()
			}
			all := make([]RestOption, 0, len(sh)+len(opts)+2)
			all = append(all, &actionContext{ctrl: c.ctrl, name: name})
			for _, m := range sh {
				all = append(all, m)
			}
			if !explicitArg {
				arg, err := inferPathArgs(fn, name, method, full+path)
				if err != nil {
					return FailBoot(err)
				}
				if arg != nil {
					all = append(all, *arg)
				}
			}
			if !skipDefaults {
				for _, d := range c.defaults {
					all = append(all, d(method, path, name)...)
				}
			}
			return AsRest(method, route, fn, append(all, opts...)...)
		}
	}
	c.builders = append(c.builders, build(path, false), build(path+"/", true))
	return c
}

func (c *ControllerRouter[T]) gqlAction(kind string, as func(any, ...GqlOption) Option, action any, opts []GqlOption) *ControllerRouter[T] {
	fn, _, err := c.prepare(action, "")
	if err != nil {
		c.errs = append(c.errs, err)
		return c
	}
	// The op name comes from the method, before an Authorize wrapper hides it;
	// an explicit nexus.Op among opts still wins (it applies later).
	named := append([]GqlOption{Op(opNameFromFunc(action, kind))}, opts...)
	c.Router.gql(as, fn, named)
	return c
}

// prepare validates that action is a method expression on the controller
// type and, when the controller authorizes its actions, wraps it.
func (c *ControllerRouter[T]) prepare(action any, name string) (any, string, error) {
	v := reflect.ValueOf(action)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return nil, "", fmt.Errorf("nexus: Controller[%s]: action must be a method expression like (%s).Index, got %T", c.ctrl, c.ctrl, action)
	}
	if v.Type().NumIn() == 0 || v.Type().In(0) != c.ctrl {
		return nil, "", fmt.Errorf("nexus: Controller[%s]: action %s is not a method of %s — pass a method expression like (%s).Index",
			c.ctrl, v.Type(), c.ctrl, c.ctrl)
	}
	if name == "" {
		name = runtimeFuncName(v)
	}
	if !c.authz {
		return action, name, nil
	}
	wrapped, err := wrapAuthorize(v, name)
	if err != nil {
		return nil, "", fmt.Errorf("nexus: Controller[%s].%s: %w", c.ctrl, name, err)
	}
	return wrapped, name, nil
}

// wrapAuthorize returns a func with fn's parameters (plus a context.Context
// after the receiver when fn has none) that calls the receiver's Authorize
// before fn. The error travels as fn's own error return.
func wrapAuthorize(fn reflect.Value, action string) (any, error) {
	ft := fn.Type()
	errType := reflect.TypeFor[error]()
	if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != errType {
		return nil, fmt.Errorf("the controller implements Authorize, so its actions must return an error (got %s)", ft)
	}
	ctxIdx := -1
	for i := 0; i < ft.NumIn(); i++ {
		if ft.In(i) == contextType {
			ctxIdx = i
			break
		}
	}
	in := make([]reflect.Type, 0, ft.NumIn()+1)
	for i := 0; i < ft.NumIn(); i++ {
		in = append(in, ft.In(i))
	}
	injected := ctxIdx < 0
	if injected {
		in = append([]reflect.Type{in[0], contextType}, in[1:]...)
		ctxIdx = 1
	}
	out := make([]reflect.Type, ft.NumOut())
	for i := range out {
		out[i] = ft.Out(i)
	}
	wrapper := reflect.MakeFunc(reflect.FuncOf(in, out, false), func(args []reflect.Value) []reflect.Value {
		ctx, _ := args[ctxIdx].Interface().(context.Context)
		if ctx == nil {
			ctx = context.Background()
		}
		if err := args[0].Interface().(ActionAuthorizer).Authorize(ctx, action); err != nil {
			res := make([]reflect.Value, len(out))
			for i, t := range out {
				res[i] = reflect.Zero(t)
			}
			e := reflect.New(errType).Elem()
			e.Set(reflect.ValueOf(forbiddenError(err)))
			res[len(res)-1] = e
			return res
		}
		if injected {
			args = append([]reflect.Value{args[0]}, args[2:]...)
		}
		return fn.Call(args)
	})
	return wrapper.Interface(), nil
}

// forbiddenError turns an Authorize refusal into a Forbidden error with its
// message — unless it already carries a code (a nexus.NotFound, an
// Invalid()), which it keeps.
func forbiddenError(err error) error {
	var e *Error
	var c Code
	if errors.As(err, &e) || errors.As(err, &c) {
		return err
	}
	return &Error{Code: Forbidden, Message: err.Error(), Cause: err}
}

// inferPathArgs returns the nexus.Arg an action needs to bind its bare scalar
// parameters from the route's path parameters, positionally. Nil when the
// action takes no bare scalars (its path parameters come from a tagged struct,
// or it needs none).
func inferPathArgs(fn any, action, method, route string) (*ArgOption, error) {
	ft := reflect.TypeOf(fn)
	end := ft.NumIn()
	if end > 0 && ft.In(end-1).Kind() == reflect.Struct && !ft.In(end-1).Implements(paramsMarkerType) {
		end--
	}
	n := 0
	for i := end - 1; i >= 1 && isScalarParam(ft.In(i)); i-- {
		n++
	}
	if n == 0 {
		return nil, nil
	}
	params := routePathParams(route)
	if n != len(params) {
		return nil, fmt.Errorf("nexus: action %s at %s %s takes %d bare parameter(s) but the route has %d path parameter(s) %v — match them, or pass nexus.Arg / an args struct",
			action, method, route, n, len(params), params)
	}
	arg := Arg(params...)
	return &arg, nil
}

func isScalarParam(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// controllerName names a controller's dashboard module after its type:
// *UsersController → "users", BillingController → "billing".
func controllerName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := strings.TrimSuffix(t.Name(), "Controller")
	if name == "" {
		name = t.Name()
	}
	if name == "" {
		return "controller"
	}
	r := []rune(name)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
