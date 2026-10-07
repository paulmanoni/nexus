package view

import "encoding/json"

// This is the element whose event is running — JavaScript's this in its
// on… attribute. It is a JS command's target, view.AddClass("on",
// view.This()), and its reads are event arguments the browser fills in as the
// event is sent, where every other argument is fixed when the page renders:
//
//	<input oninput={ view.JS(view.Debounce(300*time.Millisecond).Push(p.Search, view.This().Value())) }/>
//	<input type="checkbox" onchange={ view.Send(p.Toggle, row.ID, view.This().Checked()) }/>
//
// They go wherever event arguments go: view.Send, view.SendTo, view.Push.
// On a form's oninput, this is the form, not the field typed into; send a
// form's fields with view.Change.
func This() ThisElement { return ThisElement{} }

// ThisElement is the element an event runs on; see This.
type ThisElement struct{}

// Value is the element's value as a string: what an input, select or
// textarea holds when the event is sent. A number parameter takes it too.
func (ThisElement) Value() ThisValue { return ThisValue{read: "value"} }

// Checked is whether a checkbox or radio is checked, as a bool.
func (ThisElement) Checked() ThisValue { return ThisValue{read: "checked"} }

// Attr is the element's attribute name as a string, null when it has none
// (a pointer parameter is then nil). A number parameter takes it too.
func (ThisElement) Attr(name string) ThisValue { return ThisValue{read: "attr", name: name} }

// ThisValue is an event argument read from the element as the event is
// sent; see This.
type ThisValue struct{ read, name string }

// MarshalJSON writes the marker the runtime replaces with the read value.
func (v ThisValue) MarshalJSON() ([]byte, error) {
	m := map[string]string{"$nx": v.read}
	if v.name != "" {
		m["name"] = v.name
	}
	return json.Marshal(m)
}
