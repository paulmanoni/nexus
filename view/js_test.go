package view

import (
	"context"
	"fmt"
	"html"
	"strings"
	"testing"
	"time"
)

func TestJSEncoding(t *testing.T) {
	got := html.UnescapeString(JS(
		Show("#menu", Display("flex")),
		AddClass("a b", ".row", Closest()),
		SetAttr("aria-expanded", "true", ""),
		FocusFirst("#dlg", Inner()),
	).Call)
	want := `__nx.js(this,event,[["show",{"display":"flex","to":"#menu"}],["add_class",{"names":"a b","scope":"closest","to":".row"}],["set_attr",{"name":"aria-expanded","value":"true"}],["focus_first",{"scope":"inner","to":"#dlg"}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if call := JS(Show("#x")).Call; strings.Contains(call, `"`) {
		t.Errorf("not escaped for an attribute: %s", call)
	}
}

func TestJSPhase2Encoding(t *testing.T) {
	got := html.UnescapeString(JS(
		Hide("#m", Animate("t", "a", "b"), Time(150*time.Millisecond)),
		Dispatch("picked", "", Detail(map[string]int{"n": 1}), NoBubble()),
		PushFocus(""), PopFocus(), Exec("data-cancel", "#dlg"), Transition("shake", "#m"),
	).Call)
	want := `__nx.js(this,event,[["hide",{"anim":["t","a","b"],"time":150,"to":"#m"}],["dispatch",{"bubbles":false,"detail":{"n":1},"event":"picked"}],["push_focus",{}],["pop_focus",{}],["exec",{"attr":"data-cancel","to":"#dlg"}],["transition",{"names":"shake","to":"#m"}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if c := Commands(Hide("#dlg"), PopFocus()); c != `[["hide",{"to":"#dlg"}],["pop_focus",{}]]` {
		t.Errorf("Commands: %s", c)
	}
}

// A chain encodes as the same commands given one by one, and extending a
// chain never changes it: a shared start is safe to build on.
func TestJSChaining(t *testing.T) {
	chained := JS(Show("#menu", Display("flex")).AddClass("a b", ".row", Closest()).SetAttr("aria-expanded", "true", "").FocusFirst("#dlg", Inner())).Call
	listed := JS(Show("#menu", Display("flex")), AddClass("a b", ".row", Closest()), SetAttr("aria-expanded", "true", ""), FocusFirst("#dlg", Inner())).Call
	if chained != listed {
		t.Errorf("chained %s\nlisted  %s", chained, listed)
	}

	closing := Hide("#dlg").PopFocus()
	ok := closing.PushTo(&chainPage{}, "Delete", 7)
	cancel := closing.Dispatch("closed", "")
	if c := Commands(closing); c != `[["hide",{"to":"#dlg"}],["pop_focus",{}]]` {
		t.Errorf("extending changed the start: %s", c)
	}
	if c := Commands(cancel); c != `[["hide",{"to":"#dlg"}],["pop_focus",{}],["dispatch",{"event":"closed"}]]` {
		t.Errorf("cancel: %s", c)
	}
	if !strings.Contains(Commands(ok), `"push"`) || strings.Contains(Commands(ok), "dispatch") {
		t.Errorf("one extension leaked into the other: %s", Commands(ok))
	}

	if c := Commands(Show("#a").Then(Hide("#b").Focus("#c"), PopFocus())); c != `[["show",{"to":"#a"}],["hide",{"to":"#b"}],["focus",{"to":"#c"}],["pop_focus",{}]]` {
		t.Errorf("Then: %s", c)
	}
	// A list of commands, as code builds one, still spreads.
	ops := []JSOp{Show("#a"), Hide("#b").Focus("#c")}
	if c := Commands(ops...); c != `[["show",{"to":"#a"}],["hide",{"to":"#b"}],["focus",{"to":"#c"}]]` {
		t.Errorf("spread: %s", c)
	}
}

func TestJSPhase5Encoding(t *testing.T) {
	got := Commands(Confirm("Sure?").SetValue("", "#q").Copy("#pw").CopyText("hi").ScrollTo("#row-7"))
	want := `[["confirm",{"message":"Sure?"}],["set_value",{"to":"#q","value":""}],["copy",{"to":"#pw"}],["copy",{"text":"hi"}],["scroll_to",{"to":"#row-7"}]]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

type chainPage struct{ LiveView }

func (*chainPage) Delete(ctx context.Context, id int) error { return nil }

func TestJSTimingAndThis(t *testing.T) {
	got := JS(AddClass("on", This()).Debounce(300 * time.Millisecond).Throttle(-time.Second).Toggle("#m")).Call
	want := `__nx.js(this,event,[[&#34;add_class&#34;,{&#34;names&#34;:&#34;on&#34;}],[&#34;debounce&#34;,{&#34;ms&#34;:300}],[&#34;throttle&#34;,{&#34;ms&#34;:0}],[&#34;toggle&#34;,{&#34;to&#34;:&#34;#m&#34;}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	type sel string
	if got := JS(Hide(sel(".x"))).Call; got != JS(Hide(".x")).Call {
		t.Errorf("a named string type: %s", got)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "view.This()") {
			t.Errorf("a target that is no selector: %v", r)
		}
	}()
	Show(42)
}
