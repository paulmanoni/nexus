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
	"github.com/paulmanoni/nexus/httpx"
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

// scrollProps exercises the infinite-scroll merge variants: a shallow Merge
// with a match key and a DeepMerge with a match key.
type scrollProps struct {
	Rows inertia.Prop `json:"rows"`
	Tree inertia.Prop `json:"tree"`
}

func NewScroll(ctx context.Context) (scrollProps, error) {
	return scrollProps{
		Rows: inertia.Merge(func() ([]int, error) { return []int{1}, nil }, "id"),
		Tree: inertia.DeepMerge(func() (map[string]any, error) { return map[string]any{"a": 1}, nil }, "key"),
	}, nil
}

// groupedProps exercises deferred prop GROUPS: two Defer props in distinct
// named groups (the client fetches each in parallel) plus one default-group prop.
type groupedProps struct {
	Report  inertia.Prop `json:"report"`
	Sidebar inertia.Prop `json:"sidebar"`
	Extra   inertia.Prop `json:"extra"`
}

func NewGrouped(ctx context.Context) (groupedProps, error) {
	return groupedProps{
		Report:  inertia.Defer(func() (string, error) { return "r", nil }, "report"),
		Sidebar: inertia.Defer(func() (string, error) { return "s", nil }, "sidebar"),
		Extra:   inertia.Defer(func() (string, error) { return "e", nil }), // default group
	}, nil
}

// historyProps + NewHistory exercise the per-response history controls; the
// handler takes *httpx.Ctx so it can flag encryption / clearing.
type historyProps struct {
	OK bool `json:"ok"`
}

func NewHistory(c *httpx.Ctx, p nexus.Params[struct{}]) (historyProps, error) {
	inertia.EncryptHistory(c)
	inertia.ClearHistory(c)
	return historyProps{OK: true}, nil
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

// regProps + NewRegister exercise the validation flow: GET renders the form,
// POST returns inertia.Invalid to trigger the redirect-back + errors flash.
type regProps struct {
	Title string `json:"title"`
}

func NewRegister(p nexus.Params[struct{}]) (any, error) {
	if p.Method == http.MethodGet {
		return regProps{Title: "Register"}, nil
	}
	return nil, inertia.Invalid(map[string]string{"email": "Email is required"})
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
		inertia.Module(inertia.Config{Frontend: fsys, Root: "dist", Head: inertia.Head{
			Title: "NXHEAD",
			Links: []inertia.Link{{Rel: "stylesheet", Href: "/x.css"}},
		}}),
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
	if !strings.Contains(body, `<title>NXHEAD</title>`) || !strings.Contains(body, `<link rel="stylesheet" href="/x.css">`) {
		t.Fatalf("Config.Head not injected into the shell: %s", body)
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

// TestPartialExcept asserts X-Inertia-Partial-Except: on a partial reload every
// plain prop is sent EXCEPT those listed, Always props survive, and Except wins
// over Data when a key appears in both.
func TestPartialExcept(t *testing.T) {
	addr := "127.0.0.1:8821"
	bootInertia(t, addr)

	// Pure-except partial: no Partial-Data, so all plain props are sent minus
	// the excepted "title". Shared "csrf" is plain → present; Always "menu" → present.
	res, body := req(t, addr, "/widgets", map[string]string{
		"X-Inertia":                   "true",
		"X-Inertia-Partial-Component": "Widgets/Index",
		"X-Inertia-Partial-Except":    "title",
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
	if _, present := page.Props["title"]; present {
		t.Fatalf("excepted prop must be absent: %v", page.Props)
	}
	if page.Props["csrf"] == nil {
		t.Fatalf("non-excepted plain (shared) prop must be present: %v", page.Props)
	}
	if page.Props["menu"] == nil {
		t.Fatalf("Always prop must ignore Except: %v", page.Props)
	}

	// Except takes precedence over Data: title in both → excluded.
	_, body2 := req(t, addr, "/widgets", map[string]string{
		"X-Inertia":                   "true",
		"X-Inertia-Partial-Component": "Widgets/Index",
		"X-Inertia-Partial-Data":      "title",
		"X-Inertia-Partial-Except":    "title",
	})
	var page2 struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal([]byte(body2), &page2); err != nil {
		t.Fatal(err)
	}
	if _, present := page2.Props["title"]; present {
		t.Fatalf("Except must win over Data: %v", page2.Props)
	}
}

// TestResetMergeProp asserts X-Inertia-Reset: a Merge prop named in the header
// is still sent but NOT flagged in mergeProps, so the client replaces it.
func TestResetMergeProp(t *testing.T) {
	addr := "127.0.0.1:8822"
	bootInertia(t, addr, inertia.Page("GET", "/feed", "Feed/Index", NewFeed))

	// Baseline: a full visit flags "items" as a merge prop.
	_, body := req(t, addr, "/feed", map[string]string{"X-Inertia": "true"})
	var page struct {
		Props      map[string]any `json:"props"`
		MergeProps []string       `json:"mergeProps"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.MergeProps) != 1 || page.MergeProps[0] != "items" {
		t.Fatalf("baseline mergeProps should be [items], got %v", page.MergeProps)
	}

	// With X-Inertia-Reset: items, the value is still sent but no longer flagged.
	_, body2 := req(t, addr, "/feed", map[string]string{
		"X-Inertia":       "true",
		"X-Inertia-Reset": "items",
	})
	var page2 struct {
		Props      map[string]any `json:"props"`
		MergeProps []string       `json:"mergeProps"`
	}
	if err := json.Unmarshal([]byte(body2), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.MergeProps) != 0 {
		t.Fatalf("reset prop must not be flagged as merge, got %v", page2.MergeProps)
	}
	if page2.Props["items"] == nil {
		t.Fatalf("reset prop value must still be sent, got %v", page2.Props)
	}
}

// TestMergeVariants asserts the infinite-scroll merge flags: Merge → mergeProps,
// DeepMerge → deepMergeProps, and each matchOn key → matchPropsOn as "<prop>.<field>".
func TestMergeVariants(t *testing.T) {
	addr := "127.0.0.1:8831"
	bootInertia(t, addr, inertia.Page("GET", "/scroll", "Scroll/Index", NewScroll))

	_, body := req(t, addr, "/scroll", map[string]string{"X-Inertia": "true"})
	var page struct {
		MergeProps     []string `json:"mergeProps"`
		DeepMergeProps []string `json:"deepMergeProps"`
		MatchPropsOn   []string `json:"matchPropsOn"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.MergeProps) != 1 || page.MergeProps[0] != "rows" {
		t.Fatalf("mergeProps=%v want [rows]", page.MergeProps)
	}
	if len(page.DeepMergeProps) != 1 || page.DeepMergeProps[0] != "tree" {
		t.Fatalf("deepMergeProps=%v want [tree]", page.DeepMergeProps)
	}
	want := map[string]bool{"rows.id": true, "tree.key": true}
	if len(page.MatchPropsOn) != 2 || !want[page.MatchPropsOn[0]] || !want[page.MatchPropsOn[1]] {
		t.Fatalf("matchPropsOn=%v want rows.id + tree.key", page.MatchPropsOn)
	}

	// X-Inertia-Reset clears both merge flavors and their match keys.
	_, body2 := req(t, addr, "/scroll", map[string]string{
		"X-Inertia":       "true",
		"X-Inertia-Reset": "rows,tree",
	})
	var page2 struct {
		MergeProps     []string `json:"mergeProps"`
		DeepMergeProps []string `json:"deepMergeProps"`
		MatchPropsOn   []string `json:"matchPropsOn"`
	}
	if err := json.Unmarshal([]byte(body2), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.MergeProps) != 0 || len(page2.DeepMergeProps) != 0 || len(page2.MatchPropsOn) != 0 {
		t.Fatalf("reset should clear all merge flags, got merge=%v deep=%v match=%v",
			page2.MergeProps, page2.DeepMergeProps, page2.MatchPropsOn)
	}
}

// TestShellVary asserts the initial (non-XHR) HTML load sets Vary: X-Inertia so
// shared caches differentiate it from the XHR JSON of the same URL.
func TestShellVary(t *testing.T) {
	addr := "127.0.0.1:8823"
	bootInertia(t, addr)

	res, _ := req(t, addr, "/widgets", nil) // full load, no X-Inertia
	if v := res.Header.Get("Vary"); !strings.Contains(v, "X-Inertia") {
		t.Fatalf("HTML shell must Vary on X-Inertia, got %q", v)
	}
}

// TestErrorsPropAlwaysPresent asserts every render carries an `errors` object
// (empty by default) so the client's useForm can read page.props.errors
// unconditionally.
func TestErrorsPropAlwaysPresent(t *testing.T) {
	addr := "127.0.0.1:8824"
	bootInertia(t, addr)

	_, body := req(t, addr, "/widgets", map[string]string{"X-Inertia": "true"})
	var page struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	errs, ok := page.Props["errors"]
	if !ok {
		t.Fatalf("errors prop must always be present: %v", page.Props)
	}
	if m, isMap := errs.(map[string]any); !isMap || len(m) != 0 {
		t.Fatalf("errors should default to {}, got %#v", errs)
	}
}

// TestValidationFlow is the end-to-end useForm contract: a failed submit
// returns 303 back to the form with a flash cookie, and following that redirect
// surfaces the messages in page.props.errors (then clears the cookie).
func TestValidationFlow(t *testing.T) {
	addr := "127.0.0.1:8825"
	bootInertia(t, addr, inertia.Page("GET,POST", "/register", "Register", NewRegister, nexus.Public()))

	// Failed submit → 303 back + flash cookie.
	res, _ := doReq(t, "POST", addr, "/register", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("validation failure should 303, got %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/register" {
		t.Fatalf("should redirect back to /register, got %q", loc)
	}
	var flash string
	for _, ck := range res.Cookies() {
		if ck.Name == "nexus_inertia_errors" {
			flash = ck.Value
		}
	}
	if flash == "" {
		t.Fatal("expected a flash cookie carrying the errors")
	}

	// Follow the redirect with the flash cookie → errors appear in props.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	r, _ := http.NewRequest("GET", "http://"+addr+"/register", nil)
	r.Header.Set("X-Inertia", "true")
	r.AddCookie(&http.Cookie{Name: "nexus_inertia_errors", Value: flash})
	res2, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	b, _ := io.ReadAll(res2.Body)
	var page struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	errs, _ := page.Props["errors"].(map[string]any)
	if errs["email"] != "Email is required" {
		t.Fatalf("flashed error must surface in props.errors, got %#v", page.Props["errors"])
	}
	// The re-render clears the one-shot cookie.
	cleared := false
	for _, ck := range res2.Cookies() {
		if ck.Name == "nexus_inertia_errors" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the one-shot errors cookie must be cleared after consumption")
	}
}

// TestInvalidField asserts the single-field helper flashes one field error
// through the same redirect-back flow.
func TestInvalidField(t *testing.T) {
	addr := "127.0.0.1:8827"
	newOne := func(p nexus.Params[struct{}]) (any, error) {
		if p.Method == http.MethodGet {
			return regProps{Title: "One"}, nil
		}
		return nil, inertia.InvalidField("name", "Name is required")
	}
	bootInertia(t, addr, inertia.Page("GET,POST", "/one", "One", newOne, nexus.Public()))

	res, _ := doReq(t, "POST", addr, "/one", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("InvalidField should 303, got %d", res.StatusCode)
	}
	var flash string
	for _, ck := range res.Cookies() {
		if ck.Name == "nexus_inertia_errors" {
			flash = ck.Value
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	r, _ := http.NewRequest("GET", "http://"+addr+"/one", nil)
	r.Header.Set("X-Inertia", "true")
	r.AddCookie(&http.Cookie{Name: "nexus_inertia_errors", Value: flash})
	res2, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	b, _ := io.ReadAll(res2.Body)
	var page struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	errs, _ := page.Props["errors"].(map[string]any)
	if errs["name"] != "Name is required" {
		t.Fatalf("InvalidField message must surface, got %#v", page.Props["errors"])
	}
}

// TestValidationErrorBag asserts X-Inertia-Error-Bag nests the flashed messages
// under the bag name, so a page with multiple forms can scope its errors.
func TestValidationErrorBag(t *testing.T) {
	addr := "127.0.0.1:8826"
	bootInertia(t, addr, inertia.Page("GET,POST", "/register", "Register", NewRegister, nexus.Public()))

	res, _ := doReq(t, "POST", addr, "/register", map[string]string{
		"X-Inertia":           "true",
		"X-Inertia-Error-Bag": "signup",
	})
	var flash string
	for _, ck := range res.Cookies() {
		if ck.Name == "nexus_inertia_errors" {
			flash = ck.Value
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	r, _ := http.NewRequest("GET", "http://"+addr+"/register", nil)
	r.Header.Set("X-Inertia", "true")
	r.AddCookie(&http.Cookie{Name: "nexus_inertia_errors", Value: flash})
	res2, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	b, _ := io.ReadAll(res2.Body)
	var page struct {
		Props map[string]any `json:"props"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	errs, _ := page.Props["errors"].(map[string]any)
	bag, _ := errs["signup"].(map[string]any)
	if bag["email"] != "Email is required" {
		t.Fatalf("errors should nest under the bag, got %#v", page.Props["errors"])
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

// TestDevTagsPreferViteServer asserts that when NEXUS_VITE_DEV is set, the
// document shell references the Vite dev server for HMR and ignores the build
// manifest (whose hashed assets go stale in dev).
func TestDevTagsPreferViteServer(t *testing.T) {
	t.Setenv("NEXUS_VITE_DEV", "http://localhost:5173")
	addr := "127.0.0.1:8818"
	bootInertia(t, addr) // bootInertia supplies a build manifest

	_, body := req(t, addr, "/widgets", nil) // full (non-XHR) visit
	if !strings.Contains(body, "http://localhost:5173/@vite/client") {
		t.Fatalf("dev shell should reference the vite dev server, got: %s", body)
	}
	if strings.Contains(body, "/assets/main-abc123.js") {
		t.Fatalf("dev shell must ignore the build manifest, got: %s", body)
	}
	if !strings.Contains(body, "/__nexus/dev/script.js") {
		t.Fatalf("dev shell should include the live-reload script, got: %s", body)
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

// TestDeferredGroups asserts Defer's group argument partitions deferredProps so
// the client can fetch each group in a parallel request.
func TestDeferredGroups(t *testing.T) {
	addr := "127.0.0.1:8828"
	bootInertia(t, addr, inertia.Page("GET", "/grouped", "Grouped/Index", NewGrouped))

	_, body := req(t, addr, "/grouped", map[string]string{"X-Inertia": "true"})
	var page struct {
		Props         map[string]any      `json:"props"`
		DeferredProps map[string][]string `json:"deferredProps"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if got := page.DeferredProps["report"]; len(got) != 1 || got[0] != "report" {
		t.Fatalf("deferredProps[report]=%v want [report]", got)
	}
	if got := page.DeferredProps["sidebar"]; len(got) != 1 || got[0] != "sidebar" {
		t.Fatalf("deferredProps[sidebar]=%v want [sidebar]", got)
	}
	if got := page.DeferredProps["default"]; len(got) != 1 || got[0] != "extra" {
		t.Fatalf("deferredProps[default]=%v want [extra]", got)
	}
	// All deferred props are excluded from the initial payload.
	for _, k := range []string{"report", "sidebar", "extra"} {
		if _, present := page.Props[k]; present {
			t.Fatalf("deferred prop %q must be absent on the full visit: %v", k, page.Props)
		}
	}
}

// TestHistoryEncryption asserts the per-response controls: a handler that calls
// EncryptHistory/ClearHistory produces a page object with both flags set.
func TestHistoryEncryption(t *testing.T) {
	addr := "127.0.0.1:8829"
	bootInertia(t, addr, inertia.Page("GET", "/secure", "Secure", NewHistory))

	_, body := req(t, addr, "/secure", map[string]string{"X-Inertia": "true"})
	var page struct {
		EncryptHistory bool `json:"encryptHistory"`
		ClearHistory   bool `json:"clearHistory"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if !page.EncryptHistory {
		t.Fatalf("EncryptHistory(c) should set encryptHistory=true: %s", body)
	}
	if !page.ClearHistory {
		t.Fatalf("ClearHistory(c) should set clearHistory=true: %s", body)
	}

	// A page that doesn't touch the controls (and an app with no default)
	// emits neither flag.
	_, body2 := req(t, addr, "/widgets", map[string]string{"X-Inertia": "true"})
	if strings.Contains(body2, "encryptHistory") || strings.Contains(body2, "clearHistory") {
		t.Fatalf("untouched page must omit history flags: %s", body2)
	}
}

// TestHistoryEncryptDefault asserts Config.EncryptHistory turns encryption on
// app-wide, with no per-handler call.
func TestHistoryEncryptDefault(t *testing.T) {
	addr := "127.0.0.1:8830"
	fsys := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
	ready := make(chan struct{})
	go func() {
		nexus.Run(nexus.Config{Server: nexus.ServerConfig{Addr: addr}, TraceCapacity: 10},
			inertia.Module(inertia.Config{Frontend: fsys, Root: "dist", EncryptHistory: true}),
			inertia.Page("GET", "/home", "Home", NewWidgets),
			nexus.Invoke(func() { close(ready) }),
		)
	}()
	<-ready
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/__nexus/config"); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	_, body := req(t, addr, "/home", map[string]string{"X-Inertia": "true"})
	var page struct {
		EncryptHistory bool `json:"encryptHistory"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if !page.EncryptHistory {
		t.Fatalf("Config.EncryptHistory should default encryptHistory=true: %s", body)
	}
}
