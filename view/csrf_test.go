package view_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/view"
)

func signupPage() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<form method="post" action="/signup">`)
		if err := view.CSRF().Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</form>`)
		return err
	})
}

// A form posted over plain HTTP gets through CSRF with the field view.CSRF
// renders — and not without it.
func TestCSRFField(t *testing.T) {
	on := true
	app, stop, err := nexus.InProcess(config.Runtime{Middleware: config.Middleware{Security: &config.Security{CSRF: &on}}},
		view.Page("GET", "/signup", signupPage),
		nexus.AsRest("POST", "/signup", func(c *httpx.Ctx) { c.String(200, "signed up") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	srv := httptest.NewServer(app)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	res, err := client.Get(srv.URL + "/signup")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)">`).FindSubmatch(page)
	if m == nil {
		t.Fatalf("no CSRF field in %s", page)
	}

	post := func(form url.Values) int {
		res, err := client.Post(srv.URL+"/signup", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(url.Values{"name": {"ada"}}); code != http.StatusForbidden {
		t.Errorf("without the field: %d, want 403", code)
	}
	if code := post(url.Values{"name": {"ada"}, "csrf_token": {string(m[1])}}); code != 200 {
		t.Errorf("with it: %d, want 200", code)
	}

	// No CSRF, no field.
	app2, stop2, err := nexus.InProcess(config.Runtime{}, view.Page("GET", "/signup", signupPage))
	if err != nil {
		t.Fatal(err)
	}
	defer stop2(context.Background())
	w := httptest.NewRecorder()
	app2.ServeHTTP(w, httptest.NewRequest("GET", "/signup", nil))
	if strings.Contains(w.Body.String(), "csrf_token") {
		t.Errorf("a field with CSRF off: %s", w.Body.String())
	}
}
