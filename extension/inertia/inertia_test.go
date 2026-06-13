package inertia_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

// pageProps exercises all three prop kinds: a plain field, an Optional (lazy)
// field that must be skipped on full visits, and an Always field that survives
// partial reloads it isn't named in.
type pageProps struct {
	Title string       `json:"title"`
	Stats inertia.Prop `json:"stats"`
	Menu  inertia.Prop `json:"menu"`
}

func NewWidgets(ctx context.Context) (pageProps, error) {
	return pageProps{
		Title: "Widgets",
		Stats: inertia.Optional(func() (int, error) { return 42, nil }),
		Menu:  inertia.Always([]string{"home", "about"}),
	}, nil
}

// feedProps exercises Merge (sent + flagged) and Defer (excluded + advertised).
type feedProps struct {
	Items inertia.Prop `json:"items"`
	More  inertia.Prop `json:"more"`
}

func NewFeed(ctx context.Context) (feedProps, error) {
	return feedProps{
		Items: inertia.Merge(func() ([]int, error) { return []int{1, 2}, nil }),
		More:  inertia.Defer(func() (string, error) { return "later", nil }),
	}, nil
}

// authProps + NewAuthForm exercise a single handler mounted for GET+POST that
// branches on nexus.Params.Method.
type authProps struct {
	Mode string `json:"mode"`
}

func NewAuthForm(p nexus.Params[struct{}]) (authProps, error) {
	if p.Method == http.MethodGet {
		return authProps{Mode: "form"}, nil
	}
	return authProps{Mode: "submitted"}, nil
}

func NewSave(ctx context.Context) (any, error) { return nil, inertia.Redirect("/users") }
func NewExternal(ctx context.Context) (any, error) {
	return nil, inertia.Location("https://ext.example/login")
}

const manifestJSON = `{"src/main.ts":{"file":"assets/main-abc123.js","isEntry":true,"css":["assets/main-xyz.css"]}}`

func wantVersion() string {
	sum := sha256.Sum256([]byte(manifestJSON))
	return hex.EncodeToString(sum[:])[:16]
}

func bootInertia(t *testing.T, addr string, extra ...nexus.Option) {
	t.Helper()
	fsys := fstest.MapFS{
		"dist/.vite/manifest.json": {Data: []byte(manifestJSON)},
	}
	ready := make(chan struct{})
	opts := append([]nexus.Option{
		inertia.Module(inertia.Config{Frontend: fsys, Root: "dist"}),
		inertia.Share(func(ctx context.Context) (string, any) { return "csrf", "tok-123" }),
		inertia.Page("GET", "/widgets", "Widgets/Index", NewWidgets),
		nexus.Invoke(func() { close(ready) }),
	}, extra...)
	go func() {
		nexus.Run(nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 10}, opts...)
	}()
	<-ready
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("inertia app didn't bind HTTP within 3s")
}

func req(t *testing.T, addr, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	return doReq(t, "GET", addr, path, headers)
}

// doReq issues a request without following redirects so redirect statuses can
// be asserted directly.
func doReq(t *testing.T, method, addr, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	r, _ := http.NewRequest(method, "http://"+addr+path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// TestXHRVisit asserts the JSON page object on an X-Inertia request: the
// component, url and (manifest-derived) version, the X-Inertia response
// header, and that Optional props are omitted while plain/Always/shared props
// are present.
func TestXHRVisit(t *testing.T) {
	addr := "127.0.0.1:8810"
	bootInertia(t, addr)

	res, body := req(t, addr, "/widgets?tab=1", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if res.Header.Get("X-Inertia") != "true" {
		t.Fatalf("missing X-Inertia response header; got %q", res.Header.Get("X-Inertia"))
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want JSON content-type, got %q", ct)
	}

	var page struct {
		Component string         `json:"component"`
		Props     map[string]any `json:"props"`
		URL       string         `json:"url"`
		Version   string         `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("bad page JSON: %v — %s", err, body)
	}
	if page.Component != "Widgets/Index" {
		t.Fatalf("component=%q", page.Component)
	}
	if page.URL != "/widgets?tab=1" {
		t.Fatalf("url=%q", page.URL)
	}
	if page.Version != wantVersion() {
		t.Fatalf("version=%q want %q", page.Version, wantVersion())
	}
	if page.Props["title"] != "Widgets" {
		t.Fatalf("plain prop missing: %v", page.Props)
	}
	if page.Props["csrf"] != "tok-123" {
		t.Fatalf("shared prop missing: %v", page.Props)
	}
	if page.Props["menu"] == nil {
		t.Fatalf("Always prop missing: %v", page.Props)
	}
	if _, present := page.Props["stats"]; present {
		t.Fatalf("Optional prop must be absent on a full visit: %v", page.Props)
	}
}

// TestFullVisit asserts the HTML document shell on a non-XHR load: the
// data-page root div and the manifest's hashed asset tags.
func TestFullVisit(t *testing.T) {
	addr := "127.0.0.1:8811"
	bootInertia(t, addr)

	res, body := req(t, addr, "/widgets", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("want HTML content-type, got %q", ct)
	}
	if !strings.Contains(body, `id="app" data-page=`) {
		t.Fatalf("missing data-page root div: %s", body)
	}
	if !strings.Contains(body, "Widgets/Index") {
		t.Fatalf("data-page should embed the component: %s", body)
	}
	if !strings.Contains(body, `src="/assets/main-abc123.js"`) {
		t.Fatalf("missing manifest entry script: %s", body)
	}
	if !strings.Contains(body, `href="/assets/main-xyz.css"`) {
		t.Fatalf("missing manifest css: %s", body)
	}
}

// TestPartialReload asserts that a partial visit returns only the requested
// prop plus Always props, evaluating the Optional thunk on demand.
func TestPartialReload(t *testing.T) {
	addr := "127.0.0.1:8812"
	bootInertia(t, addr)

	res, body := req(t, addr, "/widgets", map[string]string{
		"X-Inertia":                   "true",
		"X-Inertia-Partial-Component": "Widgets/Index",
		"X-Inertia-Partial-Data":      "stats",
	})
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	var page struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if page.Props["stats"] == nil {
		t.Fatalf("requested Optional prop must be present: %v", page.Props)
	}
	if page.Props["menu"] == nil {
		t.Fatalf("Always prop must survive a partial: %v", page.Props)
	}
	if _, present := page.Props["title"]; present {
		t.Fatalf("unrequested plain prop must be absent on a partial: %v", page.Props)
	}
	if _, present := page.Props["csrf"]; present {
		t.Fatalf("unrequested shared prop must be absent on a partial: %v", page.Props)
	}
}

// TestVersionMismatch asserts the asset-version guard: a stale X-Inertia-Version
// on a GET XHR visit gets a 409 + X-Inertia-Location for a forced full reload.
func TestVersionMismatch(t *testing.T) {
	addr := "127.0.0.1:8813"
	bootInertia(t, addr)

	res, _ := req(t, addr, "/widgets", map[string]string{
		"X-Inertia":         "true",
		"X-Inertia-Version": "stale-hash",
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale version should 409, got %d", res.StatusCode)
	}
	if loc := res.Header.Get("X-Inertia-Location"); loc != "/widgets" {
		t.Fatalf("missing/incorrect X-Inertia-Location: %q", loc)
	}

	// Matching version proceeds normally.
	res2, _ := req(t, addr, "/widgets", map[string]string{
		"X-Inertia":         "true",
		"X-Inertia-Version": wantVersion(),
	})
	if res2.StatusCode != 200 {
		t.Fatalf("matching version should pass, got %d", res2.StatusCode)
	}
}

// TestRedirect asserts inertia.Redirect → 303 See Other + Location, and
// inertia.Location (external) → 409 + X-Inertia-Location for an XHR visit.
func TestRedirect(t *testing.T) {
	addr := "127.0.0.1:8814"
	bootInertia(t, addr,
		inertia.Page("POST", "/save", "Save", NewSave),
		inertia.Page("GET", "/ext", "Ext", NewExternal),
	)

	res, _ := doReq(t, "POST", addr, "/save", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("internal redirect should 303, got %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/users" {
		t.Fatalf("Location=%q want /users", loc)
	}

	res2, _ := doReq(t, "GET", addr, "/ext", map[string]string{"X-Inertia": "true"})
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("external redirect on XHR should 409, got %d", res2.StatusCode)
	}
	if loc := res2.Header.Get("X-Inertia-Location"); loc != "https://ext.example/login" {
		t.Fatalf("X-Inertia-Location=%q", loc)
	}
}

// TestPageInsideModule guards the fx-ordering fix: an inertia.Page declared
// inside a nexus.Module (whose route registration runs ahead of root-level
// invokes) must still find the engine. Before the App-pull fix this 500'd with
// "engine not installed".
func TestPageInsideModule(t *testing.T) {
	addr := "127.0.0.1:8817"
	bootInertia(t, addr, nexus.Module("sub", inertia.Page("GET", "/sub", "Sub/Index", NewWidgets)))

	res, body := req(t, addr, "/sub", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != 200 {
		t.Fatalf("module-wrapped page status=%d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, "Sub/Index") {
		t.Fatalf("expected the module page to render, got: %s", body)
	}
}

// TestMultiMethodPage asserts a single inertia.Page registered for "GET,POST"
// serves both verbs through one handler that branches on Params.Method.
func TestMultiMethodPage(t *testing.T) {
	addr := "127.0.0.1:8816"
	bootInertia(t, addr, inertia.Page("GET,POST", "/auth", "Auth", NewAuthForm))

	_, getBody := req(t, addr, "/auth", map[string]string{"X-Inertia": "true"})
	if !strings.Contains(getBody, `"mode":"form"`) {
		t.Fatalf("GET should render the form mode, got: %s", getBody)
	}
	_, postBody := doReq(t, "POST", addr, "/auth", map[string]string{"X-Inertia": "true"})
	if !strings.Contains(postBody, `"mode":"submitted"`) {
		t.Fatalf("POST should hit the submit branch, got: %s", postBody)
	}
}

// TestDeferAndMerge asserts that on a full visit a Merge prop is sent and
// flagged in mergeProps, a Defer prop is excluded but advertised in
// deferredProps, and that a partial reload then delivers the deferred prop.
func TestDeferAndMerge(t *testing.T) {
	addr := "127.0.0.1:8815"
	bootInertia(t, addr, inertia.Page("GET", "/feed", "Feed/Index", NewFeed))

	_, body := req(t, addr, "/feed", map[string]string{"X-Inertia": "true"})
	var page struct {
		Props         map[string]any      `json:"props"`
		DeferredProps map[string][]string `json:"deferredProps"`
		MergeProps    []string            `json:"mergeProps"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if page.Props["items"] == nil {
		t.Fatalf("Merge prop should be sent on full visit: %v", page.Props)
	}
	if len(page.MergeProps) != 1 || page.MergeProps[0] != "items" {
		t.Fatalf("mergeProps=%v want [items]", page.MergeProps)
	}
	if _, present := page.Props["more"]; present {
		t.Fatalf("Defer prop must be excluded on full visit: %v", page.Props)
	}
	if got := page.DeferredProps["default"]; len(got) != 1 || got[0] != "more" {
		t.Fatalf("deferredProps[default]=%v want [more]", got)
	}

	// The client's follow-up partial fetches the deferred prop.
	_, body2 := req(t, addr, "/feed", map[string]string{
		"X-Inertia":                   "true",
		"X-Inertia-Partial-Component": "Feed/Index",
		"X-Inertia-Partial-Data":      "more",
	})
	var partial struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal([]byte(body2), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Props["more"] != "later" {
		t.Fatalf("deferred prop should resolve on partial request: %v", partial.Props)
	}
}
