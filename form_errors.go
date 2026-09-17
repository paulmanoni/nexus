package nexus

import (
	"sort"
	"strings"
)

// GlobalErrorKey is the reserved field name Errors.Global writes under. It is
// part of the wire contract: Inertia clients read page.props.errors._global,
// REST clients find it inside the 422 body's errors map, GraphQL clients in
// the error's extensions. A reserved key (rather than a separate channel)
// keeps global messages on the same object a form is already watching.
const GlobalErrorKey = "_global"

// Errors accumulates validation failures — per-field and global — from a
// handler or service, and renders per transport when returned as the
// handler's error:
//
//	errs := nexus.NewErrors()
//	if taken { errs.Field("email", "already taken") }
//	if provider.Down() { errs.Global("payment provider unreachable") }
//	if errs.Any() {
//	    return nil, errs
//	}
//
// Inertia pages: flashed and 303-redirected back, surfacing in the next
// render's `errors` prop (the useForm convention; error bags honored).
// REST: 422 {"message": ..., "errors": {field: [msgs]}}. GraphQL: a normal
// GraphQL error carrying the field map in extensions. Field keys should
// match the args struct's json tags so useForm binds messages to inputs.
type Errors struct {
	fields map[string][]string
	order  []string
}

// NewErrors returns an empty accumulator. The zero value is not usable —
// always construct through here.
func NewErrors() *Errors {
	return &Errors{fields: map[string][]string{}}
}

// Field records a message against a field. Repeated calls accumulate.
// Returns the receiver for chaining.
func (e *Errors) Field(field, message string) *Errors {
	if _, seen := e.fields[field]; !seen {
		e.order = append(e.order, field)
	}
	e.fields[field] = append(e.fields[field], message)
	return e
}

// Global records a message not tied to any one field, under GlobalErrorKey.
func (e *Errors) Global(message string) *Errors {
	return e.Field(GlobalErrorKey, message)
}

// Any reports whether anything has been recorded — the standard guard
// before returning the accumulator.
func (e *Errors) Any() bool { return e != nil && len(e.fields) > 0 }

// FieldErrors returns a copy of the accumulated messages, every message
// kept, global included under GlobalErrorKey. The REST 422 body shape.
func (e *Errors) FieldErrors() map[string][]string {
	out := make(map[string][]string, len(e.fields))
	for k, v := range e.fields {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// First returns the first message per field — the flat map[field]message
// shape Inertia's useForm expects in the errors prop.
func (e *Errors) First() map[string]string {
	out := make(map[string]string, len(e.fields))
	for k, v := range e.fields {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// Error implements error: a stable, sorted summary naming the failed fields.
func (e *Errors) Error() string {
	keys := make([]string, 0, len(e.fields))
	for k := range e.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "nexus: validation failed: " + strings.Join(keys, ", ")
}

// Extensions implements graphql-go's ExtendedError, so a GraphQL resolver
// returning *Errors ships the field map inside the error entry's extensions.
func (e *Errors) Extensions() map[string]interface{} {
	return map[string]interface{}{
		"code":   "VALIDATION",
		"errors": e.FieldErrors(),
	}
}
