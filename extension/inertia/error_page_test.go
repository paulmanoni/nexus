package inertia_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

func failingPages() nexus.Option {
	boom := func(p nexus.Params[struct{}]) (any, error) {
		return nil, errors.New("qualifications could not be loaded")
	}
	missing := func(p nexus.Params[struct{}]) (any, error) {
		return nil, fmt.Errorf("user 9: %w", nexus.ErrCRUDNotFound)
	}
	moved := func(p nexus.Params[struct{}]) (any, error) {
		return nil, inertia.Redirect("/elsewhere")
	}
	type listProps struct {
		Items inertia.Prop `json:"items"`
	}
	deferred := func(p nexus.Params[struct{}]) (listProps, error) {
		return listProps{Items: inertia.Defer(func() ([]string, error) {
			return nil, errors.New("items could not be loaded")
		})}, nil
	}
	return nexus.Options(
		inertia.Page("GET", "/deferred", "List", deferred, nexus.Public()),
		inertia.Page("GET,POST", "/boom", "Boom", boom, nexus.Public()),
		inertia.Page("GET", "/missing", "Missing", missing, nexus.Public()),
		inertia.Page("GET", "/moved", "Moved", moved, nexus.Public()),
	)
}

func withErrorPage(c *inertia.Config) { c.ErrorPage = "Error" }

type errorPageObject struct {
	Component string         `json:"component"`
	Props     map[string]any `json:"props"`
	URL       string         `json:"url"`
}

// An Inertia visit to a failing page gets the app's error page — a valid
// Inertia response with the error's status — instead of plain JSON.
func TestErrorPage_XHRVisit(t *testing.T) {
	addr := "127.0.0.1:8861"
	bootInertiaWith(t, addr, withErrorPage, failingPages())

	res, body := req(t, addr, "/boom", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", res.StatusCode, body)
	}
	if res.Header.Get("X-Inertia") != "true" {
		t.Fatal("error page must carry X-Inertia so the client renders it")
	}
	var page errorPageObject
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("not a page object: %v\n%s", err, body)
	}
	if page.Component != "Error" || page.URL != "/boom" {
		t.Fatalf("page = %+v", page)
	}
	if page.Props["status"] != float64(500) || page.Props["message"] != "qualifications could not be loaded" {
		t.Fatalf("props = %v", page.Props)
	}
	if page.Props["csrf"] != "tok-123" {
		t.Fatalf("shared props missing from the error page: %v", page.Props)
	}

	res, body = req(t, addr, "/missing", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body, `"status":404`) {
		t.Fatalf("CRUD not-found: status=%d body=%s", res.StatusCode, body)
	}

	// A failing Defer prop, fetched by the client's partial reload.
	res, body = req(t, addr, "/deferred", map[string]string{
		"X-Inertia": "true", "X-Inertia-Partial-Component": "List", "X-Inertia-Partial-Data": "items",
	})
	if res.StatusCode != http.StatusInternalServerError || res.Header.Get("X-Inertia") != "true" ||
		!strings.Contains(body, `"component":"Error"`) || !strings.Contains(body, "items could not be loaded") {
		t.Fatalf("deferred prop failure: status=%d body=%s", res.StatusCode, body)
	}

	res, _ = req(t, addr, "/moved", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound {
		t.Fatalf("an inertia.Redirect must still redirect, got %d", res.StatusCode)
	}
}

// A full browser load renders the error page into the document.
func TestErrorPage_FullLoad(t *testing.T) {
	addr := "127.0.0.1:8862"
	bootInertiaWith(t, addr, withErrorPage, failingPages())

	res, body := req(t, addr, "/boom", nil)
	if res.StatusCode != http.StatusInternalServerError || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status=%d ct=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	m := regexp.MustCompile(`data-page="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-page in document:\n%s", body)
	}
	var page errorPageObject
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &page); err != nil {
		t.Fatal(err)
	}
	if page.Component != "Error" || page.Props["status"] != float64(500) {
		t.Fatalf("page = %+v", page)
	}
}

// A failing form submit goes back with the message under errors._global
// rather than rendering a page at the POST URL.
func TestErrorPage_FormSubmitFlashesGlobal(t *testing.T) {
	addr := "127.0.0.1:8863"
	bootInertiaWith(t, addr, withErrorPage, failingPages())

	res, _ := doReq(t, "POST", addr, "/boom", map[string]string{"X-Inertia": "true", "Referer": "/form"})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/form" {
		t.Fatalf("status=%d location=%q, want 303 to /form", res.StatusCode, res.Header.Get("Location"))
	}
	var flash string
	for _, ck := range res.Cookies() {
		if ck.Name == "nexus_inertia_errors" {
			flash = ck.Value
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(flash)
	if err != nil || !strings.Contains(string(raw), `"_global":"qualifications could not be loaded"`) {
		t.Fatalf("flash = %q (%v)", raw, err)
	}
}

// Without ErrorPage the error keeps the plain JSON response.
func TestErrorPage_OffByDefault(t *testing.T) {
	addr := "127.0.0.1:8864"
	bootInertia(t, addr, failingPages())

	res, body := req(t, addr, "/boom", map[string]string{"X-Inertia": "true"})
	if res.StatusCode != http.StatusInternalServerError || res.Header.Get("X-Inertia") != "" ||
		!strings.Contains(body, `"error":"qualifications could not be loaded"`) {
		t.Fatalf("status=%d x-inertia=%q body=%s", res.StatusCode, res.Header.Get("X-Inertia"), body)
	}
}
