package client

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/registry"
)

// pageRoute is one URL that renders an Inertia component, as the
// generated pageUrl helper sees it.
type pageRoute struct {
	// Path is the route template relative to the manifest's BasePath,
	// with :name / *name segments.
	Path string `json:"path"`
	// Params are the template's path parameters in order.
	Params []string `json:"params"`
	// Query are the handler's URL-query arguments (query:/form: tagged
	// fields) — not in the runtime table (any extra key becomes a query
	// parameter there), only in the typings.
	Query []pageQuery `json:"-"`
}

type pageQuery struct {
	Key string
	TS  string
}

// pageRoutes groups the manifest's Inertia page routes by component — the
// table behind pageUrl. Per component:
//
//   - GET routes only, unless the component has none (a POST-only page is
//     still somewhere to send a form).
//   - Trailing-slash twins ("/users" and "/users/") collapse to the form
//     without the slash.
//   - Ordered most path parameters first, then by path, so the runtime's
//     first-match picks the most specific route the caller supplied
//     parameters for.
func pageRoutes(m Manifest) map[string][]pageRoute {
	type cand struct {
		e   EndpointInfo
		get bool
	}
	byComponent := map[string][]cand{}
	for _, e := range m.Endpoints {
		if e.Page == "" || e.Transport != string(registry.REST) {
			continue
		}
		byComponent[e.Page] = append(byComponent[e.Page], cand{e, strings.EqualFold(e.Method, "GET")})
	}
	out := make(map[string][]pageRoute, len(byComponent))
	for comp, cands := range byComponent {
		hasGet := false
		for _, c := range cands {
			hasGet = hasGet || c.get
		}
		seen := map[string]int{} // normalized path → index in routes
		var routes []pageRoute
		for _, c := range cands {
			if hasGet && !c.get {
				continue
			}
			norm := trimTrailingSlash(c.e.Path)
			if i, ok := seen[norm]; ok {
				// Keep the slash-less twin; a POST twin of a GET path adds nothing.
				if routes[i].Path != norm && c.e.Path == norm {
					routes[i].Path = norm
				}
				routes[i].Query = mergeQuery(routes[i].Query, pageQueryParams(c.e.Args, m.Refs, routes[i].Params))
				continue
			}
			params := pathParams(c.e.Path)
			seen[norm] = len(routes)
			routes = append(routes, pageRoute{
				Path:   c.e.Path,
				Params: params,
				Query:  pageQueryParams(c.e.Args, m.Refs, params),
			})
		}
		sort.SliceStable(routes, func(i, j int) bool {
			if len(routes[i].Params) != len(routes[j].Params) {
				return len(routes[i].Params) > len(routes[j].Params)
			}
			return routes[i].Path < routes[j].Path
		})
		out[comp] = routes
	}
	return out
}

func trimTrailingSlash(p string) string {
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		return strings.TrimRight(p, "/")
	}
	return p
}

// pathParams lists a route template's :name and *name segments.
func pathParams(path string) []string {
	out := []string{} // [] not null in the JSON route table
	for _, seg := range strings.Split(path, "/") {
		if len(seg) > 1 && (seg[0] == ':' || seg[0] == '*') {
			out = append(out, seg[1:])
		}
	}
	return out
}

// pageQueryParams lists the args fields the REST binder reads from the URL
// query (query:/form: tags), skipping names the path already carries.
func pageQueryParams(args *registry.TypeRef, refs map[string]registry.NamedType, pathNames []string) []pageQuery {
	fields := argFields(args, refs)
	var out []pageQuery
	for _, f := range fields {
		if f.Query == "" || containsName(pathNames, f.Query) {
			continue
		}
		out = append(out, pageQuery{Key: f.Query, TS: queryTSType(f.Type)})
	}
	return out
}

func argFields(t *registry.TypeRef, refs map[string]registry.NamedType) []registry.FieldSchema {
	if t == nil {
		return nil
	}
	switch t.Kind {
	case "ref":
		if nt, ok := refs[t.Ref]; ok {
			return nt.Fields
		}
	case "object":
		if t.Object != nil {
			return t.Object.Fields
		}
	}
	return nil
}

func mergeQuery(a, b []pageQuery) []pageQuery {
	for _, q := range b {
		dup := false
		for _, have := range a {
			if have.Key == q.Key {
				dup = true
				break
			}
		}
		if !dup {
			a = append(a, q)
		}
	}
	return a
}

func containsName(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// queryTSType is the TypeScript type a query parameter accepts. Numbers
// also accept strings: an id may reach the page as a masked string
// (extension/maskid) and goes onto the URL as text either way.
func queryTSType(t registry.TypeRef) string {
	switch t.Kind {
	case "primitive":
		switch t.Primitive {
		case "integer", "number":
			return "number | string"
		case "boolean":
			return "boolean"
		case "string":
			return "string"
		}
	case "array":
		if t.Of != nil && t.Of.Kind == "primitive" {
			return "ReadonlyArray<" + queryTSType(*t.Of) + ">"
		}
	}
	return "PageQueryValue"
}

// pageAction is one Inertia form action as pageAction sees it.
type pageAction struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Params []string `json:"params"`
}

// pageActions picks one route per form action (inertia.Resource's write
// routes): POST before PUT before PATCH before DELETE, slash-less twins first.
func pageActions(m Manifest) map[string]pageAction {
	rank := map[string]int{"POST": 0, "PUT": 1, "PATCH": 2, "DELETE": 3}
	out := map[string]pageAction{}
	for _, e := range m.Endpoints {
		if e.Action == "" || e.Transport != string(registry.REST) {
			continue
		}
		method := strings.ToUpper(e.Method)
		if have, ok := out[e.Action]; ok {
			hr := rank[strings.ToUpper(have.Method)]
			if rank[method] > hr || (rank[method] == hr && len(have.Path) <= len(e.Path)) {
				continue
			}
		}
		out[e.Action] = pageAction{Method: strings.ToLower(method), Path: e.Path, Params: pathParams(e.Path)}
	}
	return out
}

func pageComponents(routes map[string][]pageRoute) []string {
	comps := make([]string, 0, len(routes))
	for c := range routes {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	return comps
}

// GeneratePagesJS projects a manifest into pages.js — the page-URL helper
// for Inertia apps:
//
//	import { pageUrl } from 'nexus-client/pages'
//	router.visit(pageUrl('Users/Show', { id: user.id }))   // → /users/42
//
// The route table is data generated from the registered inertia.Page
// routes; the function substitutes path parameters, turns every other
// parameter into the query string, and prefixes the manifest's BasePath.
// Returns "" when the manifest has no pages.
func GeneratePagesJS(m Manifest) string {
	routes, actions := pageRoutes(m), pageActions(m)
	if len(routes) == 0 && len(actions) == 0 {
		return ""
	}
	table := map[string][]pageRoute{}
	for c, rs := range routes {
		table[c] = rs
	}
	body, _ := json.MarshalIndent(table, "", "  ")
	actionBody, _ := json.MarshalIndent(actions, "", "  ")
	base, _ := json.Marshal(m.BasePath)

	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString("// Page URLs for the app's Inertia pages. Pairs with pages.d.ts.\n")
	fmt.Fprintf(&b, "// Schema version: %s\n\n", m.Version)
	fmt.Fprintf(&b, "const basePath = %s\n\n", base)
	fmt.Fprintf(&b, "export const pageRoutes = %s\n\n", body)
	fmt.Fprintf(&b, "export const pageActions = %s\n\n", actionBody)
	b.WriteString(pagesRuntimeJS)
	return b.String()
}

const pagesRuntimeJS = `const filled = (v) => v !== undefined && v !== null && v !== ''

export function pageUrl(component, params, opts) {
  const routes = pageRoutes[component]
  if (!routes) throw new Error('pageUrl: no page is registered for component "' + component + '"')
  const p = params || {}
  const o = opts || {}
  let route
  if (o.route) {
    route = routes.find((r) => r.path === o.route)
    if (!route) throw new Error('pageUrl: "' + component + '" has no route ' + o.route + ' (routes: ' + routes.map((r) => r.path).join(', ') + ')')
    const missing = route.params.filter((k) => !filled(p[k]))
    if (missing.length) throw new Error('pageUrl: ' + o.route + ' needs ' + missing.join(', '))
  } else {
    route = routes.find((r) => r.params.every((k) => filled(p[k])))
    if (!route) throw new Error('pageUrl: "' + component + '" needs path parameters for one of: ' + routes.map((r) => r.path).join(', '))
  }
  const path = buildPath(route.path, p)
  const qs = new URLSearchParams()
  const add = (k, v) => {
    if (v === undefined || v === null) return
    if (Array.isArray(v)) { for (const x of v) add(k, x); return }
    qs.append(k, String(v))
  }
  for (const k of Object.keys(p)) if (!route.params.includes(k)) add(k, p[k])
  if (o.query) for (const k of Object.keys(o.query)) add(k, o.query[k])
  const q = qs.toString()
  return basePath + path + (q ? '?' + q : '')
}

export function pageAction(action, params) {
  const a = pageActions[action]
  if (!a) throw new Error('pageAction: no form action is registered as "' + action + '"')
  const p = params || {}
  const missing = a.params.filter((k) => !filled(p[k]))
  if (missing.length) throw new Error('pageAction: "' + action + '" needs ' + missing.join(', '))
  return [a.method, basePath + buildPath(a.path, p)]
}

function buildPath(path, p) {
  return path.replace(/([:*])([A-Za-z0-9_]+)/g, (_, kind, name) =>
    kind === '*'
      ? String(p[name]).split('/').map(encodeURIComponent).join('/')
      : encodeURIComponent(String(p[name])))
}
`

// GeneratePagesDTS projects a manifest into pages.d.ts, the typings for
// pages.js. NexusPageRoutes maps each component to the union of its routes'
// parameters — path parameters required, the handler's query arguments
// optional — so a wrong component, a missing id, or a misspelled parameter
// is a compile error. Returns "" when the manifest has no pages.
func GeneratePagesDTS(m Manifest) string {
	routes, actions := pageRoutes(m), pageActions(m)
	if len(routes) == 0 && len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString("// Pairs with pages.js.\n")
	fmt.Fprintf(&b, "// Schema version: %s\n\n", m.Version)
	b.WriteString("export type PageQueryValue = string | number | boolean | null | undefined | ReadonlyArray<string | number | boolean>\n\n")

	comps := pageComponents(routes)
	b.WriteString("/** Parameters per page component: one member per route that renders it. */\n")
	b.WriteString("export interface NexusPageRoutes {\n")
	for _, c := range comps {
		rs := routes[c]
		paths := make([]string, len(rs))
		for i, r := range rs {
			paths[i] = r.Path
		}
		fmt.Fprintf(&b, "  /** %s */\n", escapeComment(strings.Join(paths, " · ")))
		members := make([]string, 0, len(rs))
		for _, r := range rs {
			members = append(members, routeParamsType(r))
		}
		members = uniqueStrings(members)
		if len(members) == 1 {
			fmt.Fprintf(&b, "  %s: %s\n", tsLiteral(c), members[0])
		} else {
			fmt.Fprintf(&b, "  %s:\n", tsLiteral(c))
			for _, mem := range members {
				fmt.Fprintf(&b, "    | %s\n", mem)
			}
		}
	}
	b.WriteString("}\n\n")

	b.WriteString("/** Route templates per page component, for PageUrlOptions.route. */\n")
	b.WriteString("export interface NexusPageRouteTemplates {\n")
	for _, c := range comps {
		lits := make([]string, 0, len(routes[c]))
		for _, r := range routes[c] {
			lits = append(lits, tsLiteral(r.Path))
		}
		fmt.Fprintf(&b, "  %s: %s\n", tsLiteral(c), strings.Join(lits, " | "))
	}
	b.WriteString("}\n\n")

	b.WriteString(`export type PageComponent = keyof NexusPageRoutes

export interface PageUrlOptions<C extends PageComponent = PageComponent> {
  /** Use this route when several routes render the component. */
  route?: NexusPageRouteTemplates[C]
  /** Extra query parameters the page's Go handler doesn't declare. */
  query?: Record<string, PageQueryValue>
}

/**
 * The URL of an Inertia page, built from its registered route:
 *
 *   router.visit(pageUrl('Users/Show', { id: user.id }))
 *
 * Path parameters are substituted, every other parameter becomes the query
 * string, and the app's route prefix is applied. When several routes render
 * the component, the most specific one whose path parameters are all given
 * is used; opts.route picks one explicitly.
 */
export declare function pageUrl<C extends PageComponent>(
  component: C,
  ...args: {} extends NexusPageRoutes[C]
    ? [params?: NexusPageRoutes[C], opts?: PageUrlOptions<C>]
    : [params: NexusPageRoutes[C], opts?: PageUrlOptions<C>]
): string

export declare const pageRoutes: {
  readonly [C in PageComponent]: ReadonlyArray<{ readonly path: NexusPageRouteTemplates[C]; readonly params: readonly string[] }>
}

export type PageActionName = keyof NexusPageActions
export type PageActionMethod = 'post' | 'put' | 'patch' | 'delete'

/**
 * The method and URL of an Inertia form action (inertia.Resource's Create,
 * Update, Destroy), ready for useForm's submit:
 *
 *   form.submit(...pageAction('Users/Update', { id: user.id }))
 */
export declare function pageAction<A extends PageActionName>(
  action: A,
  ...args: {} extends NexusPageActions[A] ? [params?: NexusPageActions[A]] : [params: NexusPageActions[A]]
): [method: PageActionMethod, url: string]
`)
	writePageActionTypes(&b, actions)
	return b.String()
}

// routeParamsType renders one route's parameter object type.
func routeParamsType(r pageRoute) string {
	parts := make([]string, 0, len(r.Params)+len(r.Query))
	for _, p := range r.Params {
		parts = append(parts, fmt.Sprintf("%s: string | number", tsKey(p)))
	}
	for _, q := range r.Query {
		parts = append(parts, fmt.Sprintf("%s?: %s", tsKey(q.Key), q.TS))
	}
	if len(parts) == 0 {
		// Not {}: TypeScript skips excess-property checks against an empty
		// type, so pageUrl('X', { idd: 1 }) would slip through a union
		// member of {}. This still accepts {} and no argument.
		return "{ [key: string]: never }"
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}

func uniqueStrings(in []string) []string {
	out := in[:0]
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// WritePagesFiles writes pages.js and pages.d.ts into outDir when the
// manifest has pages, and removes previously generated copies when it no
// longer does (files without the generated banner are left alone). Shared
// by Dump and `nexus client`.
func WritePagesFiles(outDir string, m Manifest, stdout io.Writer) error {
	js, dts := GeneratePagesJS(m), GeneratePagesDTS(m)
	for _, f := range []struct{ name, body string }{{"pages.js", js}, {"pages.d.ts", dts}} {
		if err := writeOrRemoveGenerated(filepath.Join(outDir, f.name), []byte(f.body), "no Inertia pages", stdout); err != nil {
			return err
		}
	}
	return nil
}

// writePageActionTypes emits NexusPageActions: each form action's path
// parameters (its body is the form's data, not part of the URL).
func writePageActionTypes(b *strings.Builder, actions map[string]pageAction) {
	names := make([]string, 0, len(actions))
	for n := range actions {
		names = append(names, n)
	}
	sort.Strings(names)
	b.WriteString("\n/** Path parameters per Inertia form action. */\n")
	b.WriteString("export interface NexusPageActions {\n")
	for _, n := range names {
		a := actions[n]
		fmt.Fprintf(b, "  /** %s %s */\n", strings.ToUpper(a.Method), escapeComment(a.Path))
		fmt.Fprintf(b, "  %s: %s\n", tsLiteral(n), routeParamsType(pageRoute{Params: a.Params}))
	}
	b.WriteString("}\n")
}
