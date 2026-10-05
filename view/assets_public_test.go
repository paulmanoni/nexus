package view_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/view"
)

// A signed-out visitor — on a sign-in page — gets the view runtime and the
// app's assets while auth denies everything else by default; a gated asset
// stays gated.
func TestAssetsArePublic(t *testing.T) {
	files := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "body{}") })
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.UseUsers(authtest.NewUsers)}),
		view.Assets("/assets/", files),
		view.Assets("/private/", files, auth.Required()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	for path, want := range map[string]int{
		"/_view/runtime.js":    200,
		"/_view/import.js":     200,
		"/assets/css/app.css":  200,
		"/private/css/app.css": 401,
	} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}
