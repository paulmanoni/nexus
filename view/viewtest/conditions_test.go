package viewtest_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// Gate drives If, ElseIf and Else: conditions on the page and on the
// element, scoped ones, nested ones, a push in a branch and a Debounce in one.
type Gate struct {
	view.LiveView
	Count int
	Echo  string
}

func (g *Gate) Said(ctx context.Context, s string) error { g.Echo = s; return nil }

func (g *Gate) Bump(ctx context.Context) error { g.Count++; return nil }

func (g *Gate) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Gate</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		b.WriteString(`</head><body>`)
		on := func(id, attrs string, s templ.ComponentScript) {
			fmt.Fprintf(&b, `<button id="%s" %s onclick="%s">%s</button>`, id, attrs, s.Call, id)
		}
		me := view.This()
		b.WriteString(`<input type="checkbox" id="agree">`)
		on("go", "", view.JS(view.RemoveClass("ok no", me).If(view.El("#agree:checked")).AddClass("ok", me).Push(g.Bump).Else().AddClass("no", me)))
		fmt.Fprintf(&b, `<span id="count">%d</span>`, g.Count)

		b.WriteString(`<input id="mail" type="email" placeholder="you@example.com">`)
		on("rate", "", view.JS(view.If(view.El("#mail:placeholder-shown")).SetAttr("data-r", "empty", me).
			ElseIf(view.El("#mail:invalid")).SetAttr("data-r", "bad", me).
			Else().SetAttr("data-r", "good", me)))

		fmt.Fprintf(&b, `<input type="checkbox" id="more" onchange="%s"><p id="extra" hidden>extra</p>`,
			view.JS(view.If(me.Is(":checked")).Show("#extra").Else().Hide("#extra")).Call)

		for _, r := range []string{"a", "b"} {
			fmt.Fprintf(&b, `<div class="row" id="row-%s">`, r)
			on("del-"+r, "", view.JS(view.If(view.El(".row.sel"), view.Closest()).AddClass("gone", ".row", view.Closest())))
			b.WriteString(`</div>`)
		}
		on("mark-a", "", view.JS(view.AddClass("sel", "#row-a")))

		on("nest", `class="outer"`, view.JS(view.If(me.Is(".outer")).If(view.El("#agree:checked")).SetAttr("data-n", "both", me).
			Else().SetAttr("data-n", "outer", me)))
		on("nest-off", "", view.JS(view.If(me.Is(".outer")).If(view.El("#agree:checked")).SetAttr("data-n", "both", me).
			Else().SetAttr("data-n", "outer", me)))

		on("slow", `class="armed"`, view.JS(view.If(me.Is(".armed")).Debounce(50*time.Millisecond).AddClass("later", me).
			Else().AddClass("never", me)))

		fmt.Fprintf(&b, `<form id="f"><input name="n" required minlength="3"></form>`)
		on("check-form", "", view.JS(view.If(view.El("#f:valid")).SetAttr("data-v", "yes", me).Else().SetAttr("data-v", "no", me)))
		b.WriteString(`<ul id="list"></ul>`)
		on("check-empty", "", view.JS(view.If(view.El("#list:empty")).SetAttr("data-e", "yes", me)))
		on("readc", "", view.JS(view.If(view.El("#agree").Checked()).SetAttr("data-c", "on", me).
			ElseIf(view.El("#mail").Value()).SetAttr("data-c", "typed", me).
			ElseIf(view.El("#mail").Attr("data-flag")).SetAttr("data-c", "flag", me).
			Else().SetAttr("data-c", "none", me)))
		on("say", "", view.Send(g.Said, view.El("#mail").Value()))
		on("press", `aria-pressed="false"`, view.JS(view.If(me.Attr("aria-pressed").Eq("true")).SetAttr("aria-pressed", "false", me).
			Else().SetAttr("aria-pressed", "true", me)))
		b.WriteString(`<select id="size"><option value="s">S</option><option value="m">M</option></select>`)
		on("size-check", "", view.JS(view.If(view.El("#size").Value().Eq("m")).SetAttr("data-s", "medium", me).
			ElseIf(me.Attr("data-x").Eq("")).SetAttr("data-s", "missing-attr", me).
			ElseIf(view.El("#agree").Checked().Eq("false")).SetAttr("data-s", "unticked", me).
			Else().SetAttr("data-s", "other", me)))
		fmt.Fprintf(&b, `<span id="echo">%s</span>`, g.Echo)
		b.WriteString(`</body></html>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func TestJSConditions(t *testing.T) {
	app := nexustest.New(t, config.Runtime{}, view.Live[*Gate]("/gate"))
	p := viewtest.Mount[*Gate](t, app)

	// If / Else on another element's state; the push runs only in its branch.
	p.Click("#go")
	p.Expect("#go").HasClass("no").NoClass("ok")
	p.Expect("#count").Text("0")
	p.Check("#agree")
	p.Click("#go").Wait()
	p.Expect("#go").HasClass("ok").NoClass("no")
	p.Expect("#count").Text("1")

	// ElseIf: the first condition that holds wins.
	p.Click("#rate")
	p.Expect("#rate").Attr("data-r", "empty")
	p.Fill("#mail", "nope")
	p.Click("#rate")
	p.Expect("#rate").Attr("data-r", "bad")
	p.Fill("#mail", "a@b.co")
	p.Click("#rate")
	p.Expect("#rate").Attr("data-r", "good")

	// view.This().Is: the element's own state.
	p.Check("#more")
	p.Expect("#extra").Visible()
	p.Uncheck("#more")
	p.Expect("#extra").Hidden()

	// A condition scoped like a target: only the marked row's button acts.
	p.Click("#mark-a")
	p.Click("#del-a")
	p.Click("#del-b")
	p.Expect("#row-a").HasClass("gone")
	p.Expect("#row-b").NoClass("gone")

	// An If inside a branch takes the Else after it; when the outer If fails,
	// none of it runs.
	p.Click("#nest")
	p.Expect("#nest").Attr("data-n", "both")
	p.Uncheck("#agree")
	p.Click("#nest")
	p.Expect("#nest").Attr("data-n", "outer")
	p.Click("#nest-off")
	p.Expect("#nest-off").NoAttr("data-n")

	// A Debounce inside a branch: the steps after it wait, and the Else
	// still ends the branch when they run.
	p.Click("#slow")
	p.Expect("#slow").HasClass("later").NoClass("never")

	// :valid and :empty, as a real browser reads them.
	p.Click("#check-form")
	p.Expect("#check-form").Attr("data-v", "no")
	p.Fill(`#f [name="n"]`, "abc")
	p.Click("#check-form")
	p.Expect("#check-form").Attr("data-v", "yes")
	p.Click("#check-empty")
	p.Expect("#check-empty").Attr("data-e", "yes")

	// Reads as conditions: checked, a non-empty value, an attribute present.
	p.Check("#agree")
	p.Click("#readc")
	p.Expect("#readc").Attr("data-c", "on")
	p.Uncheck("#agree")
	p.Click("#readc")
	p.Expect("#readc").Attr("data-c", "typed")
	p.Fill("#mail", "")
	p.Click("#readc")
	p.Expect("#readc").Attr("data-c", "none")
	p.Eval(`document.getElementById("mail").setAttribute("data-flag", "")`)
	p.Click("#readc")
	p.Expect("#readc").Attr("data-c", "flag")

	// view.El reads another element as the event is sent.
	p.Fill("#mail", "sent@b.co")
	p.Click("#say").Wait()
	p.Expect("#echo").Text("sent@b.co")

	// Eq: an attribute's value, a field's value, Checked as "true"/"false";
	// an attribute the element lacks never equals, even "".
	p.Click("#press")
	p.Expect("#press").Attr("aria-pressed", "true")
	p.Click("#press")
	p.Expect("#press").Attr("aria-pressed", "false")
	p.Click("#size-check")
	p.Expect("#size-check").Attr("data-s", "unticked")
	p.Select("#size", "m")
	p.Click("#size-check")
	p.Expect("#size-check").Attr("data-s", "medium")
	p.Select("#size", "s")
	p.Check("#agree")
	p.Click("#size-check")
	p.Expect("#size-check").Attr("data-s", "other")

	if c := p.Console(); len(c) > 0 {
		t.Errorf("console: %v", c)
	}
}
