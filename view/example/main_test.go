package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
)

func boot(t *testing.T) http.Handler {
	t.Helper()
	h, stop, err := nexus.InProcess(nexus.Config{}, app()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return h
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

type meta struct {
	Shard  string            `json:"shard"`
	Path   string            `json:"path"`
	Args   []json.RawMessage `json:"args"`
	Reads  []json.RawMessage `json:"reads"`
	States []string          `json:"states"`
}

var metaRE = regexp.MustCompile(`data-nx-shard-meta="([^"]*)"`)

func metas(t *testing.T, page string) map[string]meta {
	t.Helper()
	out := map[string]meta{}
	for _, m := range metaRE.FindAllStringSubmatch(page, -1) {
		var v meta
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &v); err != nil {
			t.Fatal(err)
		}
		name := v.Shard[:strings.IndexByte(v.Shard, '-')]
		out[name[strings.IndexByte(name, '.')+1:]] = v
	}
	return out
}

// rerender asks the server for a shard the way the browser does.
func rerender(t *testing.T, h http.Handler, m meta, args []any, states map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"path": m.Path, "args": args, "states": states})
	code, out := do(t, h, "POST", "/_view/shard/"+m.Shard, string(body))
	if code != 200 {
		t.Fatalf("shard %s = %d: %s", m.Shard, code, out)
	}
	return out
}

func TestPage(t *testing.T) {
	h := boot(t)
	code, page := do(t, h, "GET", "/", "")
	if code != 200 {
		t.Fatalf("GET / = %d: %s", code, page)
	}
	for _, want := range []string{
		`<b id="count"><nx-t data-nx-bind-text=`, // { count.Get() } is live text
		`>0</nx-t></b> times`,
		`data-nx-on-click=`,
		`<nx-if data-nx-bind-show=`, // if count.Get() >= 5 renders both branches
		`<p id="five">High five!</p>`,
		`<p id="more" class="text-muted-foreground"><nx-t`,
		`<p id="details" hidden data-nx-bind-hidden=`,
		`value="" data-nx-bind-value=`,
		`<small id="summary" hidden data-nx-bind-hidden=`, // SearchSummary reads the shared Query
		`data-nx-on-input=`,
		`<nx-shard><ul id="pets">`,
		`<li>Biscuit <small>dog</small></li>`,
		`<nx-shard><section class="rounded-lg border p-4 space-y-3"><h2 class="text-lg font-semibold">All pets</h2>`,
		`/templui/js/dialog.min.js`, // templUI's script, loaded by dialog.Script()
		`page <nx-t`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	// templUI's Button spreads Props.Attributes: the compiled action and the
	// live disabled binding land on its <button>, disabled at count 0.
	if !regexp.MustCompile(`<button id="reset"[^>]*data-nx-bind-disabled="[^"]*"[^>]*data-nx-on-click="[^"]*"[^>]* disabled>`).MatchString(page) {
		t.Error("the reset button lacks its compiled attributes")
	}
	if strings.Contains(page, "not compiled") {
		t.Error("an action reached the page uncompiled")
	}
	if strings.Index(page, `id="five"`) > strings.Index(page, `id="more"`) ||
		!regexp.MustCompile(`<nx-if data-nx-bind-show="[^"]*" hidden><p id="five">`).MatchString(page) {
		t.Error("with count 0, the High five branch is rendered hidden")
	}

	ms := metas(t, page)
	pets, paged := ms["PetResults"], ms["Paged"]
	if pets.Shard == "" || paged.Shard == "" {
		t.Fatalf("shard metadata: %+v", ms)
	}
	if len(pets.Args) != 0 || len(pets.Reads) != 1 || len(pets.States) != 1 || len(paged.States) != 1 || len(paged.Reads) != 1 {
		t.Fatalf("PetResults %+v / Paged %+v", pets, paged)
	}

	// PetResults reads the shared Search.Query: the browser sends its value back.
	query := pets.States[0]
	if !strings.HasPrefix(query, "g") || strings.Count(page, `&#34;$sig&#34;:&#34;`+query+`&#34;`) < 3 {
		t.Fatalf("the box, the summary and the results must share one signal %s", query)
	}
	out := rerender(t, h, pets, []any{}, map[string]any{query: "cat"})
	if strings.Contains(out, "<nx-shard>") || !strings.Contains(out, "Mochi") || strings.Contains(out, "Biscuit") ||
		!strings.Contains(out, "data-nx-shard-meta") {
		t.Fatalf("PetResults for cat:\n%s", out)
	}

	// Paged owns its page signal: the re-render starts from the browser's
	// value, and the signal keeps its id.
	out = rerender(t, h, paged, []any{}, map[string]any{paged.States[0]: 2})
	if !strings.Contains(out, "<li>Kiwi</li>") || strings.Contains(out, "<li>Biscuit</li>") {
		t.Fatalf("Paged on page 2:\n%s", out)
	}
	again := metas(t, out)["Paged"]
	if again.Path != paged.Path || len(again.States) != 1 || again.States[0] != paged.States[0] {
		t.Fatalf("the re-rendered shard must keep its path and signal ids: %+v vs %+v", again, paged)
	}
	out = rerender(t, h, paged, []any{}, map[string]any{paged.States[0]: 99})
	if !strings.Contains(out, "<li>Luna</li>") {
		t.Fatalf("an out-of-range page from the browser is clamped:\n%s", out)
	}

	if code, _ := do(t, h, "POST", "/_view/shard/"+pets.Shard, `{"args":[1]}`); code != 400 {
		t.Fatalf("wrong arguments = %d, want 400", code)
	}
	if code, js := do(t, h, "GET", "/templui/js/dialog.min.js", ""); code != 200 || len(js) < 100 {
		t.Fatalf("templUI's dialog script = %d", code)
	}
	code, js := do(t, h, "GET", "/_view/twins.js", "")
	if code != 200 || !strings.Contains(js, `c.count.set((c.count.get() + 1))`) || !strings.Contains(js, `c.q.set(e.target.value)`) {
		t.Fatalf("twins.js = %d:\n%s", code, js)
	}
}
