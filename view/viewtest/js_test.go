package viewtest_test

import (
	"context"
	"errors"
	"fmt"
	"html"
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

// Panel drives every JS command: a menu the browser opens, events that
// re-render it, and one that renders its class differently.
type Panel struct {
	view.LiveView
	Count int
	Tone  string
}

func (p *Panel) Bump(ctx context.Context) error { p.Count++; return nil }

func (p *Panel) Retone(ctx context.Context) error { p.Tone = "warm"; return nil }

func (p *Panel) Mount(ctx context.Context) error {
	if p.Connected() {
		p.PushJS(view.SetAttr("data-ready", "yes", "#count"))
	}
	return nil
}

// Save closes the menu and marks the count once its reply is applied.
func (p *Panel) Save(ctx context.Context) error {
	p.Count++
	p.PushJS(view.Hide("#menu"), view.AddClass("saved", "#count"))
	p.PushEvent("panel:saved", map[string]int{"n": p.Count})
	return nil
}

// Fail pushes, then fails: nothing it pushed runs.
func (p *Panel) Fail(ctx context.Context) error {
	p.PushJS(view.Hide("#menu"))
	return errors.New("no")
}

func (p *Panel) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Panel</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		b.WriteString(`</head><body>`)
		on := func(id, label string, s templ.ComponentScript) {
			fmt.Fprintf(&b, `<button id="%s" onclick="%s">%s</button>`, id, s.Call, label)
		}
		on("open", "open", view.JS(view.Show("#menu"), view.AddClass("is-open", "#menu"), view.SetAttr("aria-expanded", "true", "")))
		on("close", "close", view.JS(view.Hide("#menu"), view.RemoveAttr("aria-expanded", "#open")))
		on("bump", "bump", view.JS(view.Push(p.Bump)))
		on("retone", "retone", view.Send(p.Retone))
		on("save", "save", view.Send(p.Save))
		on("fail", "fail", view.Send(p.Fail))
		fmt.Fprintf(&b, `<div id="menu" class="%s" hidden><span id="count">%d</span></div>`, strings.TrimSpace("menu "+p.Tone), p.Count)
		on("toggle", "toggle", view.JS(view.Toggle("#tip")))
		b.WriteString(`<p id="tip" style="color: red">tip</p>`)
		fmt.Fprintf(&b, `<div class="row" id="row"><button id="pick" onclick="%s">pick</button></div>`,
			view.JS(view.ToggleClass("picked", ".row", view.Closest()), view.ToggleAttr("data-on", "yes", "")).Call)
		fmt.Fprintf(&b, `<div id="box" onclick="%s"><i class="dot" id="dot"></i></div>`, view.JS(view.AddClass("lit", ".dot", view.Inner())).Call)
		fmt.Fprintf(&b, `<i class="dot" id="far"></i><input id="q"><div id="form-box"><span>x</span><input id="first"></div>`)
		on("find", "find", view.JS(view.Focus("#q")))
		fade := view.Animate("fading", "faint", "solid")
		on("fade-in", "fade in", view.JS(view.Show("#fade", fade, view.Time(300*time.Millisecond))))
		on("fade-out", "fade out", view.JS(view.Hide("#fade", fade)))
		on("shake", "shake", view.JS(view.Transition("shaking", "#fade", view.Time(5*time.Second))))
		b.WriteString(`<p id="fade" hidden>fade</p>`)
		on("opener", "open dialog", view.JS(view.PushFocus(""), view.Show("#dlg"), view.FocusFirst("#dlg")))
		fmt.Fprintf(&b, `<div id="dlg" hidden data-cancel="%s"><input id="dlg-field">`, html.EscapeString(view.Commands(view.Hide("#dlg"), view.PopFocus())))
		on("dlg-close", "close dialog", view.JS(view.Exec("data-cancel", "#dlg")))
		b.WriteString(`</div>`)
		fmt.Fprintf(&b, `<span id="bumper" data-go="%s"></span>`, view.Send(p.Bump).Call)
		on("exec-send", "exec a send", view.JS(view.Exec("data-go", "#bumper")))
		on("ping", "ping", view.JS(view.Dispatch("click", "#bump")))
		on("into", "into", view.JS(view.FocusFirst("#form-box")))
		b.WriteString(`</body></html>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func TestJSCommands(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.Live[*Panel]("/panel").Provide(func() *Panel { return &Panel{} }),
	)
	p := viewtest.Mount[*Panel](t, app)

	p.Expect("#menu").Hidden()
	p.Click("#open")
	p.Expect("#menu").Visible().HasClass("is-open")
	p.Expect("#open").Attr("aria-expanded", "true")

	// A re-render keeps what the commands did.
	p.Click("#bump").Wait()
	p.Expect("#count").Text("1")
	p.Expect("#menu").Visible().HasClass("is-open")
	p.Expect("#open").Attr("aria-expanded", "true")

	// The server renders the class differently: it wins; the rest stays.
	p.Click("#retone").Wait()
	p.Expect("#menu").HasClass("warm").NoClass("is-open").Visible()

	p.Click("#close")
	p.Expect("#menu").Hidden()
	p.Expect("#open").NoAttr("aria-expanded")
	p.Click("#bump").Wait()
	p.Expect("#count").Text("2")
	p.Expect("#menu").Hidden()

	p.Click("#toggle")
	p.Expect("#tip").Hidden().Attr("style", "color: red; display: none")
	p.Click("#toggle")
	p.Expect("#tip").Visible().Attr("style", "color: red")

	p.Click("#pick")
	p.Expect("#row").HasClass("picked")
	p.Expect("#pick").Attr("data-on", "yes")
	p.Click("#pick")
	p.Expect("#row").NoClass("picked")
	p.Expect("#pick").NoAttr("data-on")

	p.Click("#box")
	p.Expect("#dot").HasClass("lit")
	p.Expect("#far").NoClass("lit")

	p.Click("#find")
	p.Expect("#q").Focused()
	p.Click("#into")
	p.Expect("#first").Focused()

	if c := p.Console(); len(c) > 0 {
		t.Errorf("console: %v", c)
	}
}

func TestJSPhase2(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.Live[*Panel]("/panel").Provide(func() *Panel { return &Panel{} }),
	)
	p := viewtest.Mount[*Panel](t, app)

	// Transitions run to their end, and leave none of their classes behind.
	p.Click("#fade-in")
	p.Expect("#fade").Visible().NoClass("fading").NoClass("faint").NoClass("solid")
	p.Click("#fade-out")
	p.Expect("#fade").Hidden().NoClass("fading")
	p.Click("#fade-in")
	p.Click("#shake") // five seconds: still running after the click settles
	p.Expect("#fade").HasClass("shaking")

	// The focus stack: a dialog returns focus to what opened it, and its
	// close button runs the steps the dialog keeps in data-cancel.
	p.Click("#opener")
	p.Expect("#dlg").Visible()
	p.Expect("#dlg-field").Focused()
	p.Click("#dlg-close")
	p.Expect("#dlg").Hidden()
	p.Expect("#opener").Focused()

	// Exec runs a live script held in an attribute; Dispatch fires an event
	// whose handler pushes.
	p.Click("#exec-send").Wait()
	p.Expect("#count").Text("1")
	p.Click("#ping").Wait()
	p.Expect("#count").Text("2")

	if c := p.Console(); len(c) > 0 {
		t.Errorf("console: %v", c)
	}
}

func TestServerPushedJS(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.Live[*Panel]("/panel").Provide(func() *Panel { return &Panel{} }),
	)
	p := viewtest.Mount[*Panel](t, app)
	p.Expect("#count").Attr("data-ready", "yes") // pushed by the connected Mount

	p.Eval(`window.saved = []; window.addEventListener("panel:saved", function (e) { window.saved.push(e.detail.n); })`)
	p.Click("#open")
	p.Expect("#menu").Visible()
	p.Click("#save").Wait()
	p.Expect("#count").Text("1").HasClass("saved")
	p.Expect("#menu").Hidden()
	if got := p.Eval(`window.saved.join(",")`); got != "1" {
		t.Errorf("panel:saved events: %q", got)
	}

	// A re-render keeps the pushed class: the server renders the count's
	// class as before.
	p.Click("#bump").Wait()
	p.Expect("#count").Text("2").HasClass("saved")

	p.Click("#open")
	p.Click("#fail").Wait()
	p.Expect("#menu").Visible()
	p.Click("#save").Wait()
	if got := p.Eval(`window.saved.join(",")`); got != "1,3" {
		t.Errorf("panel:saved events: %q", got)
	}
}
