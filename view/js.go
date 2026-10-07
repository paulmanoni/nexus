package view

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/a-h/templ"
)

// JS is a list of commands the browser runs on an event, in order, without
// a round trip unless one of them is Push — Phoenix LiveView's JS commands.
// Commands chain, each step a method of the one before:
//
//	<button onclick={ view.JS(view.Show("#confirm").Focus("#confirm-ok")) }>Delete</button>
//	<button onclick={ view.JS(view.Confirm("Delete it?").Hide("#confirm").Push(p.Delete, row.ID)) }>Yes</button>
//
// A chain is a value: extending it never changes it, so a shared start is
// safe to build on (closing := view.Hide("#m").PopFocus(); closing.Push(…)).
// Separate commands as arguments, view.JS(a, b), run the same as a.Then(b).
//
// What a command changes survives the live page's re-renders: the page
// keeps an element shown, a class added or an attribute set until the
// server renders that attribute differently, which then wins.
func JS(ops ...JSOp) templ.ComponentScript {
	b, err := json.Marshal(opList(ops))
	if err != nil {
		panic(fmt.Sprintf("view.JS: %v", err))
	}
	return templ.ComponentScript{Call: htmlAttr("__nx.js(this,event," + string(b) + ")")}
}

func opList(ops []JSOp) []any {
	list := make([]any, 0, len(ops))
	for _, op := range ops {
		for _, o := range op.steps() {
			list = append(list, []any{o.name, o.args})
		}
	}
	return list
}

// JSOp is a command of JS, or a chain of them.
type JSOp struct {
	name string
	args map[string]any
	seq  []JSOp // a chain: these commands, in order
}

// steps are the single commands o stands for.
func (o JSOp) steps() []JSOp {
	if o.seq != nil {
		return o.seq
	}
	if o.name == "" {
		return nil
	}
	return []JSOp{o}
}

// Then is o followed by next, as one chain.
func (o JSOp) Then(next ...JSOp) JSOp {
	a := o.steps()
	n := len(a)
	for _, x := range next {
		n += len(x.steps())
	}
	out := make([]JSOp, 0, n) // a fresh list: o itself never changes
	out = append(out, a...)
	for _, x := range next {
		out = append(out, x.steps()...)
	}
	return JSOp{seq: out}
}

// Each command is also a step of a chain: o.Show(sel) is o, then Show(sel).

func (o JSOp) Show(sel Target, opts ...JSOption) JSOp       { return o.Then(Show(sel, opts...)) }
func (o JSOp) Hide(sel Target, opts ...JSOption) JSOp       { return o.Then(Hide(sel, opts...)) }
func (o JSOp) Toggle(sel Target, opts ...JSOption) JSOp     { return o.Then(Toggle(sel, opts...)) }
func (o JSOp) Focus(sel Target, opts ...JSOption) JSOp      { return o.Then(Focus(sel, opts...)) }
func (o JSOp) FocusFirst(sel Target, opts ...JSOption) JSOp { return o.Then(FocusFirst(sel, opts...)) }
func (o JSOp) PushFocus(sel Target, opts ...JSOption) JSOp  { return o.Then(PushFocus(sel, opts...)) }
func (o JSOp) PopFocus() JSOp                               { return o.Then(PopFocus()) }
func (o JSOp) AddClass(classes string, sel Target, opts ...JSOption) JSOp {
	return o.Then(AddClass(classes, sel, opts...))
}
func (o JSOp) RemoveClass(classes string, sel Target, opts ...JSOption) JSOp {
	return o.Then(RemoveClass(classes, sel, opts...))
}
func (o JSOp) ToggleClass(classes string, sel Target, opts ...JSOption) JSOp {
	return o.Then(ToggleClass(classes, sel, opts...))
}
func (o JSOp) SetAttr(name, value string, sel Target, opts ...JSOption) JSOp {
	return o.Then(SetAttr(name, value, sel, opts...))
}
func (o JSOp) RemoveAttr(name string, sel Target, opts ...JSOption) JSOp {
	return o.Then(RemoveAttr(name, sel, opts...))
}
func (o JSOp) ToggleAttr(name, value string, sel Target, opts ...JSOption) JSOp {
	return o.Then(ToggleAttr(name, value, sel, opts...))
}
func (o JSOp) Transition(classes string, sel Target, opts ...JSOption) JSOp {
	return o.Then(Transition(classes, sel, opts...))
}
func (o JSOp) Exec(attr string, sel Target, opts ...JSOption) JSOp {
	return o.Then(Exec(attr, sel, opts...))
}
func (o JSOp) Dispatch(event string, sel Target, opts ...JSOption) JSOp {
	return o.Then(Dispatch(event, sel, opts...))
}
func (o JSOp) SetValue(value string, sel Target, opts ...JSOption) JSOp {
	return o.Then(SetValue(value, sel, opts...))
}
func (o JSOp) Copy(sel Target, opts ...JSOption) JSOp     { return o.Then(Copy(sel, opts...)) }
func (o JSOp) CopyText(text string) JSOp                  { return o.Then(CopyText(text)) }
func (o JSOp) Debounce(d time.Duration) JSOp              { return o.Then(Debounce(d)) }
func (o JSOp) Throttle(d time.Duration) JSOp              { return o.Then(Throttle(d)) }
func (o JSOp) ScrollTo(sel Target, opts ...JSOption) JSOp { return o.Then(ScrollTo(sel, opts...)) }
func (o JSOp) Confirm(message string) JSOp                { return o.Then(Confirm(message)) }
func (o JSOp) Push(method any, args ...any) JSOp          { return o.Then(Push(method, args...)) }
func (o JSOp) PushTo(recv any, name string, args ...any) JSOp {
	return o.Then(PushTo(recv, name, args...))
}

// JSOption narrows where a command applies or how.
type JSOption func(map[string]any)

// Closest makes a command's selector name the nearest ancestor of the
// element (itself included) that matches it, instead of matches anywhere
// on the page:
//
//	view.AddClass("is-open", ".menu", view.Closest())
func Closest() JSOption { return func(a map[string]any) { a["scope"] = "closest" } }

// Inner makes a command's selector match inside the element only.
func Inner() JSOption { return func(a map[string]any) { a["scope"] = "inner" } }

// Within makes a command's selector match inside the element's nearest
// ancestor matching container — a row's own panel, a tab strip's tabs:
//
//	view.ToggleClass("show", ".panel", view.Within(".item"))
//	view.SetAttr("aria-selected", "false", "[role=tab]", view.Within("[role=tablist]"))
//
// With view.This() as the target it is the container itself.
func Within(container string) JSOption {
	return func(a map[string]any) { a["scope"], a["within"] = "within", container }
}

// Display is the display Show gives the element ("flex", "grid"…), instead
// of what its stylesheet says.
func Display(value string) JSOption { return func(a map[string]any) { a["display"] = value } }

// Animate makes Show, Hide or Toggle a transition: during are the classes
// the element has while it runs (transition-all duration-200), from and to
// its first and last state (opacity-0 → opacity-100 to show; the reverse to
// hide). Time sets how long it runs.
//
//	view.Show("#menu", view.Animate("transition duration-200", "opacity-0", "opacity-100"))
func Animate(during, from, to string) JSOption {
	return func(a map[string]any) { a["anim"] = []string{during, from, to} }
}

// Time is how long a transition runs (200ms when unset).
func Time(d time.Duration) JSOption {
	return func(a map[string]any) { a["time"] = d.Milliseconds() }
}

// Detail is the detail Dispatch's event carries (JSON-encoded).
func Detail(v any) JSOption { return func(a map[string]any) { a["detail"] = v } }

// NoBubble keeps Dispatch's event from bubbling.
func NoBubble() JSOption { return func(a map[string]any) { a["bubbles"] = false } }

func jsOp(name string, sel Target, opts []JSOption, args map[string]any) JSOp {
	if args == nil {
		args = map[string]any{}
	}
	if to := selector(name, sel); to != "" {
		args["to"] = to
	}
	for _, o := range opts {
		o(args)
	}
	return JSOp{name: name, args: args}
}

// Target is what a command acts on: a CSS selector string, or This() — the
// element the event runs on (for a command pushed from the server with
// PushJS, the live view's root).
//
//	view.Toggle("#menu")
//	view.AddClass("is-on", view.This())
type Target = any

func selector(command string, sel Target) string {
	switch s := sel.(type) {
	case string:
		return s
	case ThisElement:
		return ""
	}
	if v := reflect.ValueOf(sel); v.Kind() == reflect.String {
		return v.String()
	}
	panic(fmt.Sprintf("view JS command %s: the target is a %T; give a CSS selector string or view.This()", command, sel))
}

// Show shows the elements sel matches:
// it drops their hidden attribute and their display: none.
func Show(sel Target, opts ...JSOption) JSOp { return jsOp("show", sel, opts, nil) }

// Hide hides the elements sel matches (display: none).
func Hide(sel Target, opts ...JSOption) JSOp { return jsOp("hide", sel, opts, nil) }

// Toggle shows the elements sel matches that are hidden and hides the rest.
func Toggle(sel Target, opts ...JSOption) JSOp { return jsOp("toggle", sel, opts, nil) }

// AddClass adds classes (space-separated) to the elements sel matches.
func AddClass(classes string, sel Target, opts ...JSOption) JSOp {
	return jsOp("add_class", sel, opts, map[string]any{"names": classes})
}

// RemoveClass removes classes from the elements sel matches.
func RemoveClass(classes string, sel Target, opts ...JSOption) JSOp {
	return jsOp("remove_class", sel, opts, map[string]any{"names": classes})
}

// ToggleClass adds each of classes the elements sel matches lack and removes
// those they have.
func ToggleClass(classes string, sel Target, opts ...JSOption) JSOp {
	return jsOp("toggle_class", sel, opts, map[string]any{"names": classes})
}

// SetAttr sets an attribute on the elements sel matches.
func SetAttr(name, value string, sel Target, opts ...JSOption) JSOp {
	return jsOp("set_attr", sel, opts, map[string]any{"name": name, "value": value})
}

// RemoveAttr removes an attribute from the elements sel matches.
func RemoveAttr(name string, sel Target, opts ...JSOption) JSOp {
	return jsOp("remove_attr", sel, opts, map[string]any{"name": name})
}

// ToggleAttr sets an attribute (to value) on the elements sel matches that
// lack it and removes it from those that have it: ToggleAttr("open", "",
// "details").
func ToggleAttr(name, value string, sel Target, opts ...JSOption) JSOp {
	return jsOp("toggle_attr", sel, opts, map[string]any{"name": name, "value": value})
}

// Focus focuses the first element sel matches.
func Focus(sel Target, opts ...JSOption) JSOp { return jsOp("focus", sel, opts, nil) }

// FocusFirst focuses the first focusable element inside the first element
// sel matches — a dialog's first field: an [autofocus] one when there is,
// and never one marked data-nx-nofocus (a dialog's close button).
func FocusFirst(sel Target, opts ...JSOption) JSOp { return jsOp("focus_first", sel, opts, nil) }

// Transition adds classes to the elements sel matches for a while (Time,
// 200ms when unset) — a shake, a flash.
func Transition(classes string, sel Target, opts ...JSOption) JSOp {
	return jsOp("transition", sel, opts, map[string]any{"names": classes})
}

// PushFocus remembers the first element sel matches (view.This(): the
// button opening a dialog) for PopFocus.
func PushFocus(sel Target, opts ...JSOption) JSOp { return jsOp("push_focus", sel, opts, nil) }

// PopFocus focuses the element PushFocus remembered last, and forgets it.
func PopFocus() JSOp { return JSOp{name: "pop_focus", args: map[string]any{}} }

// SetValue sets the value of the fields sel matches as if the user typed it: their input and change events fire,
// so a live form or view.Change hears it — clearing a search box, a preset.
func SetValue(value string, sel Target, opts ...JSOption) JSOp {
	return jsOp("set_value", sel, opts, map[string]any{"value": value})
}

// Copy puts on the clipboard the value of the field sel matches, else its
// text. The element the event is on
// carries data-copied for a moment after, for CSS to show it.
func Copy(sel Target, opts ...JSOption) JSOp { return jsOp("copy", sel, opts, nil) }

// Debounce runs the steps after it once the event has stopped firing for d:
// each new event restarts the wait, so a box typed into sends once the
// typing pauses. Steps before it run at once.
//
//	<input oninput={ view.JS(view.Debounce(300*time.Millisecond).Push(p.Search, view.This().Value())) }/>
//
// A debounced chain still waiting inside a form runs before that form's
// submit, so the submit never overtakes it.
func Debounce(d time.Duration) JSOp {
	return JSOp{name: "debounce", args: map[string]any{"ms": max(d.Milliseconds(), 0)}}
}

// Throttle runs the steps after it at most once every d: the first event
// runs them at once and further ones within d are dropped, except that a
// field's last input or change still runs them when d is up, so the final
// value is never lost.
//
//	<div onscroll={ view.JS(view.Throttle(200*time.Millisecond).Push(p.Seen)) }>
func Throttle(d time.Duration) JSOp {
	return JSOp{name: "throttle", args: map[string]any{"ms": max(d.Milliseconds(), 0)}}
}

// CopyText puts text itself on the clipboard; see Copy.
func CopyText(text string) JSOp { return JSOp{name: "copy", args: map[string]any{"text": text}} }

// ScrollTo scrolls the first element sel matches into view.
func ScrollTo(sel Target, opts ...JSOption) JSOp { return jsOp("scroll_to", sel, opts, nil) }

// Confirm asks message with the browser's confirm dialog; the commands
// after it run only when the user agrees:
//
//	view.JS(view.Confirm("Delete this user?").Push(p.Delete, u.ID))
func Confirm(message string) JSOp {
	return JSOp{name: "confirm", args: map[string]any{"message": message}}
}

// Exec runs the commands held in attribute attr of the elements sel matches
// — written with Commands, or any live script (view.Send) — as if their own
// event fired. A dialog keeps its closing steps once, in data-cancel, and its
// close button, Esc and backdrop all Exec them.
func Exec(attr string, sel Target, opts ...JSOption) JSOp {
	return jsOp("exec", sel, opts, map[string]any{"attr": attr})
}

// Commands is ops for an attribute Exec reads: data-cancel={ view.Commands(…) }.
func Commands(ops ...JSOp) string {
	b, err := json.Marshal(opList(ops))
	if err != nil {
		panic(fmt.Sprintf("view.Commands: %v", err))
	}
	return string(b)
}

// Dispatch fires a DOM CustomEvent named event on the elements sel matches,
// bubbling unless NoBubble, with Detail — for islands, scripts and widgets.
func Dispatch(event string, sel Target, opts ...JSOption) JSOp {
	return jsOp("dispatch", sel, opts, map[string]any{"event": event})
}

// Push is Send as a command: it calls method — a method value of the live
// page — on the server with args.
func Push(method any, args ...any) JSOp {
	mi := methodOf(method)
	return pushOp(mi.name, args, componentType(mi.recv))
}

// PushTo is SendTo as a command, for a live page whose type is generic.
func PushTo(recv any, name string, args ...any) JSOp {
	return pushOp(namedMethod(recv, name), args, componentType(LiveKey(reflect.TypeOf(recv))))
}

func pushOp(name string, args []any, comp string) JSOp {
	if args == nil {
		args = []any{}
	}
	a := map[string]any{"event": name, "args": args}
	if comp != "" {
		a["ct"] = comp
	}
	return JSOp{name: "push", args: a}
}
