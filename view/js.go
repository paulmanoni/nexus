package view

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/a-h/templ"
)

// JS is a list of commands the browser runs on an event, in order, without
// a round trip unless one of them is Push — Phoenix LiveView's JS commands:
//
//	<button onclick={ view.JS(view.Show("#confirm"), view.Focus("#confirm-ok")) }>Delete</button>
//	<button onclick={ view.JS(view.Hide("#confirm"), view.Push(p.Delete, row.ID)) }>Yes</button>
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
		list = append(list, []any{op.name, op.args})
	}
	return list
}

// JSOp is one command of JS.
type JSOp struct {
	name string
	args map[string]any
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
// With selector "" it is the container itself.
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

func jsOp(name, sel string, opts []JSOption, args map[string]any) JSOp {
	if args == nil {
		args = map[string]any{}
	}
	if sel != "" {
		args["to"] = sel
	}
	for _, o := range opts {
		o(args)
	}
	return JSOp{name: name, args: args}
}

// Show shows the elements sel matches ("" is the element the event is on):
// it drops their hidden attribute and their display: none.
func Show(sel string, opts ...JSOption) JSOp { return jsOp("show", sel, opts, nil) }

// Hide hides the elements sel matches (display: none).
func Hide(sel string, opts ...JSOption) JSOp { return jsOp("hide", sel, opts, nil) }

// Toggle shows the elements sel matches that are hidden and hides the rest.
func Toggle(sel string, opts ...JSOption) JSOp { return jsOp("toggle", sel, opts, nil) }

// AddClass adds classes (space-separated) to the elements sel matches.
func AddClass(classes, sel string, opts ...JSOption) JSOp {
	return jsOp("add_class", sel, opts, map[string]any{"names": classes})
}

// RemoveClass removes classes from the elements sel matches.
func RemoveClass(classes, sel string, opts ...JSOption) JSOp {
	return jsOp("remove_class", sel, opts, map[string]any{"names": classes})
}

// ToggleClass adds each of classes the elements sel matches lack and removes
// those they have.
func ToggleClass(classes, sel string, opts ...JSOption) JSOp {
	return jsOp("toggle_class", sel, opts, map[string]any{"names": classes})
}

// SetAttr sets an attribute on the elements sel matches.
func SetAttr(name, value, sel string, opts ...JSOption) JSOp {
	return jsOp("set_attr", sel, opts, map[string]any{"name": name, "value": value})
}

// RemoveAttr removes an attribute from the elements sel matches.
func RemoveAttr(name, sel string, opts ...JSOption) JSOp {
	return jsOp("remove_attr", sel, opts, map[string]any{"name": name})
}

// ToggleAttr sets an attribute (to value) on the elements sel matches that
// lack it and removes it from those that have it: ToggleAttr("open", "",
// "details").
func ToggleAttr(name, value, sel string, opts ...JSOption) JSOp {
	return jsOp("toggle_attr", sel, opts, map[string]any{"name": name, "value": value})
}

// Focus focuses the first element sel matches.
func Focus(sel string, opts ...JSOption) JSOp { return jsOp("focus", sel, opts, nil) }

// FocusFirst focuses the first focusable element inside the first element
// sel matches — a dialog's first field: an [autofocus] one when there is,
// and never one marked data-nx-nofocus (a dialog's close button).
func FocusFirst(sel string, opts ...JSOption) JSOp { return jsOp("focus_first", sel, opts, nil) }

// Transition adds classes to the elements sel matches for a while (Time,
// 200ms when unset) — a shake, a flash.
func Transition(classes, sel string, opts ...JSOption) JSOp {
	return jsOp("transition", sel, opts, map[string]any{"names": classes})
}

// PushFocus remembers the first element sel matches ("" is the element the
// event is on — the button opening a dialog) for PopFocus.
func PushFocus(sel string, opts ...JSOption) JSOp { return jsOp("push_focus", sel, opts, nil) }

// PopFocus focuses the element PushFocus remembered last, and forgets it.
func PopFocus() JSOp { return JSOp{name: "pop_focus", args: map[string]any{}} }

// Exec runs the commands held in attribute attr of the elements sel matches
// — written with Commands, or any live script (view.Send) — as if their own
// event fired. A dialog keeps its closing steps once, in data-cancel, and its
// close button, Esc and backdrop all Exec them.
func Exec(attr, sel string, opts ...JSOption) JSOp {
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
func Dispatch(event, sel string, opts ...JSOption) JSOp {
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
