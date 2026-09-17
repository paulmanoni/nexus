package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strconv"
	"sync"

	"github.com/paulmanoni/nexus/httpx"
)

// Form is the raw-input escape hatch for handlers whose input is genuinely
// dynamic — file uploads, variable-key forms, Django-view migrations. Declare
// it as a handler parameter (framework-filled, like *httpx.Ctx):
//
//	func NewUploadCv(svc *Svc, ctx context.Context, fm *nexus.Form) (any, error) {
//	    title := fm.Get("title")
//	    cv, err := fm.File("cv")
//	    ...
//	}
//
// Reads are SOURCE-UNIFIED: Get/All/File see the same fields whether the
// client sent multipart/form-data (what Inertia's useForm switches to when a
// file is attached), application/x-www-form-urlencoded, or a JSON body (what
// useForm sends otherwise); the URL query is the lowest-precedence fallback.
// So a working form does not break the day a file input is added to it.
//
// Prefer the typed shape — `func(ctx, args T)` / Params[T] — whenever the
// fields are known: validation tags, GraphQL schema, the typed SDK, and
// maskid unmasking all ride the args struct and none of them see fm.Get.
// Note raw reads bypass maskid: a masked id read via Get stays a string.
// Bind is the bridge back into the typed world.
//
// REST/Inertia only: on GraphQL and WS the parameter is a typed nil (every
// method no-ops), the same contract *httpx.Ctx has there.
type Form struct {
	c    *httpx.Ctx
	once sync.Once

	values   url.Values     // merged body fields (multipart / urlencoded / flattened JSON)
	jsonBody map[string]any // decoded JSON object, when the body was JSON
	rawJSON  []byte         // raw JSON bytes, kept so Bind can re-serve the body
}

func newForm(c *httpx.Ctx) *Form { return &Form{c: c} }

type formCtxKey struct{}

// FormFrom returns the request's Form from a context. Available beneath a
// handler that declares a *Form parameter (the injection wraps the request
// context); elsewhere it returns nil — check before use, or take the Form
// as a handler param and pass it down explicitly.
func FormFrom(ctx context.Context) *Form {
	if ctx == nil {
		return nil
	}
	f, _ := ctx.Value(formCtxKey{}).(*Form)
	return f
}

// parse normalizes the request body into values/jsonBody, once. Multipart
// parsing spills oversized parts to temp files (stdlib behavior) rather than
// holding them in memory; the app-level max_body_bytes cap still bounds the
// total read.
func (f *Form) parse() {
	f.once.Do(func() {
		if f.c == nil || f.c.Request == nil {
			return
		}
		r := f.c.Request
		f.values = url.Values{}
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch ct {
		case "multipart/form-data":
			_ = r.ParseMultipartForm(32 << 20)
			for k, v := range r.PostForm {
				f.values[k] = v
			}
			if r.MultipartForm != nil {
				for k, v := range r.MultipartForm.Value {
					f.values[k] = v
				}
			}
		case "application/x-www-form-urlencoded":
			_ = r.ParseForm()
			for k, v := range r.PostForm {
				f.values[k] = v
			}
		case "application/json":
			if r.Body != nil {
				body, err := io.ReadAll(r.Body)
				if err == nil && len(body) > 0 {
					f.rawJSON = body
					var m map[string]any
					if json.Unmarshal(body, &m) == nil {
						f.jsonBody = m
						flattenJSONValues(m, f.values)
					}
				}
				// Re-serve the body so a later Bind (or any other reader)
				// still sees it.
				r.Body = io.NopCloser(bytes.NewReader(f.rawJSON))
			}
		}
		// Query is the lowest-precedence fallback: only keys the body did
		// not supply.
		for k, v := range r.URL.Query() {
			if _, ok := f.values[k]; !ok {
				f.values[k] = v
			}
		}
	})
}

// flattenJSONValues folds a decoded JSON object's top level into form-style
// values: scalars stringify, arrays of scalars become repeated values.
// Nested objects/arrays stay reachable via Value.
func flattenJSONValues(m map[string]any, into url.Values) {
	for k, v := range m {
		switch t := v.(type) {
		case string:
			into[k] = []string{t}
		case float64:
			into[k] = []string{trimFloat(t)}
		case bool:
			into[k] = []string{strconv.FormatBool(t)}
		case nil:
			into[k] = []string{""}
		case []any:
			var out []string
			ok := true
			for _, e := range t {
				switch et := e.(type) {
				case string:
					out = append(out, et)
				case float64:
					out = append(out, trimFloat(et))
				case bool:
					out = append(out, strconv.FormatBool(et))
				default:
					ok = false
				}
				if !ok {
					break
				}
			}
			if ok {
				into[k] = out
			}
		}
	}
}

// trimFloat renders a JSON number the way a form would carry it: integers
// without a trailing ".0".
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Get returns the field's first value, "" when absent.
func (f *Form) Get(key string) string {
	v, _ := f.Lookup(key)
	return v
}

// Lookup distinguishes an absent field from an empty one.
func (f *Form) Lookup(key string) (string, bool) {
	if f == nil {
		return "", false
	}
	f.parse()
	vs, ok := f.values[key]
	if !ok || len(vs) == 0 {
		return "", ok
	}
	return vs[0], true
}

// All returns every value for a repeated field (or a JSON array of scalars).
func (f *Form) All(key string) []string {
	if f == nil {
		return nil
	}
	f.parse()
	return append([]string(nil), f.values[key]...)
}

// Int parses the field as an integer, returning def when absent or
// unparsable.
func (f *Form) Int(key string, def int) int {
	s, ok := f.Lookup(key)
	if !ok || s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Bool parses the field as a boolean ("true"/"1"/"on" → true), returning
// def when absent or unrecognized.
func (f *Form) Bool(key string, def bool) bool {
	s, ok := f.Lookup(key)
	if !ok || s == "" {
		return def
	}
	switch s {
	case "true", "1", "on", "yes":
		return true
	case "false", "0", "off", "no":
		return false
	}
	return def
}

// Value returns the raw decoded JSON value for a key — the door to nested
// objects and mixed arrays that Get's flat view cannot carry. Nil for
// non-JSON bodies or absent keys.
func (f *Form) Value(key string) any {
	if f == nil {
		return nil
	}
	f.parse()
	if f.jsonBody == nil {
		return nil
	}
	return f.jsonBody[key]
}

// FormFile describes one uploaded file. Open streams the part — spilled to
// a temp file by the multipart parser when large, never fully buffered by
// nexus — so GB-scale uploads cost memory only up to the parser's spill
// threshold.
type FormFile struct {
	header *multipart.FileHeader
}

func (ff *FormFile) Name() string { return ff.header.Filename }
func (ff *FormFile) Size() int64  { return ff.header.Size }
func (ff *FormFile) ContentType() string {
	return ff.header.Header.Get("Content-Type")
}
func (ff *FormFile) Open() (multipart.File, error) { return ff.header.Open() }

// File returns the first uploaded file for the key, or an error naming the
// key when the request carried no such file.
func (f *Form) File(key string) (*FormFile, error) {
	files := f.Files(key)
	if len(files) == 0 {
		return nil, fmt.Errorf("nexus: no file %q in the request", key)
	}
	return files[0], nil
}

// Files returns every uploaded file for the key — variable-count upload
// inputs are the canonical reason Form exists. Nil when none.
func (f *Form) Files(key string) []*FormFile {
	if f == nil {
		return nil
	}
	f.parse()
	if f.c == nil || f.c.Request == nil || f.c.Request.MultipartForm == nil {
		return nil
	}
	headers := f.c.Request.MultipartForm.File[key]
	out := make([]*FormFile, 0, len(headers))
	for _, h := range headers {
		out = append(out, &FormFile{header: h})
	}
	return out
}

// Bind is the bridge back into the typed world: binds the request into a
// tagged args struct exactly as reflective registration would (json / form /
// query / header / path tags), safe to call after raw reads — the JSON body
// is re-served, and multipart/urlencoded fields are already parsed.
func (f *Form) Bind(ptr any) error {
	if f == nil || f.c == nil {
		return fmt.Errorf("nexus: Form.Bind outside a REST request")
	}
	f.parse()
	if f.rawJSON != nil {
		f.c.Request.Body = io.NopCloser(bytes.NewReader(f.rawJSON))
	}
	return bindArgs(f.c, ptr)
}
