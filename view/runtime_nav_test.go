package view

import (
	"encoding/json"
	"testing"

	"github.com/paulmanoni/nexus/v2/view/internal/browser"
)

// A link on a live page moves over its socket: a patch keeps the page and
// pushes the URL; another live page takes the root (its socket, its title);
// back sends the earlier URL the same way; a redirect loads the page.
func TestRuntimeLiveNavigation(t *testing.T) {
	var urls []string
	var recv func(string)
	var sent []map[string]any
	var loaded []string
	b, err := browser.New(browser.Host{
		Dial: func(url string, r func(string), _ func()) (browser.Conn, error) {
			urls, recv = append(urls, url), r
			return recorderConn{&sent}, nil
		},
		Navigate: func(method, url, body, contentType string) { loaded = append(loaded, url) },
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := func(title, body string) string {
		return "<!DOCTYPE html><html><head><title>" + title + "</title></head><body>" + body + "</body></html>"
	}
	deliver := func(m map[string]any, html string) {
		t.Helper()
		if html != "" {
			m["tree"] = map[string]any{"t": 0, "s": []string{"", ""}, "d": []string{html}}
			m["full"] = true
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
	links := `<a id="p2" href="/list?page=2" data-nx-nav>2</a><a id="other" href="/count/bo" data-nx-nav>bo</a><a id="out" href="/static" data-nx-nav>out</a>`
	if err := b.Load(`<!DOCTYPE html><html><head><title>List</title></head><body data-nx-live="/list/_live">`+links+`<p id="page">1</p></body></html>`, "http://app.test/list"); err != nil {
		t.Fatal(err)
	}
	if err := b.Run("runtime.js", runtimeJS); err != nil {
		t.Fatal(err)
	}
	settle(t, b)
	deliver(map[string]any{"resume": "tok"}, "")
	// Joined over HTTP, the page asks for its tree when quiet, and keeps
	// itself as it is when the tree comes.
	if len(sent) != 1 || sent[0]["event"] != "__resync" {
		t.Fatalf("priming sent %v", sent)
	}
	deliver(map[string]any{"ref": sent[0]["ref"], "reset": true}, doc("List", links+`<p id="page">primed</p>`))
	if got := js(`document.querySelector("#page").textContent`); got != "1" {
		t.Fatalf("the prime patched the page: %s", got)
	}
	sent = nil

	js(`window.__moves = 0; window.addEventListener("nx:navigate", function () { window.__moves++; })`)
	js(`document.querySelector("#p2").click()`)
	settle(t, b)
	if len(sent) != 1 || sent[0]["event"] != "__nav" || sent[0]["url"] != "/list?page=2" {
		t.Fatalf("the link sent %v", sent)
	}
	deliver(map[string]any{"ref": sent[0]["ref"], "patch": "/list?page=2"}, doc("List", links+`<p id="page">2</p>`))
	if got := js(`location.pathname + location.search + " " + document.querySelector("#page").textContent`); got != "/list?page=2 2" {
		t.Fatalf("after the patch: %s", got)
	}

	sent = nil
	js(`document.querySelector("#other").click()`)
	settle(t, b)
	if len(sent) != 1 || sent[0]["url"] != "/count/bo" {
		t.Fatalf("the link sent %v", sent)
	}
	deliver(map[string]any{"ref": sent[0]["ref"], "nav": "/count/bo", "live": "/count/bo/_live", "resume": "tok2"}, doc("Bo", links+`<p id="n">10</p>`))
	if got := js(`location.pathname + " " + document.title + " " + document.body.getAttribute("data-nx-live") + " " + document.querySelector("#n").textContent`); got != "/count/bo Bo /count/bo/_live 10" {
		t.Fatalf("after navigating: %s", got)
	}
	if len(urls) != 1 {
		t.Fatalf("navigating opened another socket: %v", urls)
	}
	if got := js(`String(window.__moves)`); got != "2" {
		t.Fatalf("nx:navigate fired %s times", got)
	}

	sent = nil
	js(`history.back()`)
	settle(t, b)
	if len(sent) != 1 || sent[0]["url"] != "/list?page=2" {
		t.Fatalf("back sent %v", sent)
	}
	deliver(map[string]any{"ref": sent[0]["ref"], "nav": "/list?page=2", "live": "/list/_live"}, doc("List", links+`<p id="page">2</p>`))
	if got := js(`history.length + " " + location.pathname`); got != "3 /list" {
		t.Fatalf("back pushed an entry: %s", got)
	}

	js(`document.querySelector("#out").click()`)
	settle(t, b)
	deliver(map[string]any{"redirect": "/static"}, "")
	if len(sent) == 0 || sent[len(sent)-1]["url"] != "/static" {
		t.Fatalf("the link sent %v", sent)
	}
	if len(loaded) != 1 || loaded[0] != "http://app.test/static" {
		t.Fatalf("a redirect loaded %v", loaded)
	}
}
