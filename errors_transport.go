package nexus

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// The per-transport renderings of the error model. Each transport maps a
// failed request through ErrorOf exactly once:
//
//	code             REST  GraphQL extensions.code   WebSocket "error" event
//	InvalidInput     422   INVALID_INPUT (+ errors)  {type, code, message, errors}
//	Unauthenticated  401   UNAUTHENTICATED           …
//	Forbidden        403   FORBIDDEN
//	NotFound         404   NOT_FOUND
//	Conflict         409   CONFLICT
//	TooMany          429   TOO_MANY_REQUESTS
//	Unavailable      503   UNAVAILABLE
//	Internal         500   INTERNAL
//
// The REST body is the *Error's JSON: {"code", "message", "errors"}.

func init() {
	middleware.ErrorBody = func(status int, err error) (int, any) {
		if status == 0 { // no status asked: the error's own
			e := ErrorOf(err)
			return e.HTTPStatus(), e
		}
		return status, rejectError(status, err)
	}
}

// rejectError maps a middleware rejection (auth gates, rate limits, CSRF,
// …) onto the model. An error with a code keeps it; otherwise the status
// the middleware asked for picks the code, and its message — the
// middleware's own — is shown, except on a 500.
func rejectError(status int, err error) *Error {
	var e *Error
	var c Code
	if errors.As(err, &e) || errors.As(err, &c) || status >= 500 && status != http.StatusServiceUnavailable {
		return ErrorOf(err)
	}
	return &Error{Code: codeForStatus(status), Message: err.Error(), Cause: err}
}

// codeForStatus is the code a bare HTTP status stands for.
func codeForStatus(status int) Code {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return InvalidInput
	case http.StatusUnauthorized:
		return Unauthenticated
	case http.StatusForbidden:
		return Forbidden
	case http.StatusNotFound:
		return NotFound
	case http.StatusConflict:
		return Conflict
	case http.StatusTooManyRequests:
		return TooMany
	case http.StatusServiceUnavailable:
		return Unavailable
	}
	if status < 500 {
		return InvalidInput
	}
	return Internal
}

// WriteError writes err as a REST error response — the status of its code
// and the {"code", "message", "errors"} body — and aborts the chain. For
// raw handlers (an *httpx.Ctx param) and extensions that answer a request
// themselves.
func WriteError(c *httpx.Ctx, err error) {
	e := ErrorOf(err)
	if e.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	c.AbortWithStatusJSON(e.HTTPStatus(), e)
}

// graphqlError is the GraphQL field error mapper: the *Error carries its
// code (and field map) into the response's extensions.
func graphqlError(err error) error {
	if err == nil {
		return nil
	}
	return ErrorOf(err)
}

// bindError reports arguments that could not be decoded as InvalidInput,
// naming the field when the decoder does.
func bindError(err error) *Error {
	e := &Error{Code: InvalidInput, Message: "invalid arguments: " + err.Error(), Cause: err}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		e.Field(te.Field, "must be a "+te.Type.String())
	}
	return e
}

// wsErrorEvent is the data of the "error" event a WebSocket handler's
// failure sends back on its connection.
func wsErrorEvent(msgType string, err error) any {
	e := ErrorOf(err)
	return struct {
		Type    string              `json:"type"`
		Code    Code                `json:"code"`
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors,omitempty"`
	}{msgType, e.Code, e.Error(), e.Fields}
}
