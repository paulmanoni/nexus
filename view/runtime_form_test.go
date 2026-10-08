package view

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/view/internal/browser"
)

// The form rules of a live page, as the runtime applies them, run against
// the test DOM (view/internal/browser), which keeps field state the way a
// browser does: a value the user typed shadows the value attribute, an
// option chosen by the user stops following its selected attribute, and
// a field the user hasn't edited follows its markup.

type livePage struct {
	t       *testing.T
	b       *browser.Browser
	recv    func(string)
	sent    []map[string]any
	console []string
}

type fakeConn struct{ p *livePage }

func (c fakeConn) Send(msg string) error {
	var m map[string]any
	_ = json.Unmarshal([]byte(msg), &m)
	c.p.sent = append(c.p.sent, m)
	return nil
}
func (c fakeConn) Close() error { return nil }

// openLive loads a live page whose body is body and connects it.
func openLive(t *testing.T, body string) *livePage {
	t.Helper()
	p := &livePage{t: t}
	b, err := browser.New(browser.Host{
		Dial: func(url string, recv func(string), closed func()) (browser.Conn, error) {
			p.recv = recv
			return fakeConn{p}, nil
		},
		Console: func(level, msg string) { p.console = append(p.console, level+": "+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.b = b
	if err := b.Load(`<!DOCTYPE html><html><head></head><body data-nx-live="/f/_live">`+body+`</body></html>`, "http://app.test/f"); err != nil {
		t.Fatal(err)
	}
	if err := b.Run("runtime.js", runtimeJS); err != nil {
		t.Fatal(err)
	}
	p.settle()
	if p.recv == nil {
		t.Fatal("the live page did not connect")
	}
	return p
}

func (p *livePage) settle() {
	p.t.Helper()
	if err := p.b.Settle(1000, time.Second, nil, nil); err != nil {
		p.t.Fatal(err)
	}
	for _, c := range p.console {
		if strings.HasPrefix(c, "error") {
			p.t.Fatalf("console %s", c)
		}
	}
}

// render delivers a server render of body, answering event ref.
func (p *livePage) render(ref int, invalid bool, body string) {
	p.t.Helper()
	html := "<!DOCTYPE html><html><head></head><body>" + body + "</body></html>"
	tree := map[string]any{"t": 0, "s": []string{"", ""}, "d": []string{html}}
	msg, _ := json.Marshal(map[string]any{"ref": ref, "invalid": invalid, "tree": tree, "full": true, "reset": true})
	p.recv(string(msg))
	p.settle()
}

func (p *livePage) js(src string) any {
	p.t.Helper()
	v, err := p.b.VM.RunString(src)
	if err != nil {
		p.t.Fatalf("%s: %v", src, err)
	}
	return v.Export()
}

func (p *livePage) value(sel string) string {
	p.t.Helper()
	return p.js(`__dom.value(document.querySelector(` + jsonString(sel) + `))`).(string)
}

// fill types into a field; blur leaves it afterwards.
func (p *livePage) fill(sel, v string, blur bool) {
	p.t.Helper()
	if bad := p.js(`__dom.fill(document.querySelector(` + jsonString(sel) + `), ` + jsonString(v) + `)`).(string); bad != "" {
		p.t.Fatal(bad)
	}
	if blur {
		p.js(`__dom.blur()`)
	}
	p.settle()
}

func (p *livePage) choose(sel string, vals ...string) {
	p.t.Helper()
	b, _ := json.Marshal(vals)
	if bad := p.js(`__dom.choose(document.querySelector(` + jsonString(sel) + `), ` + string(b) + `)`).(string); bad != "" {
		p.t.Fatal(bad)
	}
	p.js(`__dom.blur()`)
	p.settle()
}

func (p *livePage) want(sel, want string) {
	p.t.Helper()
	if got := p.value(sel); got != want {
		p.t.Fatalf("%s = %q, want %q\n%s", sel, got, want, p.js(`__dom.html(document.body)`))
	}
}

func TestFormUserValueKeptUntilServerChanges(t *testing.T) {
	p := openLive(t, `<input id="a" name="a" value="x"><p>1</p>`)
	p.fill("#a", "typed", true)
	p.render(0, false, `<input id="a" name="a" value="x"><p>2</p>`) // the server's value is unchanged
	p.want("#a", "typed")
	p.render(0, false, `<input id="a" name="a" value="y"><p>3</p>`)
	p.want("#a", "y")
}

func TestFormFocusedFieldNeverOverwritten(t *testing.T) {
	p := openLive(t, `<input id="a" value="x"><input id="b" value="1">`)
	p.fill("#a", "typing", false) // still focused
	p.render(0, false, `<input id="a" value="z"><input id="b" value="1">`)
	p.want("#a", "typing")

	// A field the user focused but has not edited would follow its value
	// attribute in a browser: the runtime keeps what it showed.
	p.js(`document.querySelector("#b").focus()`)
	p.render(0, false, `<input id="a" value="z"><input id="b" value="2">`)
	p.want("#b", "1")
	p.js(`__dom.blur()`)
	p.render(0, false, `<input id="a" value="z"><input id="b" value="3">`)
	p.want("#b", "3")
	p.want("#a", "typing") // its server value hasn't changed since

	// A focused checkbox keeps its state too.
	q := openLive(t, `<input id="c" type="checkbox">`)
	q.js(`document.querySelector("#c").focus()`)
	q.render(0, false, `<input id="c" type="checkbox" checked>`)
	if q.js(`document.querySelector("#c").checked`) != false {
		t.Fatal("a focused checkbox was checked by the server")
	}
	q.js(`__dom.blur()`)
	q.render(0, false, `<input id="c" type="checkbox">`)
	if q.js(`document.querySelector("#c").checked`) != false {
		t.Fatal("checkbox")
	}
	q.render(0, false, `<input id="c" type="checkbox" checked>`)
	if q.js(`document.querySelector("#c").checked`) != true {
		t.Fatal("an unfocused checkbox follows the server's checked attribute")
	}
}

func TestFormSelectFollowsServerChoice(t *testing.T) {
	opts := func(sel string) string {
		var b strings.Builder
		for _, v := range []string{"a", "b", "c"} {
			b.WriteString(`<option value="` + v + `"`)
			if v == sel {
				b.WriteString(` selected`)
			}
			b.WriteString(`>` + v + `</option>`)
		}
		return `<select id="s" name="s">` + b.String() + `</select>`
	}
	p := openLive(t, opts("b"))
	p.want("#s", "b")
	p.choose("#s", "c")
	p.render(0, false, opts("b")+`<p>x</p>`) // the server's choice is unchanged
	p.want("#s", "c")
	p.render(0, false, opts("a"))
	p.want("#s", "a")

	// Focused: the user's choice holds even when the server's changes.
	p.js(`document.querySelector("#s").focus()`)
	p.render(0, false, opts("b"))
	p.want("#s", "a")
	p.js(`__dom.blur()`)
	p.render(0, false, opts("c"))
	p.want("#s", "c")

	// No option marked: the first.
	p.render(0, false, opts(""))
	p.want("#s", "a")
}

func TestFormTextareaFollowsServerValue(t *testing.T) {
	p := openLive(t, `<textarea id="t" name="t">one</textarea>`)
	p.want("#t", "one")
	p.fill("#t", "mine", true)
	p.render(0, false, `<textarea id="t" name="t">one</textarea><p>x</p>`)
	p.want("#t", "mine")
	p.render(0, false, `<textarea id="t" name="t">two</textarea>`)
	p.want("#t", "two")
	p.fill("#t", "typing", false)
	p.render(0, false, `<textarea id="t" name="t">three</textarea>`)
	p.want("#t", "typing")
	p.js(`__dom.blur()`)
	p.render(0, false, `<textarea id="t" name="t">four</textarea>`)
	p.want("#t", "four")
}

func TestFormResetsAfterSuccessfulSubmit(t *testing.T) {
	form := func(v, errMsg string) string {
		return `<form id="f" onsubmit="__nx.live.submit(event,this,&#34;Add&#34;)">` +
			`<input id="n" name="name" value="` + v + `"><textarea id="d" name="desc"></textarea><p id="err">` + errMsg + `</p>` +
			`<button id="go" type="submit">Add</button></form>`
	}
	p := openLive(t, form("", ""))
	p.fill("#n", "Rex", false)
	p.fill("#d", "a dog", false)
	p.js(`document.querySelector("#go").focus(); document.querySelector("#go").click()`)
	p.settle()
	if len(p.sent) != 1 || p.sent[0]["event"] != "Add" {
		t.Fatalf("sent %v", p.sent)
	}
	fields, _ := p.sent[0]["form"].(map[string]any)
	if got, _ := json.Marshal(fields); string(got) != `{"desc":["a dog"],"name":["Rex"]}` {
		t.Fatalf("form fields %s", got)
	}
	ref := int(p.sent[0]["ref"].(float64))

	// Invalid: the form keeps what was typed.
	p.render(ref, true, form("", "taken"))
	p.want("#n", "Rex")
	p.want("#d", "a dog")

	p.js(`document.querySelector("#go").click()`)
	p.settle()
	ref = int(p.sent[1]["ref"].(float64))
	p.render(ref, false, form("", ""))
	p.want("#n", "")
	p.want("#d", "")
}

func TestFormValueMarksServerOwned(t *testing.T) {
	p := openLive(t, `<input id="a" value="srv" data-nx-value>`+
		`<select id="s" value="b" data-nx-value><option value="a">a</option><option value="b">b</option></select>`+
		`<textarea id="t" value="text" data-nx-value></textarea>`)
	p.want("#s", "b") // a select reads view.Value's value attribute
	p.want("#t", "text")
	p.fill("#a", "mine", true)
	p.choose("#s", "a")
	p.fill("#t", "mine", true)
	same := `<input id="a" value="srv" data-nx-value>` +
		`<select id="s" value="b" data-nx-value><option value="a">a</option><option value="b">b</option></select>` +
		`<textarea id="t" value="text" data-nx-value></textarea>`
	p.render(0, false, same) // unchanged, yet the fields take the server's value
	p.want("#a", "srv")
	p.want("#s", "b")
	p.want("#t", "text")

	p.fill("#a", "typing", false) // focus still wins
	p.render(0, false, same)
	p.want("#a", "typing")
}

func TestValueAttributes(t *testing.T) {
	var buf bytes.Buffer
	if err := templ.RenderAttributes(context.Background(), &buf, Value(42)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != ` data-nx-value value="42"` {
		t.Fatalf("view.Value(42) renders %q", got)
	}
	if v := Value(nil)["value"]; v != "" {
		t.Fatalf("view.Value(nil) value = %v", v)
	}
}

// events lists the events the page sent, leaving out the connection's own.
func (p *livePage) events() []string {
	var out []string
	for _, m := range p.sent {
		if e, _ := m["event"].(string); e != "" && e != "__join" && e != "__resync" {
			out = append(out, e)
		}
	}
	return out
}

// A form's change waits for typing to pause; an event sent meanwhile goes
// after it, so the server sees what the user did in order — a click right
// after typing is neither overtaken by the typing nor undone by it.
func TestPendingFormChangeSentBeforeOtherEvents(t *testing.T) {
	for name, tc := range map[string]struct{ form, change string }{
		"view.Form":   {`<form id="f" data-nx-form="draft" data-nx-then="Changed" oninput="__nx.live.form(event,this)" onsubmit="__nx.live.submit(event,this,&#34;Next&#34;)">`, "__form"},
		"view.Change": {`<form id="f" oninput="__nx.live.change(event,this,&#34;Validate&#34;)" onsubmit="__nx.live.submit(event,this,&#34;Next&#34;)">`, "Validate"},
	} {
		t.Run(name, func(t *testing.T) {
			p := openLive(t, tc.form+`<input id="a" name="a"><button id="next" type="submit">Next</button></form>`+
				`<button id="add" onclick="__nx.live.send(this,&#34;AddChoice&#34;)">Add</button>`)
			p.js(`__dom.fill(document.querySelector("#a"), "typed"); document.querySelector("#add").click()`)
			p.settle()
			if got := strings.Join(p.events(), ","); got != tc.change+",AddChoice" {
				t.Fatalf("sent %s, want the change first and once", got)
			}

			p.sent = nil
			p.js(`__dom.fill(document.querySelector("#a"), "more"); document.querySelector("#next").click()`)
			p.settle()
			if got := strings.Join(p.events(), ","); got != tc.change+",Next" {
				t.Fatalf("sent %s, want the change before the submit and none after", got)
			}
		})
	}
}
