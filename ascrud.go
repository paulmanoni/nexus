package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
)

// AsCRUD registers a default set of CRUD endpoints for type T.
//
// REST surface (default — always on unless WithoutREST() is passed):
//
//	GET    /<plural>          → List
//	GET    /<plural>/:id      → Read
//	POST   /<plural>          → Create
//	PATCH  /<plural>/:id      → Update
//	DELETE /<plural>/:id      → Delete
//
// GraphQL surface (opt-in via WithGraphQL()):
//
//	query    list<T>s(limit, offset, sort)
//	query    get<T>(id)
//	mutation create<T>(...)
//	mutation update<T>(id, ...)
//	mutation delete<T>(id) → Boolean
//
// The `resolver` is a function returning a Store[T] for each request.
// Its first argument must be context.Context; further arguments are
// fx-injected at boot, so depending on your DBM (or any other dep)
// just means putting it in the signature:
//
//	nexus.AsCRUD[Note](
//	    func(ctx context.Context, db *OAtsDB) (nexus.Store[Note], error) {
//	        return gormstore.For[Note](db.GormDB().WithContext(ctx)), nil
//	    },
//	    nexus.WithGraphQL(),
//	)
//
// Any resolver dep that implements NexusResources() (the framework's
// resource provider interface — DBs, caches, queues) is automatically
// linked to the generated endpoints on the dashboard, so the resource
// node fans out to every action without a manual edge declaration.
//
// Storage is per-request: the resolver runs on every action and the
// returned Store handles that one request. That makes multi-tenancy /
// read-replica routing trivial — scope your Store inside the resolver
// from anything you can pull off ctx.
//
// Convenience: if `resolver` is a `CRUDResolver[T]` (no fx deps), it's
// accepted as-is — handy for `MemoryResolver[T]` and tests.
//
//	nexus.AsCRUD[User](nexus.MemoryResolver[User](nil, nil))                       // REST only
//	nexus.AsCRUD[User](resolver, nexus.WithGraphQL())                              // REST + GraphQL
//	nexus.AsCRUD[User](resolver, nexus.WithGraphQL(), nexus.WithoutREST())         // GraphQL only
//
// Options layer over the generated endpoints — auth.Required(),
// nexus.Use(...), nexus.OnCreate(...) all work as they do for AsRest.
func AsCRUD[T any](resolver any, opts ...Option) Option {
	if resolver == nil {
		return rawOption{o: fx.Error(errBadResolver)}
	}
	bound, depTypes, err := bindCRUDResolver[T](resolver)
	if err != nil {
		return rawOption{o: fx.Error(err)}
	}
	// Lazy reader closure: captures `bound` by reference so handlers
	// see the populated fn even though bind* runs at AsCRUD's call
	// time (when fn is nil — the setup invoke fills it later, before
	// fx.Start hooks bind the listener).
	perReq := CRUDResolver[T](func(ctx context.Context) (Store[T], error) {
		if bound.fn == nil {
			return nil, errors.New("nexus: AsCRUD resolver not initialized — fx setup invoke didn't run")
		}
		return bound.fn(ctx)
	})
	// REST is the default surface; opting in to GraphQL is explicit
	// because the SDL adds a non-trivial chunk to the schema and
	// users typically pick one transport per project.
	cc := crudConfig{enableREST: true}
	for _, o := range opts {
		if c, ok := o.(crudOption); ok {
			c.applyToCRUD(&cc)
		}
	}
	if !cc.enableREST && !cc.enableGraphQL {
		return rawOption{o: fx.Error(errors.New("nexus: AsCRUD with both REST and GraphQL disabled has no effect"))}
	}
	rt := reflect.TypeOf((*T)(nil)).Elem()
	plural := defaultPlural(rt.Name())
	base := "/" + plural

	tName := rt.Name()
	registrations := make([]Option, 0, 10)

	if cc.enableREST {
		// AsRest's reflective binder accepts the handlers as-is —
		// dep scanner + arg binder + return mapper need no extra
		// machinery on the framework side.
		registrations = append(registrations,
			AsRest("GET", base, makeListHandler[T](perReq), restOpts(opts)...),
			AsRest("GET", base+"/:id", makeReadHandler[T](perReq), restOpts(opts)...),
			AsRest("POST", base, makeCreateHandler[T](perReq), restOpts(opts)...),
			AsRest("PATCH", base+"/:id", makeUpdateHandler[T](perReq), restOpts(opts)...),
			AsRest("DELETE", base+"/:id", makeDeleteHandler[T](perReq), restOpts(opts)...),
		)
	}

	if cc.enableGraphQL {
		// GraphQL handlers carry args via p.Args only (no path
		// params), so the Update variant pulls the id off the body
		// itself. Op naming follows the conventional verbs:
		// list<T>s / get<T> / create<T> / update<T> / delete<T>;
		// auto-mounts onto the module's single service via the
		// framework's default-service resolution.
		registrations = append(registrations,
			AsQuery(makeGqlListHandler[T](perReq), append(gqlOpts(opts), Op("list"+capitalize(plural)))...),
			AsQuery(makeGqlReadHandler[T](perReq), append(gqlOpts(opts), Op("get"+tName))...),
			AsMutation(makeGqlCreateHandler[T](perReq), append(gqlOpts(opts), Op("create"+tName))...),
			AsMutation(makeGqlUpdateHandler[T](perReq), append(gqlOpts(opts), Op("update"+tName))...),
			AsMutation(makeGqlDeleteHandler[T](perReq), append(gqlOpts(opts), Op("delete"+tName))...),
		)
	}

	// Setup invoke: at fx.Start, fx provides the resolver's deps,
	// we bake them into the per-request closure stored on `bound`,
	// and walk the same deps for NexusResourceProvider — registering
	// each provided resource and attaching it to every endpoint
	// generated by this AsCRUD instance. The dashboard then draws
	// resource→endpoint edges automatically; users never need to
	// declare them manually.
	setup := buildCRUDSetup[T](bound, depTypes, base, plural, tName, cc)
	registrations = append(registrations, setup)

	return optionGroup(registrations...)
}

// crudConfig collects the AsCRUD-only knobs harvested from opts
// before any AsRest / AsQuery registrations happen.
type crudConfig struct {
	enableREST    bool
	enableGraphQL bool
}

// crudOption is the marker satisfied by AsCRUD-only options. Kept
// distinct from RestOption / GqlOption so an option means exactly
// one thing: the transport-selection toggles below don't accidentally
// flow into AsRest's middleware chain.
type crudOption interface {
	applyToCRUD(*crudConfig)
}

// crudOnlyOption wraps a crudConfig mutator into something that's
// also a nexus Option (so it composes with AsCRUD's opts ...Option
// signature) but contributes nothing to the fx graph itself.
type crudOnlyOption struct {
	apply func(*crudConfig)
}

func (c crudOnlyOption) nexusOption() fx.Option       { return fx.Options() }
func (c crudOnlyOption) applyToCRUD(cc *crudConfig)   { c.apply(cc) }

// WithGraphQL turns on GraphQL op generation for AsCRUD. By default
// AsCRUD only registers REST endpoints; pass this option (and
// optionally WithoutREST) to opt in to the GraphQL surface.
//
//	nexus.AsCRUD[User](resolver, nexus.WithGraphQL())                       // REST + GraphQL
//	nexus.AsCRUD[User](resolver, nexus.WithGraphQL(), nexus.WithoutREST())  // GraphQL only
func WithGraphQL() Option {
	return crudOnlyOption{apply: func(c *crudConfig) { c.enableGraphQL = true }}
}

// WithoutREST disables REST endpoint generation for AsCRUD. Combine
// with WithGraphQL() to expose the resource over GraphQL alone.
// Without WithGraphQL, AsCRUD will return a boot error rather than
// silently registering nothing.
func WithoutREST() Option {
	return crudOnlyOption{apply: func(c *crudConfig) { c.enableREST = false }}
}

// capitalize uppercases the first rune of s — used to build CamelCase
// op names like "createUser" / "listUsers" from the plural lowercase
// noun supplied by defaultPlural.
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return string(s[0]-32) + s[1:]
}

// gqlOpts is the GraphQL twin of restOpts — picks out the options
// that target the GraphQL transport, so each registration only sees
// the subset it understands.
func gqlOpts(opts []Option) []GqlOption {
	out := make([]GqlOption, 0, len(opts))
	for _, o := range opts {
		if g, ok := o.(GqlOption); ok {
			out = append(out, g)
		}
	}
	return out
}

// optionGroup folds N Options into one, so AsCRUD can return a
// single Option that expands into multiple registrations.
//
// The wrapper forwards module / route-prefix / deployment annotations
// onto the children so a Module(...) wrapping AsCRUD still tags each
// generated endpoint correctly. Without this, the dashboard would
// show CRUD endpoints as bare service ops outside any module card,
// because Module() walks its direct children once and AsCRUD hides
// the inner AsRest/AsQuery options behind a single Option value.
type optionGroupT struct {
	o        fx.Option
	children []Option
}

func (g *optionGroupT) nexusOption() fx.Option { return g.o }

func (g *optionGroupT) setModule(name string) {
	for _, c := range g.children {
		if ma, ok := c.(moduleAnnotator); ok {
			ma.setModule(name)
		}
	}
}

func (g *optionGroupT) setRestPrefix(prefix string) {
	for _, c := range g.children {
		if rp, ok := c.(restPrefixAnnotator); ok {
			rp.setRestPrefix(prefix)
		}
	}
}

func (g *optionGroupT) setDeployment(tag string) {
	for _, c := range g.children {
		if da, ok := c.(deploymentAnnotator); ok {
			da.setDeployment(tag)
		}
	}
}

func optionGroup(opts ...Option) Option {
	fxOpts := make([]fx.Option, 0, len(opts))
	kept := make([]Option, 0, len(opts))
	for _, o := range opts {
		if o == nil {
			continue
		}
		fxOpts = append(fxOpts, o.nexusOption())
		kept = append(kept, o)
	}
	return &optionGroupT{o: fx.Options(fxOpts...), children: kept}
}

// restOpts filters the AsCRUD options down to those that AsRest
// accepts. For v1 we treat any RestOption as "applies to every
// generated endpoint"; per-action scoping (OnList/OnCreate/...)
// lands in v1.1.
func restOpts(opts []Option) []RestOption {
	out := make([]RestOption, 0, len(opts))
	for _, o := range opts {
		if r, ok := o.(RestOption); ok {
			out = append(out, r)
		}
	}
	return out
}

// ─── Handler factories ────────────────────────────────────────────
//
// Each factory returns a function with a fixed shape that AsRest's
// reflective binder accepts:
//
//	func(deps..., p Params[Args]) (Result, error)
//
// We pass *gin.Context where path-params are needed (Update / Delete
// when paired with body), and Params[T] / Params[ListOptions] /
// Params[idArg] for the rest.
//
// All five close over the same resolver. Each call resolves the Store
// fresh — the cost is one function call per request, dwarfed by
// transport overhead.

type idArg struct {
	// graphql:"id,required" pins the GraphQL arg name to "id" — the
	// reflective binder otherwise falls back to lowerFirst("ID")
	// which yields the awkward "iD". Required because every action
	// that consumes idArg (Read, Delete) must have one.
	ID string `uri:"id" json:"id" graphql:"id,required"`
}

func makeListHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[ListOptions]) (Page[T], error) {
		store, err := resolver(p.Context)
		if err != nil {
			return Page[T]{}, err
		}
		opts := p.Args
		// Defensive clamp — Stores are entitled to assume sane bounds.
		if opts.Limit <= 0 {
			opts.Limit = 20
		}
		if opts.Limit > 100 {
			opts.Limit = 100
		}
		if opts.Offset < 0 {
			opts.Offset = 0
		}
		items, total, err := store.Search(p.Context, opts)
		if err != nil {
			return Page[T]{}, err
		}
		return Page[T]{
			Items:  items,
			Total:  total,
			Limit:  opts.Limit,
			Offset: opts.Offset,
		}, nil
	}
}

func makeReadHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[idArg]) (*T, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		return store.Find(p.Context, p.Args.ID)
	}
}

func makeCreateHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[T]) (*T, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		item := p.Args
		if err := store.Save(p.Context, &item); err != nil {
			return nil, err
		}
		return &item, nil
	}
}

func makeUpdateHandler[T any](resolver CRUDResolver[T]) any {
	// gin.Context dep gives us :id without needing the body struct
	// to also carry a uri:"id" field — JSON-bound bodies don't
	// reliably surface URI params alongside.
	return func(c *gin.Context, p Params[T]) (*T, error) {
		id := c.Param("id")
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		// Load existing, shallow-merge the patch on top — fields the
		// caller didn't send keep their current value. Mirrors most
		// REST PATCH conventions.
		existing, err := store.Find(p.Context, id)
		if err != nil {
			return nil, err
		}
		merged := mergePatch(*existing, p.Args)
		// Make sure the merged record keeps its id even if the body
		// omitted (or zeroed) it.
		applyID(&merged, id)
		if err := store.Save(p.Context, &merged); err != nil {
			return nil, err
		}
		return &merged, nil
	}
}

func makeDeleteHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[idArg]) (struct{}, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return struct{}{}, err
		}
		if err := store.Remove(p.Context, p.Args.ID); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}
}

// mergePatch overlays non-zero fields of `patch` onto `base` and
// returns the merged value. v1 uses a "non-zero fields win" rule
// — simpler than JSON-merge but covers the common PATCH case
// without the framework parsing the request twice. Stores that need
// stricter semantics (only fields the JSON literal contained) can
// switch to a streaming decode in v1.1.
func mergePatch[T any](base, patch T) T {
	bv := reflect.ValueOf(&base).Elem()
	pv := reflect.ValueOf(&patch).Elem()
	if bv.Kind() != reflect.Struct {
		return patch
	}
	for i := 0; i < bv.NumField(); i++ {
		fp := pv.Field(i)
		if !fp.IsValid() || !fp.CanInterface() {
			continue
		}
		if fp.IsZero() {
			continue
		}
		bv.Field(i).Set(fp)
	}
	return base
}

// applyID writes id onto a struct's "ID" field if one is present.
// Used after Update's merge so a PATCH body that omitted ID can't
// drop the URL-derived value.
func applyID[T any](item *T, id string) {
	v := reflect.ValueOf(item).Elem()
	if v.Kind() != reflect.Struct {
		return
	}
	f := v.FieldByName("ID")
	if !f.IsValid() || !f.CanSet() || f.Kind() != reflect.String {
		return
	}
	f.SetString(id)
}

// ─── GraphQL handler factories ────────────────────────────────────
//
// GraphQL has no path params, so the Update handler reads the id
// straight off the inbound body (T's ID field). For List/Read/Delete
// the args struct carries the id (or pagination); for Create the
// args struct IS T, with each field auto-named by the framework's
// graphql-tag → field-name fallback.

func makeGqlListHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[ListOptions]) (Page[T], error) {
		store, err := resolver(p.Context)
		if err != nil {
			return Page[T]{}, err
		}
		opts := p.Args
		if opts.Limit <= 0 {
			opts.Limit = 20
		}
		if opts.Limit > 100 {
			opts.Limit = 100
		}
		if opts.Offset < 0 {
			opts.Offset = 0
		}
		items, total, err := store.Search(p.Context, opts)
		if err != nil {
			return Page[T]{}, err
		}
		return Page[T]{Items: items, Total: total, Limit: opts.Limit, Offset: opts.Offset}, nil
	}
}

func makeGqlReadHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[idArg]) (*T, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		return store.Find(p.Context, p.Args.ID)
	}
}

func makeGqlCreateHandler[T any](resolver CRUDResolver[T]) any {
	return func(p Params[T]) (*T, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		item := p.Args
		if err := store.Save(p.Context, &item); err != nil {
			return nil, err
		}
		return &item, nil
	}
}

func makeGqlUpdateHandler[T any](resolver CRUDResolver[T]) any {
	// GraphQL has no path params — the id comes in alongside the
	// patch fields on the same input. Pull it off the body via
	// reflection so the same applyID/mergePatch flow as REST works.
	return func(p Params[T]) (*T, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return nil, err
		}
		id := readID(&p.Args)
		if id == "" {
			return nil, errors.New("id is required for update")
		}
		existing, err := store.Find(p.Context, id)
		if err != nil {
			return nil, err
		}
		merged := mergePatch(*existing, p.Args)
		applyID(&merged, id)
		if err := store.Save(p.Context, &merged); err != nil {
			return nil, err
		}
		return &merged, nil
	}
}

func makeGqlDeleteHandler[T any](resolver CRUDResolver[T]) any {
	// Returning a bool keeps the SDL ergonomic — many GraphQL
	// clients reject `null` mutation responses, and `Boolean` is
	// the simplest "did this work?" signal.
	return func(p Params[idArg]) (bool, error) {
		store, err := resolver(p.Context)
		if err != nil {
			return false, err
		}
		if err := store.Remove(p.Context, p.Args.ID); err != nil {
			return false, err
		}
		return true, nil
	}
}

// readID returns the value of *t's "ID" field, or "" if no such
// field exists. Mirrors applyID's reflection but in the read
// direction; used by the GraphQL update path where the id rides on
// the body rather than a URL param.
func readID[T any](item *T) string {
	v := reflect.ValueOf(item).Elem()
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("ID")
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// MapCRUDError translates a sentinel error into the right HTTP
// status. Handler return errors flow through Gin's standard JSON
// error path, but a transport-level wrapper can use this to attach
// the right code without each Store having to know about HTTP.
//
// Currently unused by AsRest's default 500 path; reserved for the
// next pass when we wire the sentinel mapping into the framework's
// error renderer.
func MapCRUDError(err error) (status int, ok bool) {
	switch {
	case err == nil:
		return http.StatusOK, false
	case errors.Is(err, ErrCRUDNotFound):
		return http.StatusNotFound, true
	case errors.Is(err, ErrCRUDConflict):
		return http.StatusConflict, true
	case errors.Is(err, ErrCRUDValidation):
		return http.StatusBadRequest, true
	}
	return 0, false
}

// ─── Resolver binding (fx-injected deps) ───────────────────────────
//
// AsCRUD accepts the resolver as `any` so it can have fx-injected
// deps after the leading context.Context. bindCRUDResolver inspects
// the function shape, captures its dep types, and returns a holder
// whose `fn` field is filled in at fx.Start by the setup invoke.
// Handlers close over the holder, so the resolver-with-deps appears
// to them as a plain `func(ctx) (Store[T], error)`.

// resolverHolder is the indirection point between fx-injection (which
// happens once at boot) and request-time resolver invocation (which
// happens many times after). The setup invoke writes `fn` once and
// the handlers read it for every request — no contention since the
// write is fully done before fx.Start hooks bind the listener.
type resolverHolder[T any] struct {
	once sync.Once
	fn   CRUDResolver[T]
	// rv holds the original resolver's reflect.Value when it has fx
	// deps. The setup invoke closes over a pointer to this field
	// (via resolverVal()) so the resolver call site doesn't need a
	// direct reference to the AsCRUD-owned closure.
	rv reflect.Value
}

// nexusResourceProviderType caches the reflect.Type of the resource
// provider interface so the boot path doesn't recompute it per call.
var nexusResourceProviderType = reflect.TypeOf((*NexusResourceProvider)(nil)).Elem()

// bindCRUDResolver validates `resolver` and returns a holder whose
// `fn` will be populated by buildCRUDSetup once fx provides the deps.
//
// Two shapes are accepted:
//
//  1. CRUDResolver[T] — `func(ctx) (Store[T], error)` with no deps.
//     Bound directly into the holder; the setup invoke skips the
//     reflect-call path and just stamps `fn`.
//
//  2. Generalised resolver — `func(ctx, dep1, dep2, ...) (Store[T], error)`.
//     Each dep type is returned in `depTypes` so the setup invoke can
//     declare them as fx params and pass them through reflectively.
func bindCRUDResolver[T any](resolver any) (*resolverHolder[T], []reflect.Type, error) {
	holder := &resolverHolder[T]{}

	// Fast path for the common no-dep case — keeps boot allocation-
	// free for MemoryResolver and tests.
	if simple, ok := resolver.(CRUDResolver[T]); ok {
		holder.once.Do(func() { holder.fn = simple })
		return holder, nil, nil
	}

	rv := reflect.ValueOf(resolver)
	rt := rv.Type()
	if rt.Kind() != reflect.Func {
		return nil, nil, errors.New("nexus: AsCRUD resolver must be a function or CRUDResolver[T]")
	}
	if rt.NumIn() < 1 || rt.In(0) != reflect.TypeOf((*context.Context)(nil)).Elem() {
		return nil, nil, errors.New("nexus: AsCRUD resolver's first parameter must be context.Context")
	}
	if rt.NumOut() != 2 {
		return nil, nil, fmt.Errorf("nexus: AsCRUD resolver must return (Store[%s], error); got %d return values", reflect.TypeOf((*T)(nil)).Elem().Name(), rt.NumOut())
	}
	storeType := reflect.TypeOf((*Store[T])(nil)).Elem()
	if !rt.Out(0).Implements(storeType) && rt.Out(0) != storeType {
		return nil, nil, fmt.Errorf("nexus: AsCRUD resolver must return Store[%s]; got %s", reflect.TypeOf((*T)(nil)).Elem().Name(), rt.Out(0))
	}
	if !rt.Out(1).Implements(reflect.TypeOf((*error)(nil)).Elem()) && rt.Out(1) != reflect.TypeOf((*error)(nil)).Elem() {
		return nil, nil, errors.New("nexus: AsCRUD resolver's second return must be error")
	}

	depTypes := make([]reflect.Type, 0, rt.NumIn()-1)
	for i := 1; i < rt.NumIn(); i++ {
		depTypes = append(depTypes, rt.In(i))
	}
	holder.rv = rv
	return holder, depTypes, nil
}

// buildCRUDSetup returns an Option whose body is an fx.Invoke that
// runs once at boot. It (a) bakes the resolver's deps into a
// per-request closure and stores it on the holder, and (b) auto-
// attaches NexusResourceProvider deps to every endpoint this AsCRUD
// instance produced.
//
// Without this invoke, a resolver that depends on *gorm.DB (or any
// fx provider) couldn't be built at AsCRUD construction time — the
// deps simply don't exist yet. Resource attachment is layered into
// the same invoke so the work happens in one pass with one set of
// resolved values.
func buildCRUDSetup[T any](
	holder *resolverHolder[T],
	depTypes []reflect.Type,
	base, plural, tName string,
	cc crudConfig,
) Option {
	// Pre-compute the GraphQL op-name set for matching when we
	// attach resources to GraphQL endpoints. REST endpoints match
	// by path-prefix; GraphQL ones (mounted at /graphql) match by
	// op name. Pre-computing keeps the per-endpoint loop cheap.
	gqlNames := map[string]struct{}{}
	if cc.enableGraphQL {
		gqlNames["list"+capitalize(plural)] = struct{}{}
		gqlNames["get"+tName] = struct{}{}
		gqlNames["create"+tName] = struct{}{}
		gqlNames["update"+tName] = struct{}{}
		gqlNames["delete"+tName] = struct{}{}
	}

	// If the resolver had no deps, there's nothing to bake — the
	// holder was already populated synchronously in bindCRUDResolver.
	// We still emit an invoke so the resource-attachment pass runs,
	// but only the simple variants need the App.
	if len(depTypes) == 0 {
		// No deps means no resources to auto-attach, so skip the
		// invoke entirely. Saves one fx node per AsCRUD instance.
		return rawOption{o: fx.Options()}
	}

	// Build the invoke signature: func(fx.Lifecycle, *App, deps...) error.
	//
	// fx.Lifecycle is required because the resource-attach pass has
	// to wait for autoMountGraphQL — which runs AFTER user Invokes —
	// to finish populating the registry with GraphQL endpoints. We
	// register an OnStart hook (runs in the OnStart phase, after all
	// Invokes have completed) and do the attach there.
	//
	// The resolver-fn population, by contrast, happens synchronously
	// in the Invoke body so it's set before fx.Start binds the
	// listener and the first request can land.
	lifecycleType := reflect.TypeOf((*fx.Lifecycle)(nil)).Elem()
	appType := reflect.TypeOf((*App)(nil))
	inTypes := append([]reflect.Type{lifecycleType, appType}, depTypes...)
	errType := reflect.TypeOf((*error)(nil)).Elem()
	invokeType := reflect.FuncOf(inTypes, []reflect.Type{errType}, false)

	// Capture the resolver itself by closing over a value passed in
	// via the holder's auxiliary slot — we set holder.fn here, but
	// also need the resolver's reflect.Value to call it per-request.
	// Stash it on the holder via a side-channel field below.
	resolverValHolder := holder.resolverVal()

	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		lc := args[0].Interface().(fx.Lifecycle)
		app := args[1].Interface().(*App)
		deps := append([]reflect.Value(nil), args[2:]...)

		// (a) Bake deps into per-request resolver closure. Done
		// synchronously here — before any OnStart hook fires — so
		// the holder is populated before the listener binds.
		holder.once.Do(func() {
			rv := *resolverValHolder
			holder.fn = func(ctx context.Context) (Store[T], error) {
				callArgs := make([]reflect.Value, 0, 1+len(deps))
				callArgs = append(callArgs, reflect.ValueOf(ctx))
				callArgs = append(callArgs, deps...)
				out := rv.Call(callArgs)
				var s Store[T]
				if !out[0].IsNil() {
					s = out[0].Interface().(Store[T])
				}
				var rerr error
				if !out[1].IsNil() {
					rerr = out[1].Interface().(error)
				}
				return s, rerr
			}
		})

		// (b) Resource auto-attach is deferred to OnStart so it sees
		// GraphQL endpoints registered by autoMountGraphQL — that
		// Invoke runs AFTER user Invokes (it lives in fxLateOptions),
		// so any synchronous attach here would only see REST routes.
		// By the time OnStart hooks fire, every Invoke has completed.
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				attachCRUDResources(app, deps, base, gqlNames)
				return nil
			},
		})

		return []reflect.Value{reflect.Zero(errType)}
	})

	return rawOption{o: fx.Invoke(invokeFn.Interface())}
}

// resolverVal is a side-channel field on resolverHolder used to hand
// the original resolver's reflect.Value across to buildCRUDSetup
// without exposing it on the public-ish holder struct. We attach it
// inside bindCRUDResolver right before returning.
//
// Implemented as a method that returns a pointer slot so callers can
// both read and assign — buildCRUDSetup closes over the pointer and
// reads it after AsCRUD's caller has stamped the resolver in.
func (h *resolverHolder[T]) resolverVal() *reflect.Value {
	return &h.rv
}

// attachCRUDResources is the "find every endpoint this AsCRUD owns
// and link the resource to it" pass. Run once per AsCRUD at boot,
// after fx has resolved the resolver's deps. Idempotent — registry
// methods de-duplicate, so a second call wouldn't double-attach.
func attachCRUDResources(app *App, deps []reflect.Value, base string, gqlNames map[string]struct{}) {
	for _, dv := range deps {
		if !dv.IsValid() {
			continue
		}
		provider, ok := dv.Interface().(NexusResourceProvider)
		if !ok {
			continue
		}
		resources := provider.NexusResources()
		if len(resources) == 0 {
			continue
		}
		for _, r := range resources {
			app.Registry().RegisterResource(r)
		}
		// Two-level wiring so both dashboard surfaces light up:
		//   1. AttachResource (service-level) → architecture canvas
		//      draws a service → resource edge.
		//   2. SetEndpointResources (per-endpoint) → endpoint drawer
		//      renders the resource as a chip on each row.
		// Without (2), the resource shows on the canvas but not on
		// individual op rows, and vice versa.
		for _, ep := range app.Registry().Endpoints() {
			if !endpointBelongsToCRUD(ep.Path, ep.Name, base, gqlNames) {
				continue
			}
			names := make([]string, 0, len(resources))
			for _, r := range resources {
				app.Registry().AttachResource(ep.Service, r.Name())
				names = append(names, r.Name())
			}
			app.Registry().SetEndpointResources(ep.Service, ep.Name, names)
		}
	}
}

// endpointBelongsToCRUD identifies endpoints generated by a specific
// AsCRUD instance. REST endpoints share a known path prefix; GraphQL
// endpoints share a known op-name set. The path-prefix check has to
// match exactly base or base+"/" so we don't grab unrelated paths
// that happen to start with the same string (e.g. "/notescheduled"
// would otherwise match "/notes").
func endpointBelongsToCRUD(path, name, base string, gqlNames map[string]struct{}) bool {
	if path == base || strings.HasPrefix(path, base+"/") {
		return true
	}
	if _, ok := gqlNames[name]; ok {
		return true
	}
	return false
}