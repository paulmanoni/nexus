package inertia

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
)

// Header names defined by the Inertia protocol.
const (
	headerInertia          = "X-Inertia"
	headerVersion          = "X-Inertia-Version"
	headerLocation         = "X-Inertia-Location"
	headerPartialData      = "X-Inertia-Partial-Data"
	headerPartialComponent = "X-Inertia-Partial-Component"
)

// pageObject is the Inertia page payload: the component to render, its props,
// the canonical URL, and the asset version. Serialized as JSON for XHR visits
// and embedded in the document shell for full loads.
type pageObject struct {
	Component string         `json:"component"`
	Props     map[string]any `json:"props"`
	URL       string         `json:"url"`
	Version   string         `json:"version"`
}

// render writes the Inertia response for a page handler's return value. The
// header X-Inertia distinguishes an XHR visit (JSON page object) from an
// initial browser navigation (HTML document shell). Either way the props are
// resolved once, honoring partial-reload and Optional/Always rules.
func (e *Engine) render(c *gin.Context, component string, result any) error {
	props, err := e.resolveProps(c, component, result)
	if err != nil {
		return err
	}
	page := pageObject{
		Component: component,
		Props:     props,
		URL:       c.Request.URL.RequestURI(),
		Version:   e.version,
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

	// Initial load: full HTML document with the page embedded.
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
//   - partial reload (X-Inertia-Partial-Component == component): only the props
//     named in X-Inertia-Partial-Data, plus every Always prop.
//
// Optional/Always thunks are evaluated only for props that survive inclusion,
// so excluded heavy props cost nothing. A thunk error aborts the render before
// anything is written (surfaces as a 500 via the renderer).
func (e *Engine) resolveProps(c *gin.Context, component string, result any) (map[string]any, error) {
	partial := c.GetHeader(headerPartialComponent) == component
	only := parseList(c.GetHeader(headerPartialData))

	include := func(key string, kind propKind) bool {
		switch {
		case kind == kindAlways:
			return true
		case partial:
			return only[key]
		case kind == kindOptional:
			return false
		default:
			return true
		}
	}

	out := make(map[string]any)

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
			return out, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return out, nil
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
		if !include(key, kind) {
			continue
		}
		val, err := resolve()
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, nil
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
