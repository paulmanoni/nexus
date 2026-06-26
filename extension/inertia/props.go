package inertia

// propKind classifies how a prop participates in Inertia's prop-evaluation
// rules (see Engine.resolveProps). The zero value, kindPlain, is the ordinary
// case: a value that is always sent on a full visit and sent on a partial
// reload only when explicitly requested.
type propKind int

const (
	kindPlain     propKind = iota // always on full visit; on partial only if requested
	kindOptional                  // NEVER on full visit; only on partial when requested
	kindAlways                    // always sent, even on partials that don't request it
	kindDefer                     // like Optional, but advertised so the client auto-fetches it
	kindMerge                     // like plain, but flagged so the client shallow-merges (pagination)
	kindDeepMerge                 // like Merge, but flagged for a recursive deep merge
)

// Prop is a typed wrapper that overrides a props-struct field's evaluation
// rules. Construct one with Optional / Lazy / Always and use it as a field
// type on a page's props struct:
//
//	type DashboardProps struct {
//	    User  User          `json:"user"`           // plain: always sent
//	    Stats inertia.Prop  `json:"stats"`          // = inertia.Optional(...)
//	    Menu  inertia.Prop  `json:"menu"`           // = inertia.Always(...)
//	}
//
// A Prop carries either an eager value (Always) or a thunk (Optional/Lazy)
// that the engine evaluates ONLY when the prop is actually included in the
// response — so an expensive Optional prop costs nothing on the (common)
// visits that don't request it.
type Prop struct {
	kind    propKind
	val     any
	fn      func() (any, error)
	group   string // Defer group name; props in one group are fetched together
	matchOn string // Merge/DeepMerge: child field to match items on (infinite scroll)
}

// resolve returns the prop's value, invoking the thunk if it has one. Called
// by the engine only for props that survive the inclusion rules, so the thunk
// never runs for an excluded Optional prop.
func (p Prop) resolve() (any, error) {
	if p.fn != nil {
		return p.fn()
	}
	return p.val, nil
}

// Optional marks a prop that is excluded from the initial (full) page visit
// and evaluated only when a partial reload explicitly asks for it via
// X-Inertia-Partial-Data. This is the primary performance lever: wrap heavy
// data (aggregates, counts, related lists) so it isn't computed on every
// navigation.
//
//	Stats: inertia.Optional(func() (Stats, error) { return svc.ExpensiveStats() }),
func Optional[T any](fn func() (T, error)) Prop {
	return Prop{kind: kindOptional, fn: func() (any, error) { return fn() }}
}

// Lazy is the historical Inertia name for Optional; identical behaviour.
func Lazy[T any](fn func() (T, error)) Prop { return Optional(fn) }

// Always marks a prop that is sent on every response — including partial
// reloads that do not list it in X-Inertia-Partial-Data. Use sparingly for
// data the client must never lose across partial updates (flash messages,
// nav state).
//
//	Menu: inertia.Always(menu),
func Always[T any](v T) Prop {
	return Prop{kind: kindAlways, val: v}
}

// Defer marks a prop that is excluded from the initial visit but advertised in
// the page object's deferredProps, so the Inertia client automatically fetches
// it in a follow-up partial request right after the page mounts. Use it to get
// the page on screen fast, then stream in slower data.
//
//	Activity: inertia.Defer(func() (Feed, error) { return svc.RecentActivity() }),
//
// An optional group name batches deferred props: all props in the same group
// are fetched in one request, and DIFFERENT groups are fetched in PARALLEL.
// Omit it (or pass "") for the "default" group. Group a slow report separately
// from a fast sidebar so neither waits on the other:
//
//	Report:  inertia.Defer(svc.HeavyReport, "report"),
//	Sidebar: inertia.Defer(svc.Sidebar,     "sidebar"),
func Defer[T any](fn func() (T, error), group ...string) Prop {
	g := "default"
	if len(group) > 0 && group[0] != "" {
		g = group[0]
	}
	return Prop{kind: kindDefer, group: g, fn: func() (any, error) { return fn() }}
}

// Merge marks a prop that is sent normally but flagged in the page object's
// mergeProps, telling the client to (shallow) merge it with the existing prop
// value instead of replacing it — the basis for "load more" pagination and
// infinite scroll.
//
//	Items: inertia.Merge(func() ([]Item, error) { return svc.Page(cursor) }),
//
// An optional matchOn names a child field used to de-duplicate while merging
// (added to the page object's matchPropsOn as "<prop>.<matchOn>") — so a re-sent
// item updates in place instead of appending a duplicate:
//
//	Items: inertia.Merge(svc.Page, "id"),   // matchPropsOn: ["items.id"]
func Merge[T any](fn func() (T, error), matchOn ...string) Prop {
	return mergeProp(kindMerge, func() (any, error) { return fn() }, matchOn)
}

// DeepMerge is Merge with a recursive merge: nested objects/arrays in the prop
// are deep-merged into the existing value rather than shallow-replaced. Flagged
// in the page object's deepMergeProps. Like Merge, it accepts an optional
// matchOn de-dup key.
//
//	Feed: inertia.DeepMerge(svc.Feed, "id"),
func DeepMerge[T any](fn func() (T, error), matchOn ...string) Prop {
	return mergeProp(kindDeepMerge, func() (any, error) { return fn() }, matchOn)
}

func mergeProp(kind propKind, fn func() (any, error), matchOn []string) Prop {
	p := Prop{kind: kind, fn: fn}
	if len(matchOn) > 0 {
		p.matchOn = matchOn[0]
	}
	return p
}
