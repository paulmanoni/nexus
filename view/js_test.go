package view

import (
	"context"
	"encoding/json"
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
	id := "row-7"
	if got := JS(Hide(El("#" + id))).Call; got != JS(Hide("#row-7")).Call {
		t.Errorf("a selector built at render: %s", got)
	}
}

func TestJSConditionsEncoding(t *testing.T) {
	got := JS(If(El("#a:checked")).Show("#x").ElseIf(This().Is(".on"), Closest()).Hide("#x").Else().Toggle("#y")).Call
	want := `__nx.js(this,event,[[&#34;if&#34;,{&#34;to&#34;:&#34;#a:checked&#34;}],[&#34;show&#34;,{&#34;to&#34;:&#34;#x&#34;}],[&#34;elif&#34;,{&#34;is&#34;:&#34;.on&#34;,&#34;scope&#34;:&#34;closest&#34;}],[&#34;hide&#34;,{&#34;to&#34;:&#34;#x&#34;}],[&#34;else&#34;,{}],[&#34;toggle&#34;,{&#34;to&#34;:&#34;#y&#34;}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// An If inside a branch starts a new one: its Else is allowed.
	JS(If(El("#a")).If(El("#b")).Show("#x").Else().Hide("#x"))

	for name, chain := range map[string]JSOp{
		"Else without an If before it":   Show("#x").Else(),
		"ElseIf without an If before it": ElseIf(El("#a")).Show("#x"),
		"ElseIf after an Else":           If(El("#a")).Else().ElseIf(El("#b")),
		"Else after an Else":             If(El("#a")).Else().Else(),
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), name) {
					t.Errorf("%s: %v", name, r)
				}
			}()
			JS(chain)
		}()
	}
}

func TestElementReadsAndConditions(t *testing.T) {
	b, _ := json.Marshal([]any{7, This().Value(), El("#q").Value(), This().Checked(), El("#r").Attr("data-id")})
	if got, want := string(b), `[7,{"$nx":"value"},{"$nx":"value","to":"#q"},{"$nx":"checked"},{"$nx":"attr","name":"data-id","to":"#r"}]`; got != want {
		t.Errorf("reads: got %s\nwant %s", got, want)
	}
	got := JS(If(El("#agree").Checked()).ElseIf(El("#q").Value()).ElseIf(El("#m").Attr("open")).ElseIf(El("#m").Is("[hidden]")).ElseIf(This()).Else()).Call
	want := `__nx.js(this,event,[[&#34;if&#34;,{&#34;read&#34;:&#34;checked&#34;,&#34;to&#34;:&#34;#agree&#34;}],[&#34;elif&#34;,{&#34;read&#34;:&#34;value&#34;,&#34;to&#34;:&#34;#q&#34;}],[&#34;elif&#34;,{&#34;name&#34;:&#34;open&#34;,&#34;read&#34;:&#34;attr&#34;,&#34;to&#34;:&#34;#m&#34;}],[&#34;elif&#34;,{&#34;is&#34;:&#34;[hidden]&#34;,&#34;to&#34;:&#34;#m&#34;}],[&#34;elif&#34;,{}],[&#34;else&#34;,{}]])`
	if got != want {
		t.Errorf("conditions: got %s\nwant %s", got, want)
	}
}

func TestEqCondition(t *testing.T) {
	got := JS(If(This().Attr("aria-pressed").Eq("true")).ElseIf(El("#q").Value().Eq("")).ElseIf(El("#c").Checked().Eq("false"))).Call
	want := `__nx.js(this,event,[[&#34;if&#34;,{&#34;eq&#34;:&#34;true&#34;,&#34;name&#34;:&#34;aria-pressed&#34;,&#34;read&#34;:&#34;attr&#34;}],[&#34;elif&#34;,{&#34;eq&#34;:&#34;&#34;,&#34;read&#34;:&#34;value&#34;,&#34;to&#34;:&#34;#q&#34;}],[&#34;elif&#34;,{&#34;eq&#34;:&#34;false&#34;,&#34;read&#34;:&#34;checked&#34;,&#34;to&#34;:&#34;#c&#34;}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSetCookieAndReload(t *testing.T) {
	got := JS(SetCookie("lang", "sw", MaxAge(365*24*time.Hour)).Reload()).Call
	want := `__nx.js(this,event,[[&#34;set_cookie&#34;,{&#34;max_age&#34;:31536000,&#34;name&#34;:&#34;lang&#34;,&#34;value&#34;:&#34;sw&#34;}],[&#34;reload&#34;,{}]])`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := JS(SetCookie("lang", "", MaxAge(-time.Hour))).Call; !strings.Contains(got, `max_age&#34;:0`) {
		t.Errorf("a negative MaxAge deletes: %s", got)
	}
	for _, bad := range []string{"", "a=b", "a b", "a;b"} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("SetCookie(%q) took a bad name", bad)
				}
			}()
			SetCookie(bad, "v")
		}()
	}
}
