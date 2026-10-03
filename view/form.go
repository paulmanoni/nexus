package view

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// Submit is a form's onsubmit on a live page: the browser sends the form's
// fields to method, whose last parameter is a struct bound from them with
// `form:"name"` tags — as a REST form binds — or url.Values, the fields as
// sent, for a page whose form isn't one struct (a generic editor binds them
// itself).
//
//	<form onsubmit={ view.Submit(b.Add) }>
//
// The struct's validate: tags are checked first. If they fail, or method
// returns an InvalidInput error (nexus.Invalid()), the page re-renders with them (view.Errors)
// and the form keeps what was typed; on success the form resets to its
// server-rendered values.
func Submit(method any) templ.ComponentScript { return formScript("submit", methodName(method)) }

// Change sends the form's fields to method as they change (debounced), for
// validation as the user types — on a form's oninput or onchange:
//
//	<form onsubmit={ view.Submit(b.Add) } oninput={ view.Change(b.Validate) }>
func Change(method any) templ.ComponentScript { return formScript("change", methodName(method)) }

// SubmitTo and ChangeTo are Submit and Change for the method named name on
// recv, for a live page whose type is generic (see SendTo).
func SubmitTo(recv any, name string) templ.ComponentScript {
	return formScript("submit", namedMethod(recv, name))
}

func ChangeTo(recv any, name string) templ.ComponentScript {
	return formScript("change", namedMethod(recv, name))
}

func formScript(kind, name string) templ.ComponentScript {
	return templ.ComponentScript{Call: htmlAttr("__nx.live." + kind + "(event,this," + jsonString(name) + ")")}
}

// Value marks a form field server-owned and gives it v as its value: when it
// hasn't focus, the field shows the server's value after every render, even
// one that didn't change it — by default a field keeps what the user typed
// until the server's value changes. Spread it on an input, a select (the
// option whose value is v is chosen) or a textarea (v is its text):
//
//	<input name="title" { view.Value(b.Title)... }/>
//	<select name="sort" { view.Value(b.Sort)... }> … </select>
//	<textarea name="notes" { view.Value(b.Notes)... }></textarea>
//
// A nil v is "".
func Value(v any) templ.Attributes {
	s := ""
	if v != nil {
		s = fmt.Sprint(v)
	}
	return templ.Attributes{"value": s, "data-nx-value": true}
}

// FormErrors are the validation errors of the event that last ran, for
// Render to show beside the fields.
type FormErrors struct{ errs *nexus.Error }

type formErrorsKey struct{}

// Errors returns the validation errors of the page's last event — its
// InvalidInput error's field map — empty when it succeeded.
//
//	if msg := view.Errors(ctx).Field("name"); msg != "" { <p class="error">{ msg }</p> }
func Errors(ctx context.Context) FormErrors {
	e, _ := ctx.Value(formErrorsKey{}).(*nexus.Error)
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
// or a pointer to one) with nexus's own form binder; url.Values takes them
// as they are.
func bindForm(t reflect.Type, values map[string][]string) (reflect.Value, error) {
	if t == reflect.TypeFor[url.Values]() || t == reflect.TypeFor[map[string][]string]() {
		if values == nil {
			values = map[string][]string{}
		}
		return reflect.ValueOf(values).Convert(t), nil
	}
	st := t
	if st.Kind() == reflect.Pointer {
		st = st.Elem()
	}
	if st.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("a form event's last parameter must be a struct or url.Values, got %s", t)
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

// validation reports whether err is an InvalidInput error with field (or
// global) messages to show beside the form.
func validation(err error) (*nexus.Error, bool) {
	if err == nil {
		return nil, false
	}
	if e := nexus.ErrorOf(err); e.Code == nexus.InvalidInput && e.Any() {
		return e, true
	}
	return nil, false
}
