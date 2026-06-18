package httpx

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Neutral request binding — replaces gin's ShouldBindUri / ShouldBindQuery /
// ShouldBindHeader / ShouldBind / ShouldBindJSON. Each binder walks the struct
// once and fills fields from its own source (path params, query, headers, form,
// JSON body). Supported field kinds cover nexus's args usage: string, the int
// and uint families, float, bool, and []string (repeated query/form values).
//
// nexus does field validation separately via `validate:` tags, so binding here
// is decode-only — matching how gin behaved for nexus args (no `binding:"..."`
// enforcement was relied on).

// ShouldBindUri fills fields tagged `uri:"name"` from path params.
func (c *Ctx) ShouldBindUri(ptr any) error {
	return bindFromTag(ptr, "uri", func(name string) ([]string, bool) {
		v := c.Param(name)
		if v == "" {
			return nil, false
		}
		return []string{v}, true
	})
}

// ShouldBindQuery fills fields tagged `query:"name"` (or `form:"name"`) from
// the URL query.
func (c *Ctx) ShouldBindQuery(ptr any) error {
	q := c.Request.URL.Query()
	return bindFromTag(ptr, "query", func(name string) ([]string, bool) {
		vs, ok := q[name]
		return vs, ok
	}, "form")
}

// ShouldBindHeader fills fields tagged `header:"Name"` from request headers.
func (c *Ctx) ShouldBindHeader(ptr any) error {
	return bindFromTag(ptr, "header", func(name string) ([]string, bool) {
		vs, ok := c.Request.Header[textproto(name)]
		return vs, ok
	})
}

// ShouldBind fills fields tagged `form:"name"` from a parsed form body
// (url-encoded or multipart).
func (c *Ctx) ShouldBind(ptr any) error {
	if err := c.Request.ParseForm(); err != nil {
		return err
	}
	// multipart is best-effort; ignore the error when there's no such body.
	_ = c.Request.ParseMultipartForm(32 << 20)
	form := c.Request.Form
	return bindFromTag(ptr, "form", func(name string) ([]string, bool) {
		vs, ok := form[name]
		return vs, ok
	})
}

// ShouldBindJSON decodes the request body as JSON into ptr.
func (c *Ctx) ShouldBindJSON(ptr any) error {
	if c.Request.Body == nil {
		return nil
	}
	return json.NewDecoder(c.Request.Body).Decode(ptr)
}

// BindJSON decodes JSON and, on error, writes a 400 + aborts (gin's c.BindJSON).
func (c *Ctx) BindJSON(ptr any) error {
	if err := c.ShouldBindJSON(ptr); err != nil {
		c.AbortWithStatusJSON(400, H{"error": err.Error()})
		return err
	}
	return nil
}

// textproto canonicalizes a header key (Authorization, X-Real-IP, …) so the
// raw map lookup in ShouldBindHeader hits.
func textproto(name string) string {
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '-' }), "-")
}

// bindFromTag is the shared reflection walk. lookup returns the raw string
// values for a tag name (and whether present). altTags lets ShouldBindQuery
// also honor `form` tags.
func bindFromTag(ptr any, tag string, lookup func(string) ([]string, bool), altTags ...string) error {
	rv := reflect.ValueOf(ptr)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("bind: target must be a non-nil pointer")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("bind: target must point to a struct")
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := f.Tag.Lookup(tag)
		if !ok {
			for _, alt := range altTags {
				if name, ok = f.Tag.Lookup(alt); ok {
					break
				}
			}
		}
		if !ok || name == "" || name == "-" {
			continue
		}
		name = strings.Split(name, ",")[0]
		vals, present := lookup(name)
		if !present || len(vals) == 0 {
			continue
		}
		if err := setField(rv.Field(i), vals); err != nil {
			return fmt.Errorf("bind %s %q: %w", tag, name, err)
		}
	}
	return nil
}

func setField(fv reflect.Value, vals []string) error {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(vals[0])
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(vals[0], 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(vals[0], 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(vals[0], 64)
		if err != nil {
			return err
		}
		fv.SetFloat(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(vals[0])
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Slice:
		if fv.Type().Elem().Kind() == reflect.String {
			fv.Set(reflect.ValueOf(vals))
			return nil
		}
		return fmt.Errorf("unsupported slice element %s", fv.Type().Elem().Kind())
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}
