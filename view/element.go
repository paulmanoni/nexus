package view

import "encoding/json"

// Element is an element of the page, named by a CSS selector: a JS
// command's target, a value read as an event is sent, and a condition. A
// selector written in place is one: view.Show("#menu"). Build one from a
// string with El, or use This for the element the event is on.
//
//	view.Toggle("#menu")                                  // a target
//	view.Send(p.Search, view.El("#q").Value())            // read as the event is sent
//	view.If(view.El("#agree").Checked()).Push(p.Continue) // a condition
type Element string

// El is the element sel matches — every one, for a command; the first, for
// a read. Use it for a selector built at render time: view.Show(view.El("#row-" + id)).
func El(sel string) Element { return Element(sel) }

// This is the element whose event is running — JavaScript's this in its
// on… attribute (for a command pushed from the server with PushJS, the live
// view's root):
//
//	<input type="checkbox" onchange={ view.Send(p.Toggle, row.ID, view.This().Checked()) }/>
//	<button onclick={ view.JS(view.AddClass("pressed", view.This()).Push(p.Save)) }>Save</button>
//
// On a form's oninput, this is the form, not the field typed into; send a
// form's fields with view.Change.
func This() Element { return "" }

// Value is the element's value as a string, read as the event is sent: what
// an input, select or textarea holds. A number parameter takes it too. As a
// condition it holds when the value isn't empty.
func (e Element) Value() ElementRead { return ElementRead{to: string(e), read: "value"} }

// Checked is whether a checkbox or radio is checked, as a bool. As a
// condition it holds when it is.
func (e Element) Checked() ElementRead { return ElementRead{to: string(e), read: "checked"} }

// Attr is the element's attribute name as a string, null when it has none
// (a pointer parameter is then nil). A number parameter takes it too. As a
// condition it holds when the element has the attribute.
func (e Element) Attr(name string) ElementRead {
	return ElementRead{to: string(e), read: "attr", name: name}
}

// Is is a condition: the element matches the CSS selector css — ":checked",
// ":placeholder-shown", ".open", "[aria-expanded=true]".
//
//	view.If(view.This().Is(":checked")).Show("#extra").Else().Hide("#extra")
func (e Element) Is(css string) ElementIs { return ElementIs{to: string(e), is: css} }

func (e Element) condition() map[string]any {
	if e == "" {
		return map[string]any{}
	}
	return map[string]any{"to": string(e)}
}

// ElementRead is a value read from an element as the event is sent — an
// event argument of view.Send, view.SendTo or view.Push — and a condition of
// view.If; see Element.Value.
type ElementRead struct{ to, read, name string }

// MarshalJSON writes the marker the runtime replaces with the value read.
func (r ElementRead) MarshalJSON() ([]byte, error) {
	m := map[string]string{"$nx": r.read}
	if r.name != "" {
		m["name"] = r.name
	}
	if r.to != "" {
		m["to"] = r.to
	}
	return json.Marshal(m)
}

func (r ElementRead) condition() map[string]any {
	m := map[string]any{"read": r.read}
	if r.name != "" {
		m["name"] = r.name
	}
	if r.to != "" {
		m["to"] = r.to
	}
	return m
}

// Eq is the condition that the read equals value, compared as text: an
// attribute's value (one the element lacks never equals), a field's value,
// or "true"/"false" for Checked.
//
//	view.If(view.This().Attr("aria-pressed").Eq("true")).
//		SetAttr("aria-pressed", "false", view.This()).
//	Else().SetAttr("aria-pressed", "true", view.This())
func (r ElementRead) Eq(value string) ElementEq { return ElementEq{read: r, value: value} }

// ElementEq is the condition that a read equals a value; see ElementRead.Eq.
type ElementEq struct {
	read  ElementRead
	value string
}

func (c ElementEq) condition() map[string]any {
	m := c.read.condition()
	m["eq"] = c.value
	return m
}

// ElementIs is the condition that an element matches a CSS selector; see
// Element.Is.
type ElementIs struct{ to, is string }

func (c ElementIs) condition() map[string]any {
	m := map[string]any{"is": c.is}
	if c.to != "" {
		m["to"] = c.to
	}
	return m
}

// Condition is what view.If and view.ElseIf test: an Element (it holds when
// the selector matches anything), a read of one (Checked, Value, Attr), a
// read's Eq, or Element.Is.
type Condition interface{ condition() map[string]any }

// ThisElement is the type This returned.
//
// Deprecated: use Element.
type ThisElement = Element

// ThisValue is the type This().Value() returned.
//
// Deprecated: use ElementRead.
type ThisValue = ElementRead
