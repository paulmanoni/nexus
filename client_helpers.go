package nexus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
)

// Helpers shared by LocalInvoker and RemoteCaller — the two paths
// generated cross-module clients dispatch to. Keeping them here means
// "what gets put on the wire" and "what gets put in the in-process
// httptest request" are bit-for-bit identical, so monolith and split
// shapes can't drift.

// methodHasBody reports whether the method conventionally carries a
// request body. POST/PUT/PATCH do; GET/DELETE/HEAD/OPTIONS don't —
// nexus marshals their args into the URL instead.
func methodHasBody(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH":
		return true
	}
	return false
}

// expandPath substitutes `:name` placeholders in path with the matching
// `uri:"name"` tagged field from args. Mirrors gin's URL parameter
// binding so the same args struct round-trips through both the local
// shortcut and HTTP without remapping. Untagged paths are returned
// verbatim.
//
// Multiple :name tokens are supported; missing args are an error so the
// caller never accidentally hits a literal `:id` segment.
func expandPath(path string, args any) (string, error) {
	// Fast path: no parameters to substitute.
	if !strings.Contains(path, ":") {
		return path, nil
	}
	out := path
	if args != nil {
		for name, val := range uriTagFields(args) {
			token := ":" + name
			idx := strings.Index(out, token)
			if idx < 0 {
				continue
			}
			end := idx + len(token)
			if end < len(out) && isPathTokenChar(out[end]) {
				continue
			}
			out = out[:idx] + url.PathEscape(val) + out[end:]
		}
	}
	if hasUnsubstitutedToken(out) {
		// Better to fail loud than send a garbage path. Most likely
		// cause: missing `uri:"foo"` tag on args, or args itself is
		// nil/wrong type for a path that needs parameters.
		return "", fmt.Errorf("nexus: path %q has unsubstituted parameters; check `uri:` tags on args", path)
	}
	return out, nil
}

// hasUnsubstitutedToken reports whether path still contains a `:name`
// segment. Distinguishes a real placeholder from a stray `:` (e.g. in
// a fragment or query) by requiring the next char to look like the
// start of an identifier.
func hasUnsubstitutedToken(path string) bool {
	for i := 0; i < len(path); i++ {
		if path[i] == ':' && i+1 < len(path) && isPathTokenChar(path[i+1]) {
			return true
		}
	}
	return false
}

func isPathTokenChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// uriTagFields returns name → string-value pairs for every exported
// field of args carrying a `uri:"name"` struct tag. Non-string fields
// are formatted with %v — sufficient for path parameters which are
// typically string IDs or numeric keys.
func uriTagFields(args any) map[string]string {
	out := map[string]string{}
	v := reflect.ValueOf(args)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return out
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return out
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("uri")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		out[name] = fmt.Sprintf("%v", v.Field(i).Interface())
	}
	return out
}

// encodeQuery returns the args as a URL-encoded query string for
// methods that don't carry a body. uri-tagged fields are skipped (they
// already went into the path); everything else with a `query:` or
// `form:` tag — or no tag at all — is emitted as a key/value pair.
//
// Slice values are encoded as repeated keys (?k=a&k=b), matching how
// gin's binding parses them. Empty / zero values are omitted unless
// the tag carries `,required`.
func encodeQuery(args any) (string, error) {
	if args == nil {
		return "", nil
	}
	v := reflect.ValueOf(args)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return "", nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", nil
	}
	t := v.Type()
	q := url.Values{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("uri") != "" {
			continue
		}
		key := preferredQueryKey(f)
		if key == "" || key == "-" {
			continue
		}
		fv := v.Field(i)
		// Skip zero values to keep URLs short — server side will fall
		// back to its declared default. Required fields are the
		// caller's responsibility (validation happens server-side).
		if fv.IsZero() {
			continue
		}
		switch fv.Kind() {
		case reflect.Slice, reflect.Array:
			for j := 0; j < fv.Len(); j++ {
				q.Add(key, fmt.Sprintf("%v", fv.Index(j).Interface()))
			}
		default:
			q.Set(key, fmt.Sprintf("%v", fv.Interface()))
		}
	}
	return q.Encode(), nil
}

// preferredQueryKey picks the most specific tag for query encoding.
// Priority: query > form > json > field name. Mirrors how gin chooses
// among ShouldBindQuery / ShouldBind / ShouldBindJSON binders so the
// client and server agree on the wire name.
func preferredQueryKey(f reflect.StructField) string {
	for _, tag := range []string{"query", "form", "json"} {
		if v := f.Tag.Get(tag); v != "" {
			name, _, _ := strings.Cut(v, ",")
			return name
		}
	}
	return f.Name
}

// encodeJSONBody marshals args minus uri-tagged fields. Path
// parameters are out of band — including them in the body is
// confusing if the server's args struct uses different tags for
// transport vs path.
//
// Returns nil when args is nil or contains no body-bound fields, so
// the caller can omit the body entirely (avoiding "Content-Length: 4"
// for a literal `null`).
func encodeJSONBody(args any) ([]byte, error) {
	if args == nil {
		return nil, nil
	}
	v := reflect.ValueOf(args)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		// Scalars / maps / slices pass straight through — the "args
		// is a struct" rule is a convention, not a constraint.
		return json.Marshal(args)
	}
	// Building a map[string]any (instead of zeroing fields on a
	// struct copy) is the only way to actually drop a key from the
	// JSON output: a zero-valued field still emits unless every
	// field is `omitempty`-tagged, which we can't assume.
	t := v.Type()
	body := map[string]any{}
	hasUntagged := false
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("uri") != "" {
			continue
		}
		key := preferredJSONKey(f)
		if key == "-" {
			continue
		}
		if key == "" {
			// Field has no json/form/query tag. Forward it under its
			// Go name — gin's binding does the same case-insensitive
			// match for tagless fields, so the receiver still binds.
			key = f.Name
		}
		fv := v.Field(i)
		// Honor json:",omitempty" on the field by skipping zero
		// values entirely.
		if jsonTag := f.Tag.Get("json"); strings.Contains(jsonTag, ",omitempty") && fv.IsZero() {
			continue
		}
		body[key] = fv.Interface()
		hasUntagged = true
	}
	if !hasUntagged {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// preferredJSONKey is the body-side counterpart of preferredQueryKey:
// for body encoding, the json tag wins, then form, then query. Returns
// "-" when the field is explicitly excluded ("json:\"-\""), "" when
// no tag is present.
func preferredJSONKey(f reflect.StructField) string {
	for _, tag := range []string{"json", "form", "query"} {
		if v := f.Tag.Get(tag); v != "" {
			name, _, _ := strings.Cut(v, ",")
			if name == "-" {
				return "-"
			}
			if name != "" {
				return name
			}
		}
	}
	return ""
}

// encodeRequest expands path parameters and serializes args into the
// right place — body for POST/PUT/PATCH, query string otherwise.
// Returns the final path (with query string already attached when
// applicable), the body reader (nil when there's nothing to send),
// and the content type to put on the request ("" when there's no
// body).
//
// LocalInvoker and RemoteCaller share this helper so the same args
// produce the same wire shape regardless of whether the call goes
// through httptest in-process or over real HTTP.
func encodeRequest(method, path string, args any) (finalPath string, body io.Reader, contentType string, err error) {
	finalPath, err = expandPath(path, args)
	if err != nil {
		return "", nil, "", err
	}
	if methodHasBody(method) {
		b, err := encodeJSONBody(args)
		if err != nil {
			return "", nil, "", fmt.Errorf("nexus: encode body: %w", err)
		}
		if b != nil {
			return finalPath, bytes.NewReader(b), "application/json", nil
		}
		return finalPath, nil, "", nil
	}
	qs, err := encodeQuery(args)
	if err != nil {
		return "", nil, "", fmt.Errorf("nexus: encode query: %w", err)
	}
	if qs != "" {
		finalPath += "?" + qs
	}
	return finalPath, nil, "", nil
}

// decodeResponse turns a status-code + body pair into the call's
// final return: nil when 2xx (after JSON-decoding into out, if out is
// non-nil), or a *RemoteError carrying the status + best-effort
// message when not. Shared by LocalInvoker and RemoteCaller so the
// error envelope is identical from either path.
func decodeResponse(statusCode int, respBody []byte, method, targetPath, targetURL string, out any) error {
	if statusCode >= 400 {
		re := &RemoteError{
			Status:     statusCode,
			RawBody:    respBody,
			Method:     method,
			TargetPath: targetPath,
			TargetURL:  targetURL,
		}
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &env) == nil {
			if env.Error != "" {
				re.Message = env.Error
			} else if env.Message != "" {
				re.Message = env.Message
			}
		}
		return re
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("nexus: decode response from %s: %w", targetURL, err)
	}
	return nil
}