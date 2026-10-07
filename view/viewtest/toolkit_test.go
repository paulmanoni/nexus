package viewtest_test

import (
	"context"
	"fmt"
	"html"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

var (
	slowGate  = make(chan struct{}, 4) // lets Slow finish, one release per call
	slowCalls atomic.Int32
)

// Toolkit drives the commands that need no event of their own (Confirm,
// SetValue, Copy), a data-nx-busy button, and the behaviors.
type Toolkit struct {
	view.LiveView
	Deleted int
	Query   string
	Picked  []string
	Bumps   int
	Log     []string
}

type searchForm struct {
	Q string `form:"q"`
}

type pickForm struct {
	X []string `form:"x"`
}

func (k *Toolkit) Delete(ctx context.Context) error { k.Deleted++; return nil }

func (k *Toolkit) Search(ctx context.Context, f searchForm) error { k.Query = f.Q; return nil }

func (k *Toolkit) Pick(ctx context.Context, f pickForm) error { k.Picked = f.X; return nil }

func (k *Toolkit) Bump(ctx context.Context) error { k.Bumps++; return nil }

func (k *Toolkit) Note(ctx context.Context, what string) error {
	k.Log = append(k.Log, what)
	return nil
}

func (k *Toolkit) Flag(ctx context.Context, on bool) error {
	k.Log = append(k.Log, fmt.Sprint("on=", on))
	return nil
}

func (k *Toolkit) Num(ctx context.Context, n int) error {
	k.Log = append(k.Log, fmt.Sprint("n=", n+1))
	return nil
}

func (k *Toolkit) Saved(ctx context.Context, f searchForm) error {
	k.Log = append(k.Log, "saved")
	return nil
}

type emailForm struct {
	Email string `form:"email"`
}

func (k *Toolkit) Go(ctx context.Context, f emailForm) error {
	slowCalls.Add(1)
	<-slowGate
	return nil
}

func (k *Toolkit) Slow(ctx context.Context) error {
	slowCalls.Add(1)
	<-slowGate
	return nil
}

func (k *Toolkit) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Toolkit</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		if err := view.Behaviors().Render(ctx, &b); err != nil {
			return err
		}
		attr := func(s templ.ComponentScript) string { return html.EscapeString(view.ScriptAttr(s)) }
		fmt.Fprintf(&b, `</head><body>
<button id="del" onclick="%s">Delete</button><span id="deleted">%d</span>
<form id="search" oninput="%s" onsubmit="event.preventDefault()"><input id="q" name="q"></form>
<button id="clear" onclick="%s">Clear</button><span id="query">[%s]</span>
<code id="secret">s3cret</code><button id="copy" onclick="%s">Copy</button>
<button id="slow" data-nx-busy onclick="%s">Slow</button>
<button id="bump" onclick="%s">Bump</button><span id="bumps">%d</span>
<input id="filter" data-nx-filter="#fruit">
<ul id="fruit"><li data-nx-filter-item>Apple</li><li data-nx-filter-item="Banana yellow">Banana</li><li data-nx-filter-item>Cherry</li><li data-nx-filter-empty hidden>None</li></ul>
<form id="picks" data-nx-checks onchange="%s" onsubmit="event.preventDefault()">
<input type="checkbox" id="all" data-nx-check-all><input type="checkbox" name="x" value="1"><input type="checkbox" name="x" value="2"><input type="checkbox" name="x" value="3">
<span id="count" data-nx-checked-count>0</span></form><span id="picked">%s</span>
<form id="v" data-nx-valid onsubmit="%s"><input id="email" name="email" type="email" required><button type="submit" id="go" data-nx-busy>Go</button></form>
<input id="deb" oninput="%s"><input id="thr" oninput="%s"><button id="tap" onclick="%s">Tap</button>
<form id="df" onsubmit="%s"><input id="dfq" oninput="%s"></form><span id="log">%s</span>
<input id="tv" oninput="%s"><input type="checkbox" id="tc" onchange="%s"><button id="ta" data-n="7" onclick="%s">N</button><input id="tp" value="pushed" onclick="%s"><button id="gate" onclick="%s">Gate</button><input id="cmail" type="email" placeholder="mail"><button id="cond" onclick="%s">Cond</button>
</body></html>`,
			attr(view.JS(view.Confirm("Delete it?").Push(k.Delete))), k.Deleted,
			attr(view.Change(k.Search)),
			attr(view.JS(view.SetValue("", "#q"))), html.EscapeString(k.Query),
			attr(view.JS(view.Copy("#secret"))),
			attr(view.Send(k.Slow)),
			attr(view.Send(k.Bump)), k.Bumps,
			attr(view.Change(k.Pick)), strings.Join(k.Picked, ","),
			attr(view.Submit(k.Go)),
			attr(view.JS(view.Debounce(200*time.Millisecond).Push(k.Note, "typed"))),
			attr(view.JS(view.Throttle(400*time.Millisecond).Push(k.Note, "tick"))),
			attr(view.JS(view.Throttle(time.Second).Push(k.Note, "tap"))),
			attr(view.Submit(k.Saved)), attr(view.JS(view.Debounce(5*time.Second).Push(k.Note, "early"))),
			strings.Join(k.Log, ","),
			attr(view.JS(view.Debounce(150*time.Millisecond).Push(k.Note, view.This().Value()))),
			attr(view.Send(k.Flag, view.This().Checked())),
			attr(view.Send(k.Num, view.This().Attr("data-n"))),
			attr(view.JS(view.Push(k.Note, view.This().Value()))),
			attr(view.JS(view.AddClass("pressed", view.This()).Debounce(300*time.Millisecond).Push(k.Note, "gated"))),
			attr(view.JS(view.If(view.El("#cmail:placeholder-shown")).SetAttr("data-r", "empty", view.This()).
				ElseIf(view.El("#cmail:invalid")).SetAttr("data-r", "bad", view.This()).
				Else().SetAttr("data-r", "good", view.This()))),
		)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func TestCommandsBusyAndBehaviors(t *testing.T) {
	app := nexustest.New(t, config.Runtime{}, view.Live[*Toolkit]("/toolkit"))
	p := viewtest.Browser(t, app, "/toolkit", viewtest.Timeout(20*time.Second))

	// Confirm: declined, nothing after it runs; agreed, the push goes.
	p.Eval(`window.confirm = () => false`)
	p.Click("#del")
	p.Eval(`new Promise(r => setTimeout(r, 300))`)
	p.Expect("#deleted").Text("0")
	p.Eval(`window.confirm = () => true`)
	p.Click("#del")
	p.Expect("#deleted").Text("1")

	// SetValue: as if typed, so the form's view.Change hears it.
	p.Fill("#q", "abc")
	p.Expect("#query").Text("[abc]")
	p.Click("#clear")
	p.Expect("#query").Text("[]")
	if v := p.Eval(`document.getElementById("q").value`); v != "" {
		t.Errorf("the box holds %q", v)
	}

	// Copy: the target's text goes to the clipboard; the button says so.
	p.Eval(`window.__copied = null; Object.defineProperty(navigator, "clipboard", {value: {writeText: (s) => { window.__copied = s; return Promise.resolve(); }}, configurable: true})`)
	p.Click("#copy")
	if got := p.Eval(`window.__copied`); got != "s3cret" {
		t.Errorf("copied %v", got)
	}
	if p.Eval(`document.getElementById("copy").hasAttribute("data-copied")`) != true {
		t.Error("the copy button doesn't say it copied")
	}

	// data-nx-busy: busy until its reply, a second press dropped meanwhile.
	slowCalls.Store(0)
	p.Click("#slow")
	p.Expect(`#slow[aria-busy="true"]`).Visible()
	p.Eval(`document.getElementById("slow").click()`)
	slowGate <- struct{}{}
	p.Expect(`#slow:not([aria-busy])`).Visible()
	if n := slowCalls.Load(); n != 1 {
		t.Errorf("Slow ran %d times", n)
	}

	// Filter: items hide as the box is typed in, and stay hidden through a
	// re-render; the empty note shows when nothing matches.
	p.Fill("#filter", "ban")
	p.Expect(`#fruit li:nth-child(1)`).Hidden()
	p.Expect(`#fruit li:nth-child(2)`).Visible()
	p.Click("#bump")
	p.Expect("#bumps").Text("1")
	p.Expect(`#fruit li:nth-child(1)`).Hidden()
	p.Fill("#filter", "yellow")
	p.Expect(`#fruit li:nth-child(2)`).Visible()
	p.Expect(`#fruit li:nth-child(3)`).Hidden()
	p.Fill("#filter", "zzz")
	p.Expect(`[data-nx-filter-empty]`).Visible()

	// Select all: the boxes are set before the form's change handler reads
	// them, and the count follows.
	p.Click("#all")
	p.Expect("#picked").Text("1,2,3")
	p.Expect("#count").Text("3")
	p.Click(`#picks input[value="2"]`)
	p.Expect("#picked").Text("1,3")
	if p.Eval(`document.getElementById("all").indeterminate`) != true {
		t.Error("select all doesn't show partly checked")
	}

	// Valid: the submit is enabled only while the fields pass.
	p.Expect("#go:disabled").Visible()
	p.Fill("#email", "not-an-email")
	p.Expect("#go:disabled").Visible()
	p.Fill("#email", "a@b.co")
	p.Expect("#go:not(:disabled)").Visible()

	// A data-nx-busy submit button waits for its form's reply, as the form does.
	slowCalls.Store(0)
	p.Eval(`document.getElementById("v").requestSubmit(document.getElementById("go"))`)
	p.Expect(`#go[aria-busy="true"]`).Visible()
	p.Expect(`#v[aria-busy="true"]`).Visible()
	p.Eval(`document.getElementById("go").click()`)
	slowGate <- struct{}{}
	p.Expect(`#go:not([aria-busy])`).Visible()
	p.Expect(`#v:not([aria-busy])`).Visible()
	if n := slowCalls.Load(); n != 1 {
		t.Errorf("the form was submitted %d times", n)
	}

	// Debounce: a burst of input sends once, after it pauses.
	burst := func(id string) {
		p.Eval(`(() => { const el = document.getElementById("` + id + `"); for (let i = 0; i < 5; i++) { el.value += "x"; el.dispatchEvent(new Event("input", {bubbles: true})); } })()`)
	}
	burst("deb")
	p.Expect("#log").Text("typed")
	// Throttle: the first input at once, the burst's last when the interval is up.
	burst("thr")
	p.Expect("#log").Text("typed,tick,tick")
	// Throttle on a click: extra clicks within the interval are dropped.
	p.Eval(`(() => { const b = document.getElementById("tap"); b.click(); b.click(); b.click(); })()`)
	p.Expect("#log").Text("typed,tick,tick,tap")
	p.Eval(`new Promise(r => setTimeout(r, 1100))`)
	p.Expect("#log").Text("typed,tick,tick,tap")
	p.Click("#tap")
	p.Expect("#log").Text("typed,tick,tick,tap,tap")
	// A submit runs the debounced scripts waiting in its form first.
	burst("dfq")
	p.Eval(`document.getElementById("df").requestSubmit()`)
	p.Expect("#log").Text("typed,tick,tick,tap,tap,early,saved")

	// view.This(): what the element holds as the event is sent.
	base := "typed,tick,tick,tap,tap,early,saved"
	p.Fill("#tv", "hello")
	p.Expect("#log").Text(base + ",hello")
	p.Click("#tc")
	p.Expect("#log").Text(base + ",hello,on=true")
	p.Click("#ta")
	p.Expect("#log").Text(base + ",hello,on=true,n=8")
	p.Click("#tp")
	p.Expect("#log").Text(base + ",hello,on=true,n=8,pushed")

	// Steps before a Debounce run at once; view.This() is the button.
	if p.Eval(`(() => { const b = document.getElementById("gate"); b.click(); return b.classList.contains("pressed") && !document.getElementById("log").textContent.includes("gated") })()`) != true {
		t.Error("the step before Debounce didn't run at once, or the step after it didn't wait")
	}
	p.Expect("#log").Text(base + ",hello,on=true,n=8,pushed,gated")

	// If / ElseIf / Else with the browser's own pseudo-classes.
	cond := func() any {
		return p.Eval(`(() => { const b = document.getElementById("cond"); b.click(); return b.getAttribute("data-r") })()`)
	}
	if r := cond(); r != "empty" {
		t.Errorf("empty field: %v", r)
	}
	p.Fill("#cmail", "nope")
	if r := cond(); r != "bad" {
		t.Errorf("invalid field: %v", r)
	}
	p.Fill("#cmail", "a@b.co")
	if r := cond(); r != "good" {
		t.Errorf("valid field: %v", r)
	}
}
