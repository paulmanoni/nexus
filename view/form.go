package view

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// Submit is a form's onsubmit on a live page: the browser sends the form's
// fields to method, whose last parameter is a struct bound from them with
// `form:"name"` tags — as a REST form binds.
//
//	<form onsubmit={ view.Submit(b.Add) }>
//
// If method returns nexus.Errors the page re-renders with them (view.Errors)
// and the form keeps what was typed; on success the form resets to its
// server-rendered values.
func Submit(method any) templ.ComponentScript { return formScript("submit", method) }

// Change sends the form's fields to method as they change (debounced), for
// validation as the user types — on a form's oninput or onchange:
//
//	<form onsubmit={ view.Submit(b.Add) } oninput={ view.Change(b.Validate) }>
func Change(method any) templ.ComponentScript { return formScript("change", method) }

func formScript(kind string, method any) templ.ComponentScript {
	name := methodName(method)
	return templ.ComponentScript{Call: htmlAttr("__nx.live." + kind + "(event,this," + jsonString(name) + ")")}
}

// FormErrors are the validation errors of the event that last ran, for
// Render to show beside the fields.
type FormErrors struct{ errs *nexus.Errors }

type formErrorsKey struct{}

// Errors returns the validation errors the page's last event returned (a
// nexus.Errors), empty when it succeeded.
//
//	if msg := view.Errors(ctx).Field("name"); msg != "" { <p class="error">{ msg }</p> }
func Errors(ctx context.Context) FormErrors {
	e, _ := ctx.Value(formErrorsKey{}).(*nexus.Errors)
	return FormErrors{errs: e}
}

// Field is the first message for a field, or "".
func (f FormErrors) Field(name string) string {
	if f.errs == nil {
		return ""
	}
	return f.errs.First()[name]
}

// Global is the first message not tied to a field, or "".
func (f FormErrors) Global() string { return f.Field(nexus.GlobalErrorKey) }

// Any reports whether there are errors.
func (f FormErrors) Any() bool { return f.errs.Any() }

// bindForm binds submitted form values into a new value of type t (a struct
// or a pointer to one) with nexus's own form binder.
func bindForm(t reflect.Type, values map[string][]string) (reflect.Value, error) {
	st := t
	if st.Kind() == reflect.Pointer {
		st = st.Elem()
	}
	if st.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("a form event's last parameter must be a struct, got %s", t)
	}
	body := url.Values(values).Encode()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ptr := reflect.New(st)
	if err := httpx.NewCtx(httptest.NewRecorder(), req).ShouldBind(ptr.Interface()); err != nil {
		return reflect.Value{}, err
	}
	if t.Kind() == reflect.Pointer {
		return ptr, nil
	}
	return ptr.Elem(), nil
}

// validation reports whether err is (or wraps) a nexus.Errors.
func validation(err error) (*nexus.Errors, bool) {
	var e *nexus.Errors
	if errors.As(err, &e) && e.Any() {
		return e, true
	}
	return nil, false
}
