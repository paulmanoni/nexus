// Package canonical emits RFC 8785-style canonical JSON so two
// servers producing the same value tree produce byte-identical
// signing inputs. Without this, an Ed25519 signature over
// encoding/json's output is unstable across Go versions (and
// across map-iteration orders within one version), and clients
// reject perfectly-valid snapshots.
//
// Implements a subset of RFC 8785 sufficient for the config
// plugin's snapshot shape — string/number/bool/null/array/object
// with sorted object keys. No JSON-Schema-canonicalization
// number-rewriting (we control producer + consumer, both use
// the same encoder), no number normalization beyond what
// encoding/json already does for floats.
package canonical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Marshal returns v as canonical JSON: object keys lexicographically
// sorted, no insignificant whitespace, strings JSON-escaped via the
// stdlib's HTML-safe rules disabled (so '<', '>', '&' stay literal
// and verifiers across languages agree byte-for-byte).
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := emit(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func emit(w *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		w.WriteString("null")
		return nil
	case bool:
		if x {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
		return nil
	case string:
		return emitString(w, x)
	case json.Number:
		w.WriteString(string(x))
		return nil
	case float64:
		return emitNumber(w, x)
	case float32:
		return emitNumber(w, float64(x))
	case int:
		w.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int64:
		w.WriteString(strconv.FormatInt(x, 10))
		return nil
	case int32:
		w.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case uint:
		w.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case uint64:
		w.WriteString(strconv.FormatUint(x, 10))
		return nil
	case uint32:
		w.WriteString(strconv.FormatUint(uint64(x), 10))
		return nil
	case []any:
		return emitArray(w, x)
	case map[string]any:
		return emitObject(w, x)
	}
	// Fallback: re-decode through encoding/json so callers can hand
	// us typed structs. This walks the value through the std
	// encoder/decoder once to land it in the generic any/map shape
	// above. Slow path; the hot path is map[string]any all the way
	// down.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve int vs float distinction across re-decode
	if err := dec.Decode(&generic); err != nil {
		return err
	}
	return emit(w, generic)
}

func emitArray(w *bytes.Buffer, a []any) error {
	w.WriteByte('[')
	for i, el := range a {
		if i > 0 {
			w.WriteByte(',')
		}
		if err := emit(w, el); err != nil {
			return err
		}
	}
	w.WriteByte(']')
	return nil
}

// emitObject is the load-bearing piece: keys MUST sort
// lexicographically before emit. Two servers that disagree on map
// iteration order — which Go's runtime randomizes per process —
// would otherwise produce different signing inputs for the same
// logical value tree.
func emitObject(w *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	w.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			w.WriteByte(',')
		}
		if err := emitString(w, k); err != nil {
			return err
		}
		w.WriteByte(':')
		if err := emit(w, m[k]); err != nil {
			return err
		}
	}
	w.WriteByte('}')
	return nil
}

// emitString writes s as a JSON string with the stdlib's standard
// escapes EXCEPT we don't \u-escape <, >, &. The std encoder's
// "HTML-safe" mode escapes them to avoid embedding-in-script-tag
// hazards; we're emitting bytes for signing, not for an HTML
// document. Disabling the rewrites keeps our output aligned with
// every other canonical-JSON producer (Python rfc8785, JS
// json-canonicalize, etc.).
func emitString(w *bytes.Buffer, s string) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	// Encode() appends a trailing \n; strip it.
	b := w.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		w.Truncate(len(b) - 1)
	}
	return nil
}

// emitNumber handles floats. NaN/Inf are not valid JSON; reject
// rather than emit non-JSON. The float encoder uses 'g' precision
// to match encoding/json's behavior for the common case (small
// integers serialize as integers; floats use minimum required
// digits).
func emitNumber(w *bytes.Buffer, f float64) error {
	if f != f { // NaN
		return errors.New("canonical: NaN is not valid JSON")
	}
	if f > 1e308 || f < -1e308 {
		return fmt.Errorf("canonical: %v is not valid JSON (Inf)", f)
	}
	// Mirror encoding/json's number formatting so numeric literals
	// in a value tree match what the std encoder would have produced.
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	w.Write(raw)
	return nil
}
