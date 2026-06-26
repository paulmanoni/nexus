package inertia

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// Header names defined by the Inertia protocol.
const (
	headerInertia          = "X-Inertia"
	headerVersion          = "X-Inertia-Version"
	headerLocation         = "X-Inertia-Location"
	headerPartialData      = "X-Inertia-Partial-Data"
	headerPartialExcept    = "X-Inertia-Partial-Except"
	headerPartialComponent = "X-Inertia-Partial-Component"
	headerReset            = "X-Inertia-Reset"
)

// pageObject is the Inertia page payload: the component to render, its props,
// the canonical URL, and the asset version. Serialized as JSON for XHR visits
// and embedded in the document shell for full loads.
type pageObject struct {
	Component string         `json:"component"`
	Props     map[string]any `json:"props"`
	URL       string         `json:"url"`
	Version   string         `json:"version"`
	// DeferredProps groups the keys of Defer props the client should
	// auto-fetch after mount. Keyed by group name ("default" here);
	// omitted when there are none.
	DeferredProps map[string][]string `json:"deferredProps,omitempty"`
	// MergeProps lists Merge prop keys the client should merge with the
	// existing value rather than replace. Omitted when there are none.
	MergeProps []string `json:"mergeProps,omitempty"`
}

// render writes the Inertia response for a page handler's return value. The
// header X-Inertia distinguishes an XHR visit (JSON page object) from an
// initial browser navigation (HTML document shell). Either way the props are
// resolved once, honoring partial-reload and Optional/Always rules.
func (e *Engine) render(c *httpx.Ctx, component string, result any) error {
	// Asset-version guard: a stale X-Inertia-Version on a GET XHR visit forces
	// a fresh full load (the client follows X-Inertia-Location). Checked here
	// (rather than in global middleware) so it works regardless of route
	// registration order — see Module.
	if e.version != "" && c.Request.Method == http.MethodGet && c.GetHeader(headerInertia) != "" {
		if v := c.GetHeader(headerVersion); v != "" && v != e.version {
			c.Header(headerLocation, c.Request.URL.RequestURI())
			c.AbortWithStatus(http.StatusConflict)
			return nil
		}
	}

	props, meta, err := e.resolveProps(c, component, result)
	if err != nil {
		return err
	}
	page := pageObject{
		Component: component,
		Props:     props,
		URL:       c.Request.URL.RequestURI(),
		Version:   e.version,
	}
	if len(meta.deferred) > 0 {
		page.DeferredProps = map[string][]string{"default": meta.deferred}
	}
	if len(meta.merge) > 0 {
		page.MergeProps = meta.merge
	}
	blob, err := json.Marshal(page)
	if err != nil {
		return err
	}

	if c.GetHeader(headerInertia) != "" {
		// XHR visit: raw page object. Vary so caches don't serve the JSON
		// to a full-load request (or vice versa) for the same URL.
		c.Header("Vary", headerInertia)
		c.Header(headerInertia, "true")
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Status(http.StatusOK)
		_, err = c.Writer.Write(blob)
		return err
	}

	// Initial load: full HTML document with the page embedded. Vary on
	// X-Inertia so a shared cache never serves this HTML to a later XHR visit
	// of the same URL (which expects the JSON page object), or vice versa.
	c.Header("Vary", headerInertia)
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	_, err = c.Writer.Write(e.shell(blob))
	return err
}

// resolveProps builds the props map for a render, merging shared props with the
// handler's props struct and applying Inertia's inclusion rules:
//
//   - full visit (no matching partial header): every plain + Always prop;
//     Optional props are skipped.
//   - partial reload (X-Inertia-Partial-Component == component): the props named
//     in X-Inertia-Partial-Data (or, when that's empty, all plain props),
//     minus any named in X-Inertia-Partial-Except, plus every Always prop.
//
// X-Inertia-Partial-Except takes precedence: a key listed in both Data and
// Except is excluded. Always props ignore the partial filters entirely.
//
// Optional/Always thunks are evaluated only for props that survive inclusion,
// so excluded heavy props cost nothing. A thunk error aborts the render before
// anything is written (surfaces as a 500 via the renderer).
// propsMeta carries the page-object metadata that Defer/Merge props produce
// alongside the resolved props map.
type propsMeta struct {
	deferred []string // Defer keys advertised on a full visit
	merge    []string // Merge keys included in this response
}

func (e *Engine) resolveProps(c *httpx.Ctx, component string, result any) (map[string]any, propsMeta, error) {
	partial := c.GetHeader(headerPartialComponent) == component
	only := parseList(c.GetHeader(headerPartialData))
	except := parseList(c.GetHeader(headerPartialExcept))
	// reset names Merge props the client wants replaced rather than merged this
	// round ("load more" → refresh): they're still sent, just not flagged as
	// merge below.
	reset := parseList(c.GetHeader(headerReset))

	// include reports whether a prop of the given kind is sent in this
	// response. On a partial reload Except removes a prop even when Data lists
	// it; Always props bypass both filters.
	include := func(key string, kind propKind) bool {
		if kind == kindAlways {
			return true
		}
		if partial && except[key] {
			return false
		}
		switch kind {
		case kindOptional, kindDefer:
			// Never on a full visit; on a partial only when explicitly named.
			return partial && only[key]
		default: // kindPlain, kindMerge
			if partial && len(only) > 0 {
				return only[key]
			}
			return true
		}
	}

	out := make(map[string]any)
	var meta propsMeta

	// Shared props participate as plain props.
	ctx := c.Request.Context()
	for _, sp := range e.shared {
		key, val := sp(ctx)
		if key == "" || !include(key, kindPlain) {
			continue
		}
		out[key] = val
	}

	// Handler props struct.
	rv := reflect.ValueOf(result)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return out, meta, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return out, meta, nil
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		key := wireName(f)
		if key == "-" {
			continue
		}
		kind, resolve := classifyProp(rv.Field(i))

		// A deferred prop that isn't being sent this round is advertised
		// on full visits so the client knows to request it next.
		if kind == kindDefer && !partial {
			meta.deferred = append(meta.deferred, key)
		}
		if !include(key, kind) {
			continue
		}
		val, err := resolve()
		if err != nil {
			return nil, propsMeta{}, err
		}
		out[key] = val
		if kind == kindMerge && !reset[key] {
			meta.merge = append(meta.merge, key)
		}
	}
	return out, meta, nil
}

// classifyProp inspects a struct field value: a Prop carries its own kind and
// thunk; anything else is a plain field whose value is returned as-is.
func classifyProp(fv reflect.Value) (propKind, func() (any, error)) {
	if fv.CanInterface() {
		if p, ok := fv.Interface().(Prop); ok {
			return p.kind, p.resolve
		}
	}
	v := fv.Interface()
	return kindPlain, func() (any, error) { return v, nil }
}

// wireName returns the JSON key for a struct field, honoring the `json` tag
// (name and "-" omission). Without a tag the Go field name is used verbatim.
func wireName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

// parseList turns a comma-separated header value into a set for membership
// tests, ignoring blank entries.
func parseList(s string) map[string]bool {
	if s == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}
