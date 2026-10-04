package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/view/internal/browser"
)

// A dropped connection reconnects with the page's resume token. When the
// server mounted afresh instead, the browser first sends its forms back —
// before anything queued — and keeps the page as it is until they are
// answered, so the fresh render doesn't close what they reopen.
func TestRuntimeRecoversFormsOnReconnect(t *testing.T) {
	var urls []string
	var recv func(string)
	var closed func()
	var sent []map[string]any
	b, err := browser.New(browser.Host{
		Dial: func(url string, r func(string), c func()) (browser.Conn, error) {
			urls, recv, closed = append(urls, url), r, c
			return recorderConn{&sent}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := func(state string, form bool) string {
		f := ""
		if form {
			f = `<form id="f" oninput="__nx.live.change(event,this,&#34;Validate&#34;)"><input id="n" name="name"></form>`
		}
		return f + `<p id="state">` + state + `</p>`
	}
	deliver := func(m map[string]any, html string) {
		t.Helper()
		if html != "" {
			doc := "<!DOCTYPE html><html><head></head><body>" + html + "</body></html>"
			m["tree"] = map[string]any{"t": 0, "s": []string{"", ""}, "d": []string{doc}}
			m["full"], m["reset"] = true, true
		}
		msg, _ := json.Marshal(m)
		recv(string(msg))
		settle(t, b)
	}
	js := func(src string) string {
		t.Helper()
		v, err := b.VM.RunString(src)
		if err != nil {
			t.Fatal(err)
		}
		return v.String()
	}
	if err := b.Load(`<!DOCTYPE html><html><head></head><body data-nx-live="/f/_live">`+body("fresh", true)+`</body></html>`, "http://app.test/f"); err != nil {
		t.Fatal(err)
	}
	if err := b.Run("runtime.js", runtimeJS); err != nil {
		t.Fatal(err)
	}
	settle(t, b)
	deliver(map[string]any{"resume": "tok1"}, "")
	js(`__dom.fill(document.querySelector("#n"), "typed")`)
	settle(t, b)
	if len(sent) != 1 || sent[0]["event"] != "Validate" {
		t.Fatalf("typing sent %v", sent)
	}

	sent = nil
	closed()
	settle(t, b)
	if len(urls) != 2 || !strings.HasSuffix(urls[1], "&resume=tok1") {
		t.Fatalf("reconnected to %v", urls)
	}
	if len(sent) != 1 || sent[0]["event"] != "Validate" || sent[0]["ref"] == nil {
		t.Fatalf("the reconnect sent %v", sent)
	}
	if f, _ := json.Marshal(sent[0]["form"]); string(f) != `{"name":["typed"]}` {
		t.Fatalf("the form went back as %s", f)
	}
	ref := sent[0]["ref"]

	// The fresh mount's render waits for the form's reply.
	deliver(map[string]any{"resume": "tok2"}, body("mounted", false))
	if got := js(`document.querySelector("#state").textContent`); got != "fresh" {
		t.Fatalf("the fresh render was applied before the form came back: %s", got)
	}
	deliver(map[string]any{"ref": ref}, body("recovered", true))
	if got := js(`document.querySelector("#state").textContent`); got != "recovered" {
		t.Fatalf("after the form's reply: %s", got)
	}
	if got := js(`__dom.value(document.querySelector("#n"))`); got != "typed" {
		t.Fatalf("the field lost what was typed: %q", got)
	}
}

type recorderConn struct{ sent *[]map[string]any }

func (c recorderConn) Send(msg string) error {
	var m map[string]any
	_ = json.Unmarshal([]byte(msg), &m)
	*c.sent = append(*c.sent, m)
	return nil
}
func (c recorderConn) Close() error { return nil }

func settle(t *testing.T, b *browser.Browser) {
	t.Helper()
	if err := b.Settle(2000, time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
}
