package nexus

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/internal/graph"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// UserError is the framework's developer-facing error envelope. Fields
// produce a multi-line format with an optional `hint:` recipe and an
// optional `cause:` wrap so a developer hitting a framework error sees
// what went wrong, what to do about it, and the underlying cause in
// one block — instead of having to chase the message through several
// fmt.Errorf wraps.
//
//	nexus error [topology]: Deployment "users-svc" not in Topology.Peers
//	  declared peers: [checkout-svc orders-svc]
//	  hint: add Topology.Peers["users-svc"] in main.go's config.Runtime — URL may be empty for the active unit
//
// User code typically doesn't construct these — the framework emits
// them at known failure boundaries. They behave like normal errors
// (Error / Unwrap), so existing error-handling paths work unchanged.
type UserError struct {
	Op    string   // short verb-noun: "topology", "remote call", "path expand"
	Msg   string   // primary one-line description
	Notes []string // optional context lines (peer list, body snippet, etc.)
	Hint  string   // optional fix recipe; single line
	Cause error    // optional wrap — accessible via errors.Unwrap
}

func (e *UserError) Error() string {
	var b strings.Builder
	b.WriteString("nexus error")
	if e.Op != "" {
		b.WriteString(" [")
		b.WriteString(e.Op)
		b.WriteString("]")
	}
	b.WriteString(": ")
	b.WriteString(e.Msg)
	for _, n := range e.Notes {
		if n == "" {
			continue
		}
		b.WriteString("\n  ")
		b.WriteString(n)
	}
	if e.Cause != nil {
		b.WriteString("\n  cause: ")
		b.WriteString(e.Cause.Error())
	}
	if e.Hint != "" {
		b.WriteString("\n  hint: ")
		b.WriteString(e.Hint)
	}
	return b.String()
}

func (e *UserError) Unwrap() error { return e.Cause }

// Code classifies a request-time failure. Every transport renders an error
// through one table keyed by its code: an HTTP status and a {code, message,
// errors} body on REST, extensions.code on GraphQL, the error envelope on
// WebSocket, a flash + 303 or the error page on Inertia.
//
// A Code is itself an error, so a handler may return one bare
// (`return nil, nexus.NotFound`), and callers match with errors.Is:
//
//	if errors.Is(err, nexus.NotFound) { … }
type Code string

const (
	InvalidInput    Code = "INVALID_INPUT"     // 422 — bad arguments; carries per-field messages
	Unauthenticated Code = "UNAUTHENTICATED"   // 401
	Forbidden       Code = "FORBIDDEN"         // 403
	NotFound        Code = "NOT_FOUND"         // 404
	Conflict        Code = "CONFLICT"          // 409
	TooMany         Code = "TOO_MANY_REQUESTS" // 429
	Unavailable     Code = "UNAVAILABLE"       // 503
	Internal        Code = "INTERNAL"          // 500 — also every error without a code
)

// Error implements error with the code's default message.
func (c Code) Error() string {
	switch c {
	case InvalidInput:
		return "validation failed"
	case Unauthenticated:
		return "unauthenticated"
	case Forbidden:
		return "forbidden"
	case NotFound:
		return "not found"
	case Conflict:
		return "conflict"
	case TooMany:
		return "too many requests"
	case Unavailable:
		return "service unavailable"
	}
	return "internal error"
}

// HTTPStatus is the code's REST status.
func (c Code) HTTPStatus() int {
	switch c {
	case InvalidInput:
		return http.StatusUnprocessableEntity
	case Unauthenticated:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case TooMany:
		return http.StatusTooManyRequests
	case Unavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// GlobalErrorKey is the reserved field name Error.Global writes under. It is
// part of the wire contract: Inertia clients read page.props.errors._global,
// REST clients find it inside the 422 body's errors map, GraphQL clients in
// the error's extensions.
const GlobalErrorKey = "_global"

// Error is the one request-time error model. Handlers return it (or any
// error wrapping it); an error without one renders as Internal.
//
//	return nil, nexus.Err(nexus.NotFound, "user not found")
//	return nil, nexus.Invalid().Field("email", "already taken")
//
// errors.Is and errors.As see through Cause.
type Error struct {
	Code    Code
	Message string              // shown to the client; the code's default when empty
	Fields  map[string][]string // per-field messages (InvalidInput); global ones under GlobalErrorKey
	Cause   error
}

// Err returns an error with the given code and message.
func Err(code Code, message string) *Error { return &Error{Code: code, Message: message} }

// Errf is Err with a format; a %w verb also sets Cause.
func Errf(code Code, format string, args ...any) *Error {
	wrapped := fmt.Errorf(format, args...)
	return &Error{Code: code, Message: wrapped.Error(), Cause: errors.Unwrap(wrapped)}
}

// Invalid starts an InvalidInput error to accumulate messages into:
//
//	errs := nexus.Invalid()
//	if taken { errs.Field("email", "already taken") }
//	if down { errs.Global("payment provider unreachable") }
//	if errs.Any() { return nil, errs }
//
// Field keys should match the args struct's json/form names so the client
// binds each message to its input.
func Invalid() *Error { return &Error{Code: InvalidInput} }

// Field records a message against a field. Repeated calls accumulate.
func (e *Error) Field(field, message string) *Error {
	if e.Fields == nil {
		e.Fields = map[string][]string{}
	}
	e.Fields[field] = append(e.Fields[field], message)
	return e
}

// Global records a message not tied to any one field, under GlobalErrorKey.
func (e *Error) Global(message string) *Error { return e.Field(GlobalErrorKey, message) }

// Any reports whether any field (or global) message has been recorded — the
// guard before returning an Invalid() accumulator.
func (e *Error) Any() bool { return e != nil && len(e.Fields) > 0 }

// First returns the first message per field — the flat map[field]message
// shape Inertia's useForm expects in the errors prop.
func (e *Error) First() map[string]string {
	out := make(map[string]string, len(e.Fields))
	for k, v := range e.Fields {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// Error is the message, else the code's default (naming the failed fields
// of an InvalidInput).
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	msg := e.Code.Error()
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		msg += ": " + strings.Join(keys, ", ")
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// Is matches a Code: errors.Is(err, nexus.NotFound).
func (e *Error) Is(target error) bool {
	c, ok := target.(Code)
	return ok && c == e.Code
}

// HTTPStatus is the error's REST status.
func (e *Error) HTTPStatus() int { return e.Code.HTTPStatus() }

// MarshalJSON writes the body every JSON transport sends:
// {"code", "message", "errors"}.
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code    Code                `json:"code"`
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors,omitempty"`
	}{e.Code, e.Error(), e.Fields})
}

// Extensions implements graphql-go's ExtendedError: extensions.code, and the
// field map under extensions.errors.
func (e *Error) Extensions() map[string]any {
	ext := map[string]any{"code": string(e.Code)}
	if len(e.Fields) > 0 {
		ext["errors"] = e.Fields
	}
	return ext
}

// ErrorOf maps any error onto the model: the *Error it wraps, a bare Code,
// a GraphQL argument-validation failure as InvalidInput, and anything else
// as Internal, whose message is shown under `nexus dev` and hidden
// ("internal error") otherwise — the original stays reachable as Cause.
// nil stays nil. Every transport renders through it; an extension writing
// an error response itself should too.
func ErrorOf(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e
	}
	var c Code
	if errors.As(err, &c) {
		return &Error{Code: c, Cause: err}
	}
	var ae *graph.ArgErrors
	if errors.As(err, &ae) {
		return &Error{Code: InvalidInput, Fields: ae.Fields, Cause: err}
	}
	var rj *middleware.Rejection
	if errors.As(err, &rj) {
		return rejectError(rj.Status, rj.Err)
	}
	msg := Internal.Error()
	if dev.Enabled() {
		msg = err.Error()
	}
	return &Error{Code: Internal, Message: msg, Cause: err}
}

// CodeOf is ErrorOf(err).Code, "" for nil.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return ErrorOf(err).Code
}
