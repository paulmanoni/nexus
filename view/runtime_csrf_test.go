package view

import (
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/view/internal/browser"
)

// A plain POST form on a view page carries the CSRF token as csrf_token
// when the app's CSRF middleware set its XSRF-TOKEN cookie.
func TestPlainFormCarriesCSRFToken(t *testing.T) {
	cases := []struct {
		name, cookie, form, want, absent string
	}{
		{"same-origin post", "csrftoken=abc; XSRF-TOKEN=abc",
			`<form method="post" action="/save"><input name="x" value="1"><button id="go">Go</button></form>`,
			"csrf_token=abc", ""},
		{"no token cookie", "",
			`<form method="post" action="/save"><input name="x" value="1"><button id="go">Go</button></form>`,
			"x=1", "csrf_token"},
		{"get form", "XSRF-TOKEN=abc",
			`<form action="/find"><input name="q" value="a"><button id="go">Go</button></form>`,
			"q=a", "csrf_token"},
		{"another origin", "XSRF-TOKEN=abc",
			`<form method="post" action="https://elsewhere.test/save"><button id="go">Go</button></form>`,
			"", "csrf_token"},
		{"the form's own field wins", "XSRF-TOKEN=abc",
			`<form method="post" action="/save"><input type="hidden" name="csrf_token" value="mine"><button id="go">Go</button></form>`,
			"csrf_token=mine", "csrf_token=abc"},
		{"button posts", "XSRF-TOKEN=abc",
			`<form action="/find"><button id="go" formmethod="post">Go</button></form>`,
			"csrf_token=abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			navigated := false
			b, err := browser.New(browser.Host{
				Cookie:   func(string) string { return tc.cookie },
				Navigate: func(method, u, b, ct string) { navigated, body = true, u+" "+b },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Load(`<!DOCTYPE html><html><head></head><body>`+tc.form+`</body></html>`, "http://app.test/p"); err != nil {
				t.Fatal(err)
			}
			if err := b.Run("runtime.js", runtimeJS); err != nil {
				t.Fatal(err)
			}
			if _, err := b.VM.RunString(`var g = document.querySelector("#go"); g.form.requestSubmit(g)`); err != nil {
				t.Fatal(err)
			}
			if err := b.Settle(100, time.Second, nil, nil); err != nil {
				t.Fatal(err)
			}
			if !navigated {
				t.Fatal("the form did not submit")
			}
			if !strings.Contains(body, tc.want) || tc.absent != "" && strings.Contains(body, tc.absent) {
				t.Fatalf("request %q: want %q, not %q", body, tc.want, tc.absent)
			}
		})
	}
}
