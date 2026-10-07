package view

import (
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
