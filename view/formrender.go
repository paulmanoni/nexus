package view

import (
	"context"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// AnyForm is any view.Form, whatever its struct — what the kit's ui.Form
// takes, and what a page that embeds a Form is itself.
type AnyForm interface {
	F(name string) FormField
	FieldNames() []string
	ChoicesFor(ctx context.Context, field string) []Choice
	Dirty() bool
	Valid() bool
	Submitted() bool
	Error() string
	elementInfo() formElInfo
}

type formElInfo struct {
	id, name, comp string
	gen            uint64
	hasSave        bool
}

func (f Form) elementInfo() formElInfo {
	return formElInfo{id: f.id(), name: f.name, comp: f.comp, gen: f.gen,
		hasSave: f.meta != nil && f.meta.save >= 0}
}

// FormOption tunes the form element RenderForm renders.
type FormOption interface{ formOption() }

type liveOpt struct{}
type confirmLeave struct{ msg string }
type formAttrs struct{ a templ.Attributes }

func (liveOpt) formOption()      {}
func (confirmLeave) formOption() {}
func (formAttrs) formOption()    {}

// LiveValidation makes the form check its values as the user types: each
// change goes to the form, and errors show as fields are left. Passing a
// change method instead does the same and runs the method. The kit names
// it ui.Live.
var LiveValidation FormOption = liveOpt{}

// ConfirmLeave asks before leaving the page — in-app navigation, or closing
// the tab — while the form has changes that aren't saved. message is the
// question (browsers word their own for a closing tab).
func ConfirmLeave(message ...string) FormOption {
	m := "You have unsaved changes. Leave anyway?"
	if len(message) > 0 && message[0] != "" {
		m = message[0]
	}
	return confirmLeave{m}
}

// FormAttrs adds attributes to the form element.
func FormAttrs(a templ.Attributes) FormOption { return formAttrs{a} }

// RenderForm renders a Form's form element — its fields are the children —
// wired to the page. The first method among extras is the submit (it runs
// only when every rule passes); without one, the form's own Save method
// takes it. A second method is the change method — the form is then checked
// as the user types, as with view.LiveValidation — and the other extras are
// view.ConfirmLeave, view.FormAttrs or a templ.Attributes. The kit's
// ui.Form wraps it; use it directly for markup of your own.
func RenderForm(f AnyForm, extras ...any) templ.Component {
	info := f.elementInfo()
	live, then := false, ""
	var submit any
	var leave string
	attrs := []templ.Attributes{}
	for _, e := range extras {
		switch e := e.(type) {
		case FormOption:
			switch o := e.(type) {
			case liveOpt:
				live = true
			case confirmLeave:
				leave = o.msg
			case formAttrs:
				attrs = append(attrs, o.a)
			}
		case templ.Attributes:
			attrs = append(attrs, e)
		default:
			if submit == nil {
				submit = e
				continue
			}
			if then != "" {
				panic("view.RenderForm: at most two methods — the submit, then the change method")
			}
			live, then = true, methodOf(e).name
		}
	}
	var submitScript templ.ComponentScript
	switch {
	case submit != nil:
		submitScript = Submit(submit)
	case info.hasSave:
		call := "__nx.live.submit(event,this," + jsonString("__save")
		if info.comp != "" {
			call += "," + jsonString(info.comp)
		}
		submitScript = templ.ComponentScript{Call: htmlAttr(call + ")")}
	default:
		panic("view.RenderForm: pass the page's submit method, or give the form a Save(ctx) error method")
	}
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		fmt.Fprintf(&b, `<form id="%s" data-nx-form="%s" data-nx-gen="%s" novalidate onsubmit="%s"`,
			html.EscapeString(info.id), html.EscapeString(info.name), strconv.FormatUint(info.gen, 10), submitScript.Call)
		if live {
			b.WriteString(` oninput="__nx.live.form(event,this)" onchange="__nx.live.form(event,this)"`)
		}
		if then != "" {
			fmt.Fprintf(&b, ` data-nx-then="%s"`, html.EscapeString(then))
		}
		if info.comp != "" {
			fmt.Fprintf(&b, ` data-nx-form-ct="%s"`, html.EscapeString(info.comp))
		}
		if leave != "" {
			fmt.Fprintf(&b, ` data-nx-confirm-leave="%s"`, html.EscapeString(leave))
		}
		if f.Dirty() {
			b.WriteString(` data-nx-dirty`)
		}
		for _, a := range attrs {
			if err := templ.RenderAttributes(ctx, &b, a); err != nil {
				return err
			}
		}
		b.WriteString(">")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
		if err := templ.GetChildren(ctx).Render(withActiveForm(ctx, f), w); err != nil {
			return err
		}
		_, err := io.WriteString(w, "</form>")
		return err
	})
}

type activeFormKey struct{}

func withActiveForm(ctx context.Context, f AnyForm) context.Context {
	return context.WithValue(ctx, activeFormKey{}, f)
}

// ActiveForm is the Form whose RenderForm (ui.Form) the render is inside —
// how ui.Field finds its form without being told.
func ActiveForm(ctx context.Context) (AnyForm, bool) {
	f, ok := ctx.Value(activeFormKey{}).(AnyForm)
	return f, ok
}
